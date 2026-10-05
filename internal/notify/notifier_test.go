package notify

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sh-lucas/vops/internal/store"
)

// The Notifier end to end on a real db: who gets what (toggles, access), coalescing, silence, 410 removing a device.
func TestNotifierDelivers(t *testing.T) {
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "vops.db"), filepath.Join(dir, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	n, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := New(db); again.Keys.Public != n.Keys.Public {
		t.Fatal("the VAPID key changed on restart")
	}
	n.Repos = func() map[string][]string { return map[string][]string{"shop": {"shop/web"}} }

	var mu sync.Mutex
	got := map[string][]string{} // device -> titles
	type device struct {
		key  *ecdh.PrivateKey
		auth []byte
	}
	devs := map[string]device{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "gone" {
			w.WriteHeader(410)
			return
		}
		body, _ := io.ReadAll(r.Body)
		plain, err := decrypt(body, devs[name].key, devs[name].auth)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		var m message
		json.Unmarshal(plain, &m)
		mu.Lock()
		got[name] = append(got[name], m.Title)
		mu.Unlock()
		w.WriteHeader(201)
	}))
	defer srv.Close()
	subscribe := func(user, name string) {
		k, _ := ecdh.P256().GenerateKey(rand.Reader)
		auth := make([]byte, 16)
		rand.Read(auth)
		devs[name] = device{k, auth}
		if err := db.PutSubscription(store.Subscription{User: user, Endpoint: srv.URL + "/" + name, P256dh: b64.EncodeToString(k.PublicKey().Bytes()), Auth: b64.EncodeToString(auth), Label: name}); err != nil {
			t.Fatal(err)
		}
	}
	db.PutUser(store.User{Name: "admin", Role: store.Admin}, "admin password 1")
	db.PutUser(store.User{Name: "ops", Role: store.Admin}, "ops password 12")
	db.PutUser(store.User{Name: "ci", Role: store.Deployer, Repos: []string{"blog/site"}}, "token-ci-01234")
	subscribe("admin", "phone")
	subscribe("admin", "laptop")
	subscribe("ops", "ops-phone")
	subscribe("ci", "ci-box")
	subscribe("ops", "gone")
	db.SetPref("ops", "deploy_failed", false)

	n.Deployed("shop", errors.New("web: not ready after 1m"))
	n.Deployed("shop", errors.New("web: not ready after 1m")) // coalesced: no second push
	n.Wait()
	for dev, want := range map[string]int{"phone": 1, "laptop": 1, "ops-phone": 0, "ci-box": 0} {
		if len(got[dev]) != want {
			t.Errorf("%s got %v", dev, got[dev])
		}
	}
	live, _ := db.LiveAlerts()
	if len(live) != 1 || live[0].Occurrences != 2 || live[0].Kind != "deploy_failed" {
		t.Fatalf("live: %+v", live)
	}
	if evs, _ := db.Events("shop", 5); len(evs) != 1 || evs[0].Kind != "alert" {
		t.Fatalf("events: %+v", evs)
	}
	// a host alert: global users only; the 410 device is removed
	n.Raise(Obs{Kind: "resources", Key: "resources:disk:/", Title: "disk 95% full"})
	n.Wait()
	if len(got["ops-phone"]) != 1 || len(got["ci-box"]) != 0 {
		t.Fatalf("host alert: %v", got)
	}
	subs, _ := db.Subscriptions()
	for _, s := range subs {
		if s.Label == "gone" {
			t.Fatal("a 410 device stayed")
		}
		if s.Label == "phone" && (s.LastOkAt == 0 || s.LastError != "") {
			t.Fatalf("phone: %+v", s)
		}
	}
	// silence: recorded, never sent again; a successful deploy ends it without a "resolved" (off by default)
	if err := n.Silence(live[0].ID, "ops"); err != nil {
		t.Fatal(err)
	}
	n.Deployed("shop", nil)
	n.Wait()
	a, _, _ := db.Alert(live[0].ID)
	if a.State != "silenced" || a.ClosedBy != "ops" || a.EndedAt == 0 {
		t.Fatalf("after deploy: %+v", a)
	}
	if len(got["phone"]) != 2 { // deploy_failed + disk
		t.Fatalf("phone: %v", got["phone"])
	}
	// test: only the user's own devices
	res, err := n.Test("admin", "")
	if err != nil || len(res) != 2 {
		t.Fatalf("test: %v %+v", err, res)
	}
	if _, err := n.Test("nobody", ""); err == nil {
		t.Fatal("test without devices")
	}
}
