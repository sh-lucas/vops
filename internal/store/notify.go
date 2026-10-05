package store

import (
	"database/sql"
	"errors"

	"github.com/sh-lucas/vops/internal/store/queries"
)

// ---- notifications: push subscriptions, alerts, per-user kind toggles, thresholds

type Subscription = queries.PushSubscription

func (d *DB) PutSubscription(s Subscription) error {
	return d.q.PutSubscription(ctx, queries.PutSubscriptionParams{User: s.User, Endpoint: s.Endpoint, P256dh: s.P256dh, Auth: s.Auth, Label: s.Label, CreatedAt: now()})
}

func (d *DB) Subscriptions() ([]Subscription, error) { return d.q.ListSubscriptions(ctx) }

// DeleteSubscription removes a device by id and reports whether it existed.
func (d *DB) DeleteSubscription(id int64) (bool, error) {
	n, err := d.q.DeleteSubscription(ctx, id)
	return n > 0, err
}

func (d *DB) DeleteSubscriptionByEndpoint(endpoint string) (bool, error) {
	n, err := d.q.DeleteSubscriptionByEndpoint(ctx, endpoint)
	return n > 0, err
}

// SubscriptionResult records the outcome of a push to a device ("" = delivered).
func (d *DB) SubscriptionResult(id int64, errText string) {
	if errText == "" {
		d.q.SubscriptionOK(ctx, queries.SubscriptionOKParams{LastOkAt: now(), ID: id})
		return
	}
	d.q.SubscriptionFailed(ctx, queries.SubscriptionFailedParams{LastError: errText, ID: id})
}

type Alert = queries.Alert

// SaveAlert inserts a new alert (ID 0) or updates it, and returns its id.
func (d *DB) SaveAlert(a Alert) (int64, error) {
	if a.ID == 0 {
		return d.q.CreateAlert(ctx, queries.CreateAlertParams{Kind: a.Kind, Key: a.Key, Project: a.Project, Service: a.Service, Title: a.Title, Body: a.Body, Url: a.Url,
			FirstAt: a.FirstAt, LastAt: a.LastAt, SentAt: a.SentAt, Sends: a.Sends, Occurrences: a.Occurrences, State: a.State, EndedAt: a.EndedAt, ClosedAt: a.ClosedAt, ClosedBy: a.ClosedBy})
	}
	return a.ID, d.q.UpdateAlert(ctx, queries.UpdateAlertParams{Title: a.Title, Body: a.Body, Url: a.Url, LastAt: a.LastAt, SentAt: a.SentAt, Sends: a.Sends,
		Occurrences: a.Occurrences, State: a.State, EndedAt: a.EndedAt, ClosedAt: a.ClosedAt, ClosedBy: a.ClosedBy, ID: a.ID})
}

// Alert returns one alert; found is false when it doesn't exist.
func (d *DB) Alert(id int64) (a Alert, found bool, err error) {
	a, err = d.q.GetAlert(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Alert{}, false, nil
	}
	return a, err == nil, err
}

// LiveAlerts are the alerts whose condition still holds (open, silenced or expired).
func (d *DB) LiveAlerts() ([]Alert, error) { return d.q.LiveAlerts(ctx) }

// RecentAlerts lists the newest alerts, newest first.
func (d *DB) RecentAlerts(limit int) ([]Alert, error) { return d.q.RecentAlerts(ctx, int64(limit)) }

// Prefs returns every user's explicit kind toggles: user -> kind -> enabled (missing = the kind's default).
func (d *DB) Prefs() (map[string]map[string]bool, error) {
	rows, err := d.q.ListPrefs(ctx)
	out := map[string]map[string]bool{}
	for _, r := range rows {
		if out[r.User] == nil {
			out[r.User] = map[string]bool{}
		}
		out[r.User][r.Kind] = r.Enabled
	}
	return out, err
}

func (d *DB) SetPref(user, kind string, enabled bool) error {
	return d.q.SetPref(ctx, queries.SetPrefParams{User: user, Kind: kind, Enabled: enabled})
}

// NotifySettings returns the thresholds set by hand (missing = default).
func (d *DB) NotifySettings() (map[string]int64, error) {
	rows, err := d.q.ListNotifySettings(ctx)
	out := map[string]int64{}
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, err
}

// SetNotifySettings replaces the thresholds set by hand (an empty map = every default).
func (d *DB) SetNotifySettings(values map[string]int64) error {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := d.q.WithTx(tx)
	old, err := q.ListNotifySettings(ctx)
	if err != nil {
		return err
	}
	for _, r := range old {
		if _, keep := values[r.Key]; !keep {
			if err := q.DeleteNotifySetting(ctx, r.Key); err != nil {
				return err
			}
		}
	}
	for k, v := range values {
		if err := q.SetNotifySetting(ctx, queries.SetNotifySettingParams{Key: k, Value: v}); err != nil {
			return err
		}
	}
	return tx.Commit()
}
