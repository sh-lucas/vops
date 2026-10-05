package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/store"
)

// The alert state machine, pure (time is given): what a detector saw (Seen), how time moves an alert on (Step),
// and a user stopping it (Silence). Everything else (db, push) is the Notifier's.

// Obs is something a detector saw: a condition that holds now, or one occurrence of an event.
type Obs struct {
	Kind, Key        string
	Project, Service string
	Title, Body, URL string
}

// Seen records an observation on the live alert of its key (nil = none: a new open alert).
// An event kind counts one more occurrence and says so in the title ("shop/web restarted (5 times in 12m)").
func Seen(a *store.Alert, o Obs, now time.Time) store.Alert {
	t := now.Unix()
	if a == nil {
		return store.Alert{Kind: o.Kind, Key: o.Key, Project: o.Project, Service: o.Service, Title: o.Title, Body: o.Body, Url: o.URL,
			FirstAt: t, LastAt: t, Occurrences: 1, State: "open"}
	}
	x := *a
	x.LastAt, x.Body = t, o.Body
	if o.URL != "" {
		x.Url = o.URL
	}
	x.Title = o.Title
	if k, _ := kindOf(o.Kind); k.Event {
		x.Occurrences++
		x.Title = fmt.Sprintf("%s (%d times in %s)", o.Title, x.Occurrences, span(time.Duration(t-x.FirstAt)*time.Second))
	}
	return x
}

// Step moves a live alert to now. send: push it now (new, or repeat is due). resolved: it just ended while being
// sent (the optional "resolved" notification). A live alert ends when it wasn't seen for s.hold(kind).
func Step(a store.Alert, now time.Time, s Settings) (out store.Alert, send, resolved bool) {
	t := now.Unix()
	if a.EndedAt != 0 {
		return a, false, false
	}
	if time.Duration(t-a.LastAt)*time.Second >= s.hold(a.Kind) {
		a.EndedAt = t
		if a.State == "open" {
			a.State, a.ClosedAt, resolved = "resolved", t, true
		}
		return a, false, resolved && a.Sends > 0
	}
	if a.State != "open" {
		return a, false, false
	}
	if t-a.FirstAt >= s.Expire {
		a.State, a.ClosedAt = "expired", t
		return a, false, false
	}
	if a.SentAt == 0 || t-a.SentAt >= s.Repeat {
		a.SentAt, a.Sends, send = t, a.Sends+1, true
	}
	return a, send, false
}

// End finishes a live alert now because its cause is gone (a deploy of the project succeeded).
func End(a store.Alert, now time.Time) (out store.Alert, resolved bool) {
	if a.EndedAt != 0 {
		return a, false
	}
	a.EndedAt = now.Unix()
	if a.State == "open" {
		a.State, a.ClosedAt = "resolved", a.EndedAt
		return a, a.Sends > 0
	}
	return a, false
}

// Silence stops sending an open alert, for everyone; it stays live (doesn't reopen) until its condition ends.
func Silence(a store.Alert, user string, now time.Time) (store.Alert, error) {
	if a.State != "open" {
		return a, fmt.Errorf("alert #%d is %s, not open", a.ID, a.State)
	}
	a.State, a.ClosedAt, a.ClosedBy = "silenced", now.Unix(), user
	return a, nil
}

// span is a short duration for humans: 45s, 12m, 3h, 2d.
func span(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// CanSee is the access rule for alerts: a user sees a project's alerts when they are global (admins are) or when
// a service of the project runs one of their repos (repos: project -> its own-registry repos). Host-wide alerts
// (project "") are for global users only. A preview's alerts follow its project.
func CanSee(u store.User, project string, repos map[string][]string) bool {
	if u.Global {
		return true
	}
	if project == "" {
		return false
	}
	base, _, _ := strings.Cut(project, "@")
	for _, r := range repos[base] {
		if u.Allows(r) {
			return true
		}
	}
	return false
}
