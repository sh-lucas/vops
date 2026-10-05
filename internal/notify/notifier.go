package notify

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sh-lucas/vops/internal/store"
)

// Notifier keeps alerts in the db and pushes them to the devices of the users who want them and can see them.
type Notifier struct {
	DB      *store.DB
	Keys    *Keys
	Subject func() string              // VAPID sub: mailto:<acme email> or the dashboard's url
	Repos   func() map[string][]string // project -> own-registry repos it runs (only asked when a user isn't global)

	mu       sync.Mutex
	settings Settings
	sending  sync.WaitGroup
}

const vapidMeta = "vapid_private"

// New loads the VAPID keypair from the db, or makes one (once: browsers subscribed to it).
func New(db *store.DB) (*Notifier, error) {
	n := &Notifier{DB: db, Subject: func() string { return "mailto:vops@localhost" }, Repos: func() map[string][]string { return nil }}
	keys, err := ParseKeys(db.Meta(vapidMeta))
	if err != nil {
		if keys, err = NewKeys(); err != nil {
			return nil, err
		}
		if err := db.SetMeta(vapidMeta, keys.Private()); err != nil {
			return nil, err
		}
	}
	n.Keys = keys
	stored, err := db.NotifySettings()
	n.settings = SettingsFrom(stored)
	return n, err
}

func (n *Notifier) Settings() Settings {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.settings
}

// SetSettings validates and stores thresholds (Defaults = reset).
func (n *Notifier) SetSettings(s Settings) error {
	o, err := s.Overrides()
	if err != nil {
		return err
	}
	if err := n.DB.SetNotifySettings(o); err != nil {
		return err
	}
	n.mu.Lock()
	n.settings = s
	n.mu.Unlock()
	return nil
}

func (n *Notifier) live(key string) (*store.Alert, error) {
	all, err := n.DB.LiveAlerts()
	if err != nil {
		return nil, err
	}
	if i := slices.IndexFunc(all, func(a store.Alert) bool { return a.Key == key }); i >= 0 {
		return &all[i], nil
	}
	return nil, nil
}

// Raise records an observation: a new alert (an event row and a push right away), or one more sighting of a live one.
func (n *Notifier) Raise(o Obs) {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	cur, err := n.live(o.Key)
	if err != nil {
		log.Printf("alerts: %v", err)
		return
	}
	a := Seen(cur, o, now)
	if cur == nil {
		n.DB.Event(o.Project, "alert", "%s: %s", o.Title, o.Body)
	}
	n.step(cur, a, now)
}

// Keep marks the live alerts of a kind as still seen (a detector that skipped a cycle, e.g. during a deploy).
func (n *Notifier) Keep(kinds ...string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	all, _ := n.DB.LiveAlerts()
	for _, a := range all {
		if slices.Contains(kinds, a.Kind) {
			a.LastAt = time.Now().Unix()
			n.DB.SaveAlert(a)
		}
	}
}

// Sweep moves every live alert to now: repeats, expiry, resolution.
func (n *Notifier) Sweep() {
	n.mu.Lock()
	defer n.mu.Unlock()
	all, err := n.DB.LiveAlerts()
	if err != nil {
		log.Printf("alerts: %v", err)
		return
	}
	now := time.Now()
	for _, a := range all {
		n.step(&a, a, now)
	}
}

// step saves a's next state (when it differs from the stored one, saved) and pushes what it says. Caller holds mu.
func (n *Notifier) step(saved *store.Alert, a store.Alert, now time.Time) {
	next, send, resolved := Step(a, now, n.settings)
	if saved == nil || next != *saved {
		id, err := n.DB.SaveAlert(next)
		if err != nil {
			log.Printf("alerts: save %s: %v", a.Key, err)
			return
		}
		next.ID = id
	}
	switch {
	case send:
		n.dispatch(next, false)
	case resolved:
		n.dispatch(next, true)
	}
}

// End finishes the live alert of key now (its cause went away).
func (n *Notifier) End(key string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	cur, err := n.live(key)
	if err != nil || cur == nil {
		return
	}
	a, resolved := End(*cur, time.Now())
	if _, err := n.DB.SaveAlert(a); err == nil && resolved {
		n.dispatch(a, true)
	}
}

// Silence stops an open alert for everyone and records who.
func (n *Notifier) Silence(id int64, user string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	a, found, err := n.DB.Alert(id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no alert #%d", id)
	}
	if a, err = Silence(a, user, time.Now()); err != nil {
		return err
	}
	_, err = n.DB.SaveAlert(a)
	return err
}

// Deployed is the engine's OnDeploy: a failure raises deploy_failed, a success ends it and may say so.
func (n *Notifier) Deployed(project string, err error) {
	key := "deploy_failed:" + project
	url := projectURL(project, "timeline")
	if err != nil {
		n.Raise(Obs{Kind: "deploy_failed", Key: key, Project: project, Title: project + ": deploy failed", Body: trunc(err.Error(), 300), URL: url})
		return
	}
	n.End(key)
	n.Once("deploy_ok", project, project+" deployed", "", url)
}

// Once pushes a notification that is no alert (successful deploys).
func (n *Notifier) Once(kind, project, title, body, url string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.dispatch(store.Alert{Kind: kind, Key: kind + ":" + project, Project: project, Title: title, Body: body, Url: url}, false)
}

type message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag"`
}

// Wants reports whether a user gets a kind: their toggle, else the kind's default.
func Wants(prefs map[string]map[string]bool, user, kind string) bool {
	if v, ok := prefs[user][kind]; ok {
		return v
	}
	k, _ := kindOf(kind)
	return k.Default
}

// dispatch pushes an alert (or its resolution) to every device of the users who want its kind and can see its
// project, in the background. Caller holds mu.
func (n *Notifier) dispatch(a store.Alert, resolved bool) {
	users, err1 := n.DB.Users()
	prefs, err2 := n.DB.Prefs()
	subs, err3 := n.DB.Subscriptions()
	if err := firstErr(err1, err2, err3); err != nil {
		log.Printf("notify: %v", err)
		return
	}
	msg := message{a.Title, a.Body, a.Url, a.Key}
	urgency := "normal"
	if a.Kind == "down" || a.Kind == "oom" {
		urgency = "high"
	}
	if resolved {
		msg.Title, msg.Body, urgency = "Resolved: "+a.Title, "", "low"
	}
	payload, _ := json.Marshal(msg)
	var repos map[string][]string
	for _, s := range subs {
		i := slices.IndexFunc(users, func(u store.User) bool { return u.Name == s.User })
		if i < 0 || !Wants(prefs, s.User, a.Kind) || resolved && !Wants(prefs, s.User, "resolved") {
			continue
		}
		if !users[i].Global && repos == nil {
			repos = n.Repos()
		}
		if !CanSee(users[i], a.Project, repos) {
			continue
		}
		n.push(s, payload, urgency)
	}
}

func (n *Notifier) push(s store.Subscription, payload []byte, urgency string) {
	ttl := min(sec(n.settings.Repeat), 24*time.Hour)
	subject := n.Subject()
	n.sending.Go(func() {
		n.send(s, payload, subject, ttl, urgency)
	})
}

// send delivers to one device and records the outcome; a subscription the push service forgot is removed.
func (n *Notifier) send(s store.Subscription, payload []byte, subject string, ttl time.Duration, urgency string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code, err := n.Keys.Send(ctx, Sub{s.Endpoint, s.P256dh, s.Auth}, payload, subject, ttl, urgency)
	switch {
	case code == 404 || code == 410:
		n.DB.DeleteSubscription(s.ID)
		n.DB.Event("", "alert", "device %q of %s removed: its push subscription expired", s.Label, s.User)
	case err != nil:
		n.DB.SubscriptionResult(s.ID, trunc(err.Error(), 200))
	default:
		n.DB.SubscriptionResult(s.ID, "")
	}
	return err
}

// Wait waits for the pushes in flight (tests, shutdown).
func (n *Notifier) Wait() { n.sending.Wait() }

// TestResult is one device's answer to "Send test".
type TestResult struct {
	ID    int64  `json:"id"`
	Label string `json:"label"`
	Error string `json:"error,omitempty"`
}

// Test pushes a test notification to a user's devices (only = one device by endpoint) and waits for the answers.
func (n *Notifier) Test(user, only string) ([]TestResult, error) {
	subs, err := n.DB.Subscriptions()
	if err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(message{"vops: test notification", "Notifications work on this device.", "/#/notifications", "test"})
	out := []TestResult{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range subs {
		if s.User != user || only != "" && s.Endpoint != only {
			continue
		}
		wg.Go(func() {
			r := TestResult{ID: s.ID, Label: s.Label}
			if err := n.send(s, payload, n.Subject(), time.Minute, "normal"); err != nil {
				r.Error = err.Error()
			}
			mu.Lock()
			out = append(out, r)
			mu.Unlock()
		})
	}
	wg.Wait()
	if len(out) == 0 {
		return out, fmt.Errorf("%s has no device with notifications on", user)
	}
	return out, nil
}

func projectURL(project, tab string) string {
	if project == "" {
		return "/#/system"
	}
	base, _, _ := strings.Cut(project, "@")
	return "/#/p/" + base + "/" + tab
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
