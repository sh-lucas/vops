package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/notify"
	"github.com/sh-lucas/vops/internal/store"
)

// notifier sets up notifications: the VAPID keys, the subject push services may contact, who sees which project,
// and deploy outcomes from the engine.
func (d *Daemon) notifier() error {
	n, err := notify.New(d.DB)
	if err != nil {
		return fmt.Errorf("notifications: %w", err)
	}
	n.Subject = func() string {
		c := d.cfg.Load()
		switch {
		case c.Email != "":
			return "mailto:" + c.Email
		case c.Domain != "":
			return "https://vops." + c.Domain
		}
		return "mailto:vops@localhost"
	}
	n.Repos = func() map[string][]string {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		plan, err := d.Engine.Plan(ctx)
		if err != nil {
			return map[string][]string{}
		}
		return plan.ProjectRepos()
	}
	d.notify = n
	d.Engine.OnDeploy = n.Deployed
	return nil
}

// device is a subscription as the dashboard shows it: never the endpoint (it is a capability url).
type device struct {
	ID        int64  `json:"id"`
	User      string `json:"user"`
	Label     string `json:"label"`
	CreatedAt int64  `json:"created_at"`
	LastOKAt  int64  `json:"last_ok_at"`
	LastError string `json:"last_error"`
	Mine      bool   `json:"mine"` // the endpoint the caller asked about (this browser)
}

func (d *Daemon) notifyRoutes(h func(string, func(http.ResponseWriter, *http.Request) error)) {
	// everything the Notifications page shows, in one call; endpoint= says which device is this browser
	h("GET /api/notify", func(w http.ResponseWriter, r *http.Request) error {
		user := actor(r)
		subs, err := d.DB.Subscriptions()
		if err != nil {
			return err
		}
		prefs, err := d.DB.Prefs()
		if err != nil {
			return err
		}
		recent, err := d.DB.RecentAlerts(200)
		if err != nil {
			return err
		}
		u := d.viewer(r)
		var repos map[string][]string
		alerts := []store.Alert{}
		for _, a := range recent {
			if !u.Global && repos == nil {
				repos = d.notify.Repos()
			}
			if notify.CanSee(u, a.Project, repos) {
				alerts = append(alerts, a)
			}
		}
		devices := []device{}
		mine := r.URL.Query().Get("endpoint")
		for _, s := range subs {
			devices = append(devices, device{s.ID, s.User, s.Label, s.CreatedAt, s.LastOkAt, s.LastError, mine != "" && s.Endpoint == mine})
		}
		type kind struct {
			notify.Kind
			Enabled bool `json:"enabled"`
		}
		kinds := []kind{}
		for _, k := range notify.Kinds {
			kinds = append(kinds, kind{k, notify.Wants(prefs, user, k.ID)})
		}
		writeJSON(w, map[string]any{"public_key": d.notify.Keys.Public, "user": user, "kinds": kinds, "alerts": alerts, "devices": devices,
			"settings": d.notify.Settings(), "defaults": notify.Defaults})
		return nil
	})
	// this browser subscribed (PushSubscription.toJSON() plus a label); the same endpoint again moves it to the caller
	h("POST /api/notify/devices", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Endpoint string `json:"endpoint"`
			Keys     struct {
				P256dh string `json:"p256dh"`
				Auth   string `json:"auth"`
			} `json:"keys"`
			Label string `json:"label"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		local := strings.HasPrefix(in.Endpoint, "http://127.0.0.1:") || strings.HasPrefix(in.Endpoint, "http://localhost:") // a test push service
		if !strings.HasPrefix(in.Endpoint, "https://") && !local || in.Keys.P256dh == "" || in.Keys.Auth == "" {
			return errors.New("a push subscription needs an https endpoint and its p256dh and auth keys")
		}
		user, err := dashboardUser(r)
		if err != nil {
			return err
		}
		if err := d.DB.PutSubscription(store.Subscription{User: user, Endpoint: in.Endpoint, P256dh: in.Keys.P256dh, Auth: in.Keys.Auth, Label: trunc(in.Label, 120)}); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	// any user removes any device (id=), or this browser's (endpoint=)
	h("DELETE /api/notify/devices", func(w http.ResponseWriter, r *http.Request) error {
		q := r.URL.Query()
		var found bool
		var err error
		if ep := q.Get("endpoint"); ep != "" {
			found, err = d.DB.DeleteSubscriptionByEndpoint(ep)
		} else {
			id, perr := strconv.ParseInt(q.Get("id"), 10, 64)
			if perr != nil {
				return errors.New("invalid id")
			}
			found, err = d.DB.DeleteSubscription(id)
		}
		if err != nil {
			return err
		}
		if !found {
			return errors.New("no such device")
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	// a test push to the caller's devices (endpoint=: only this browser), waiting for the push services' answers
	h("POST /api/notify/test", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Endpoint string `json:"endpoint"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		user, err := dashboardUser(r)
		if err != nil {
			return err
		}
		res, err := d.notify.Test(user, in.Endpoint)
		if err != nil {
			return err
		}
		writeJSON(w, res)
		return nil
	})
	h("POST /api/notify/prefs", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Kind    string `json:"kind"`
			Enabled bool   `json:"enabled"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if !slices.ContainsFunc(notify.Kinds, func(k notify.Kind) bool { return k.ID == in.Kind }) {
			return fmt.Errorf("unknown kind %q", in.Kind)
		}
		user, err := dashboardUser(r)
		if err != nil {
			return err
		}
		if err := d.DB.SetPref(user, in.Kind, in.Enabled); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
	// thresholds and intervals: POST the whole set, DELETE goes back to the defaults
	h("POST /api/notify/settings", func(w http.ResponseWriter, r *http.Request) error {
		s := d.notify.Settings()
		if err := readJSON(r, &s); err != nil {
			return err
		}
		if err := d.notify.SetSettings(s); err != nil {
			return err
		}
		d.DB.Event("", "config", "notification settings changed by %s", actor(r))
		writeJSON(w, s)
		return nil
	})
	h("DELETE /api/notify/settings", func(w http.ResponseWriter, r *http.Request) error {
		if err := d.notify.SetSettings(notify.Defaults); err != nil {
			return err
		}
		d.DB.Event("", "config", "notification settings reset to defaults by %s", actor(r))
		writeJSON(w, notify.Defaults)
		return nil
	})
	// any user silences any open alert, for everyone
	h("POST /api/notify/silence", func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			ID int64 `json:"id"`
		}
		if err := readJSON(r, &in); err != nil {
			return err
		}
		if err := d.notify.Silence(in.ID, actor(r)); err != nil {
			return err
		}
		writeJSON(w, map[string]bool{"ok": true})
		return nil
	})
}

// dashboardUser is the logged-in user: devices and toggles belong to one (the cli socket has none).
func dashboardUser(r *http.Request) (string, error) {
	if u, ok := r.Context().Value(userKey{}).(string); ok {
		return u, nil
	}
	return "", errors.New("devices and notification toggles belong to a dashboard user: use the dashboard")
}

// openAlerts counts the alerts being sent that the caller can see (the sidebar badge).
func (d *Daemon) openAlerts(r *http.Request) int {
	live, err := d.DB.LiveAlerts()
	if err != nil {
		return 0
	}
	u := d.viewer(r)
	var repos map[string][]string
	n := 0
	for _, a := range live {
		if a.State != "open" {
			continue
		}
		if !u.Global && repos == nil {
			repos = d.notify.Repos()
		}
		if notify.CanSee(u, a.Project, repos) {
			n++
		}
	}
	return n
}

// viewer is who asks, for the access rule: the logged-in user, or the cli on the socket (ssh: sees everything).
func (d *Daemon) viewer(r *http.Request) store.User {
	name, err := dashboardUser(r)
	if err != nil {
		return store.User{Name: "cli", Global: true}
	}
	u, _, _ := d.DB.User(name)
	return u
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
