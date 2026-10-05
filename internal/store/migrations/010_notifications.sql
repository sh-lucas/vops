-- notifications: Web Push subscriptions (one per browser/device of a user), alerts (what is or was being sent),
-- per-user kind toggles and global thresholds. Nothing of this lives in vops.yml on purpose (see decisions.md).
CREATE TABLE push_subscriptions (
    id INTEGER PRIMARY KEY,
    user TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE ON UPDATE CASCADE,
    endpoint TEXT NOT NULL UNIQUE,
    p256dh TEXT NOT NULL,
    auth TEXT NOT NULL,
    label TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    last_ok_at INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX push_subscriptions_user_idx ON push_subscriptions(user);

-- an alert is open (being sent), silenced (by closed_by), expired (sent for too long) or resolved. ended_at = 0 while
-- its condition still holds: at most one such alert per key, so a silenced or expired one doesn't reopen.
CREATE TABLE alerts (
    id INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,
    key TEXT NOT NULL,
    project TEXT NOT NULL DEFAULT '',
    service TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    body TEXT NOT NULL DEFAULT '',
    url TEXT NOT NULL DEFAULT '',
    first_at INTEGER NOT NULL,
    last_at INTEGER NOT NULL,
    sent_at INTEGER NOT NULL DEFAULT 0,
    sends INTEGER NOT NULL DEFAULT 0,
    occurrences INTEGER NOT NULL DEFAULT 1,
    state TEXT NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'resolved', 'silenced', 'expired')),
    ended_at INTEGER NOT NULL DEFAULT 0,
    closed_at INTEGER NOT NULL DEFAULT 0,
    closed_by TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX alerts_live_key ON alerts(key) WHERE ended_at = 0;
CREATE INDEX alerts_state_idx ON alerts(state, id);

CREATE TRIGGER alerts_retention AFTER INSERT ON alerts
BEGIN
    DELETE FROM alerts WHERE id <= NEW.id - 2000 AND ended_at != 0;
END;

CREATE TABLE notify_prefs (
    user TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE ON UPDATE CASCADE,
    kind TEXT NOT NULL,
    enabled BOOLEAN NOT NULL CHECK (enabled IN (0, 1)),
    PRIMARY KEY (user, kind)
);

-- thresholds and intervals; a missing key is its default
CREATE TABLE notify_settings (
    key TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);

CREATE TRIGGER audit_push_subscriptions_insert AFTER INSERT ON push_subscriptions
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('push_subscriptions', 'insert', NEW.user, NEW.label);
END;
CREATE TRIGGER audit_push_subscriptions_delete AFTER DELETE ON push_subscriptions
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('push_subscriptions', 'delete', OLD.user, OLD.label);
END;
CREATE TRIGGER audit_alerts_silence AFTER UPDATE OF state ON alerts
WHEN NEW.state = 'silenced' AND OLD.state != 'silenced'
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('alerts', 'update', NEW.key, 'silenced by ' || NEW.closed_by);
END;
CREATE TRIGGER audit_notify_prefs_insert AFTER INSERT ON notify_prefs
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('notify_prefs', 'insert', NEW.user || ' ' || NEW.kind, CASE WHEN NEW.enabled THEN 'on' ELSE 'off' END);
END;
CREATE TRIGGER audit_notify_prefs_update AFTER UPDATE ON notify_prefs
WHEN OLD.enabled IS NOT NEW.enabled
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('notify_prefs', 'update', NEW.user || ' ' || NEW.kind, CASE WHEN NEW.enabled THEN 'on' ELSE 'off' END);
END;
CREATE TRIGGER audit_notify_settings_insert AFTER INSERT ON notify_settings
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('notify_settings', 'insert', NEW.key, NEW.value);
END;
CREATE TRIGGER audit_notify_settings_update AFTER UPDATE ON notify_settings
WHEN OLD.value IS NOT NEW.value
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('notify_settings', 'update', NEW.key, OLD.value || ' -> ' || NEW.value);
END;
CREATE TRIGGER audit_notify_settings_delete AFTER DELETE ON notify_settings
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('notify_settings', 'delete', OLD.key, 'back to default');
END;
