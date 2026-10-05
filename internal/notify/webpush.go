package notify

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Web Push with the stdlib: VAPID (RFC 8292) authenticates us to the browser's push service, aes128gcm (RFC 8291)
// encrypts the payload so only the subscribed browser reads it.

var b64 = base64.RawURLEncoding

// Keys is the VAPID keypair; Public is what browsers subscribe with (applicationServerKey).
type Keys struct {
	priv   *ecdsa.PrivateKey
	Public string // base64url of the uncompressed P-256 point
}

// NewKeys generates a keypair; Private returns it for storage.
func NewKeys() (*Keys, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return keysOf(priv)
}

// ParseKeys reads a stored private key (base64url of the 32-byte scalar).
func ParseKeys(private string) (*Keys, error) {
	raw, err := b64.DecodeString(private)
	if err != nil {
		return nil, err
	}
	priv, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), raw)
	if err != nil {
		return nil, err
	}
	return keysOf(priv)
}

func keysOf(priv *ecdsa.PrivateKey) (*Keys, error) {
	pub, err := priv.PublicKey.Bytes()
	if err != nil {
		return nil, err
	}
	return &Keys{priv: priv, Public: b64.EncodeToString(pub)}, nil
}

func (k *Keys) Private() string {
	b, _ := k.priv.Bytes()
	return b64.EncodeToString(b)
}

// vapid returns the Authorization header for a push service: an ES256 JWT for its origin, valid 12h.
func (k *Keys) vapid(endpoint, sub string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid endpoint %q", endpoint)
	}
	head := b64.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, _ := json.Marshal(map[string]any{"aud": u.Scheme + "://" + u.Host, "exp": now.Add(12 * time.Hour).Unix(), "sub": sub})
	signing := head + "." + b64.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + k.Public, nil
}

// encrypt is RFC 8291: an ephemeral ECDH key (asPriv) with the browser's p256dh, its auth secret and a salt give the
// content key and nonce; the body is one aes128gcm record (header: salt, record size, our public key).
func encrypt(payload, uaPublic, authSecret []byte, asPriv *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(authSecret) != 16 || len(salt) != 16 {
		return nil, errors.New("auth secret and salt must be 16 bytes")
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPublic)
	if err != nil {
		return nil, fmt.Errorf("invalid p256dh: %w", err)
	}
	shared, err := asPriv.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPublic := asPriv.PublicKey().Bytes()
	info := append(append([]byte("WebPush: info\x00"), uaPublic...), asPublic...)
	ikm, err := hkdf.Key(sha256.New, shared, authSecret, string(info), 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(payload) > 3993 { // 4096 minus header, tag and delimiter: one record
		return nil, errors.New("payload too large")
	}
	header := make([]byte, 0, 86)
	header = append(header, salt...)
	header = binary.BigEndian.AppendUint32(header, 4096)
	header = append(header, byte(len(asPublic)))
	header = append(header, asPublic...)
	return gcm.Seal(header, nonce, append(payload, 2), nil), nil // 0x02: the last (and only) record
}

// Sub is a browser's subscription as PushManager.subscribe() returns it.
type Sub struct {
	Endpoint string
	P256dh   string // base64url
	Auth     string // base64url
}

// pushClient: a push service that doesn't answer in 10s is treated as a failed delivery.
var pushClient = &http.Client{Timeout: 10 * time.Second}

// Send delivers one encrypted payload. It returns the push service's status (404/410: the subscription is gone).
func (k *Keys) Send(ctx context.Context, s Sub, payload []byte, subject string, ttl time.Duration, urgency string) (int, error) {
	ua, err := b64decode(s.P256dh)
	if err != nil {
		return 0, fmt.Errorf("p256dh: %w", err)
	}
	auth, err := b64decode(s.Auth)
	if err != nil {
		return 0, fmt.Errorf("auth: %w", err)
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return 0, err
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	body, err := encrypt(payload, ua, auth, eph, salt)
	if err != nil {
		return 0, err
	}
	authz, err := k.vapid(s.Endpoint, subject, time.Now())
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", s.Endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", authz)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(int(ttl.Seconds())))
	req.Header.Set("Urgency", urgency)
	resp, err := pushClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return resp.StatusCode, fmt.Errorf("push service: %s %s", resp.Status, bytes.TrimSpace(msg))
	}
	return resp.StatusCode, nil
}

// b64decode accepts base64url with or without padding (browsers differ).
func b64decode(s string) ([]byte, error) {
	if b, err := b64.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
