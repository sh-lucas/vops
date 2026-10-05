package e2e

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sh-lucas/vops/internal/podman"
)

// Notifications on a real host: deploys raise nothing, a killed container raises a restart, a /health answering 500
// raises a health alert, and a subscribed admin's push service gets them encrypted.
func TestNotifications(t *testing.T) {
	w := setup(t)
	dev := filepath.Join(t.TempDir(), "infra")
	os.MkdirAll(dev, 0o755)
	w.vops(dev, "init", "--domain", "vops.test")
	w.setConfig(dev, map[string]string{"http": w.httpAddr, "https": "off", "ui": w.uiAddr, "tls": "off"})
	out := w.vops(dev, "install", "dev@fakehost", "--binary", w.bin)
	pw := regexp.MustCompile(`dashboard login: admin / (\S+)`).FindStringSubmatch(out)[1]
	w.startDaemon()
	w.hostAPI("POST", "/api/notify/settings", `{"probe_interval": 1, "down_after": 2}`)

	// a fake push service that decrypts like a browser
	ua, _ := ecdh.P256().GenerateKey(rand.Reader)
	auth := make([]byte, 16)
	rand.Read(auth)
	var mu sync.Mutex
	var pushed []string
	push := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		plain, err := webpushDecrypt(body, ua, auth)
		if err != nil || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid t=") {
			t.Errorf("push: %v %q", err, r.Header.Get("Authorization"))
		}
		mu.Lock()
		pushed = append(pushed, string(plain))
		mu.Unlock()
		rw.WriteHeader(201)
	}))
	defer push.Close()

	_, session := webCall(t, w, "POST", "/api/login", `{"user":"admin","password":"`+pw+`"}`, "")
	b64 := base64.RawURLEncoding.EncodeToString
	sub := `{"endpoint":"` + push.URL + `/sub/1","keys":{"p256dh":"` + b64(ua.PublicKey().Bytes()) + `","auth":"` + b64(auth) + `"},"label":"Firefox on Linux"}`
	if code, b := webCall(t, w, "POST", "/api/notify/devices", sub, session); code != 200 {
		t.Fatalf("subscribe: %d %s", code, b)
	}
	if code, b := webCall(t, w, "POST", "/api/notify/test", `{}`, session); code != 200 || strings.Contains(b, "error") {
		t.Fatalf("test push: %d %s", code, b)
	}

	type alert struct {
		Kind  string `json:"kind"`
		Key   string `json:"key"`
		State string `json:"state"`
		Title string `json:"title"`
	}
	alerts := func() []alert {
		_, b := webCall(t, w, "GET", "/api/notify", "", session)
		var out struct {
			Alerts []alert `json:"alerts"`
		}
		if err := json.Unmarshal([]byte(b), &out); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		return out.Alerts
	}
	has := func(key string) bool {
		return slices.ContainsFunc(alerts(), func(a alert) bool { return a.Key == key })
	}

	w.vops(dev, "env", "set", "shop", "MSG=v1")
	w.write(dev, map[string]string{"shop/compose.yml": `services:
  web:
    image: APP
    environment: [MSG]
    x-vops: {port: 8080, health: /health}
  sick:
    image: APP
    environment: [FAIL_AFTER=4s]
    x-vops: {port: 8080, health: /health}
`})
	w.vops(dev, "sync", "--yes")
	// a rolling release replaces web's container: its death is vops' doing
	w.vops(dev, "env", "set", "shop", "MSG=v2")
	w.vops(dev, "apply", "--yes")
	w.eventually("sick unhealthy", func() bool { return has("health:shop/sick") })
	time.Sleep(4 * time.Second) // the old replica's death has settled
	for _, a := range alerts() {
		if strings.HasPrefix(a.Key, "restart:") || a.Key == "down:shop/web" || a.Key == "health:shop/web" {
			t.Fatalf("a deploy raised %+v", a)
		}
	}

	cs, _ := podman.PS(context.Background(), "vops.project=shop", "vops.service=web")
	if len(cs) != 1 {
		t.Fatalf("web containers: %+v", cs)
	}
	podman.Run(context.Background(), "kill", cs[0].ID)
	w.eventually("restart alert", func() bool { return has("restart:shop/web") })
	if !has("oom:shop/web") { // SIGKILL: could be an OOM killer
		t.Fatal("no oom alert for a SIGKILL")
	}
	w.eventually("pushes", func() bool {
		mu.Lock()
		defer mu.Unlock()
		all := strings.Join(pushed, "\n")
		return strings.Contains(all, "test notification") && strings.Contains(all, "shop/sick is unhealthy") && strings.Contains(all, "shop/web died")
	})
	if out := w.vops(dev, "events", "shop"); !strings.Contains(out, "alert") {
		t.Fatalf("no alert event:\n%s", out)
	}
	if out := w.vops(dev, "notifications"); !strings.Contains(out, "shop/web died") {
		t.Fatalf("vops notifications:\n%s", out)
	}
}

func webCall(t *testing.T, w *world, method, path, body, session string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://"+w.uiAddr+path, strings.NewReader(body))
	req.Header.Set("X-Vops", "1")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "vops_session", Value: session})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if session == "" {
		for _, c := range resp.Cookies() {
			if c.Name == "vops_session" {
				return resp.StatusCode, c.Value
			}
		}
	}
	return resp.StatusCode, string(b)
}

// webpushDecrypt is a browser's side of RFC 8291 (aes128gcm, one record).
func webpushDecrypt(body []byte, ua *ecdh.PrivateKey, auth []byte) ([]byte, error) {
	salt, idlen := body[:16], int(body[20])
	as, err := ecdh.P256().NewPublicKey(body[21 : 21+idlen])
	if err != nil {
		return nil, err
	}
	shared, err := ua.ECDH(as)
	if err != nil {
		return nil, err
	}
	info := append(append([]byte("WebPush: info\x00"), ua.PublicKey().Bytes()...), as.Bytes()...)
	ikm, _ := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	cek, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	block, _ := aes.NewCipher(cek)
	gcm, _ := cipher.NewGCM(block)
	plain, err := gcm.Open(nil, nonce, body[21+idlen:], nil)
	if err != nil {
		return nil, err
	}
	return plain[:len(plain)-1], nil
}
