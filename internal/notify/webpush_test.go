package notify

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json/v2"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// RFC 8291, appendix A.
func TestEncryptRFC8291(t *testing.T) {
	d := func(s string) []byte {
		b, err := b64.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	as, err := ecdh.P256().NewPrivateKey(d("yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	if got := b64.EncodeToString(as.PublicKey().Bytes()); got != "BP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A8" {
		t.Fatalf("as_public %s", got)
	}
	body, err := encrypt([]byte("When I grow up, I want to be a watermelon"), d("BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4"),
		d("BTBZMqHH6r4Tts7J_aSIgg"), as, d("DGv6ra1nlYgDCS1FRnbzlw"))
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if got := b64.EncodeToString(body); got != want {
		t.Fatalf("body\n got %s\nwant %s", got, want)
	}
	ua, _ := ecdh.P256().NewPrivateKey(d("q1dXpw3UpT5VOmu_cf_v6ih07Aems3njxI-JWgLcM94"))
	if plain, err := decrypt(body, ua, d("BTBZMqHH6r4Tts7J_aSIgg")); err != nil || string(plain) != "When I grow up, I want to be a watermelon" {
		t.Fatalf("decrypt: %q %v", plain, err)
	}
}

// decrypt is the browser's side of RFC 8291.
func decrypt(body []byte, ua *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	salt, rs, idlen := body[:16], binary.BigEndian.Uint32(body[16:20]), int(body[20])
	_ = rs
	asPub, _ := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	shared, err := ua.ECDH(asPub)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), asPub.Bytes()...)
	ikm, _ := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	return plain[:len(plain)-1], nil // the 0x02 delimiter
}

// A fake push service checks the VAPID JWT and decrypts what Send posted, like a browser would.
func TestSendToPushService(t *testing.T) {
	keys, err := NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseKeys(keys.Private())
	if err != nil || again.Public != keys.Public {
		t.Fatalf("keys don't round-trip: %v", err)
	}
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gone" {
			w.WriteHeader(410)
			return
		}
		if r.Header.Get("Content-Encoding") != "aes128gcm" || r.Header.Get("TTL") != "60" || r.Header.Get("Urgency") != "high" {
			t.Errorf("headers: %v", r.Header)
		}
		if err := checkVAPID(r.Header.Get("Authorization"), "http://"+r.Host, keys.Public); err != nil {
			t.Error(err)
			w.WriteHeader(403)
			return
		}
		body, _ := io.ReadAll(r.Body)
		plain, err := decrypt(body, ua, auth)
		if err != nil {
			t.Error(err)
		}
		got <- string(plain)
		w.WriteHeader(201)
	}))
	defer srv.Close()
	sub := Sub{Endpoint: srv.URL + "/push/abc", P256dh: b64.EncodeToString(ua.PublicKey().Bytes()), Auth: b64.EncodeToString(auth)}
	if code, err := keys.Send(context.Background(), sub, []byte(`{"title":"hi"}`), "mailto:me@x", time.Minute, "high"); err != nil || code != 201 {
		t.Fatalf("send: %d %v", code, err)
	}
	if p := <-got; p != `{"title":"hi"}` {
		t.Fatalf("payload %q", p)
	}
	sub.Endpoint = srv.URL + "/gone"
	if code, err := keys.Send(context.Background(), sub, []byte("x"), "mailto:me@x", time.Minute, "high"); code != 410 || err == nil {
		t.Fatalf("gone: %d %v", code, err)
	}
}

func checkVAPID(h, aud, pub string) error {
	t, k, ok := strings.Cut(strings.TrimPrefix(h, "vapid t="), ", k=")
	if !ok || k != pub {
		return fmt.Errorf("authorization %q", h)
	}
	parts := strings.Split(t, ".")
	if len(parts) != 3 {
		return fmt.Errorf("jwt %q", t)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	raw, _ := b64.DecodeString(parts[1])
	if err := json.Unmarshal(raw, &claims); err != nil || claims.Aud != aud || claims.Sub != "mailto:me@x" || claims.Exp < time.Now().Unix() {
		return fmt.Errorf("claims %s", raw)
	}
	kb, _ := b64.DecodeString(k)
	x, y := elliptic.Unmarshal(elliptic.P256(), kb)
	sig, _ := b64.DecodeString(parts[2])
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return fmt.Errorf("bad signature")
	}
	return nil
}

// encrypt never writes into the caller's payload: the notifier shares one payload between devices' goroutines.
func TestEncryptLeavesPayloadAlone(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	buf := []byte("hello\xff\xff\xff")
	payload := buf[:5] // spare capacity, as a shared slice may have
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			as, _ := ecdh.P256().GenerateKey(rand.Reader)
			body, err := encrypt(payload, ua.PublicKey().Bytes(), []byte("0123456789abcdef"), as, make([]byte, 16))
			if err != nil {
				t.Error(err)
				return
			}
			if plain, err := decrypt(body, ua, []byte("0123456789abcdef")); err != nil || string(plain) != "hello" {
				t.Errorf("decrypt: %q %v", plain, err)
			}
		})
	}
	wg.Wait()
	if string(buf) != "hello\xff\xff\xff" {
		t.Fatalf("payload's backing array changed: %q", buf)
	}
}
