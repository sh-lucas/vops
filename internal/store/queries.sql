-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
INSERT INTO meta (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: ListProjects :many
SELECT * FROM projects ORDER BY path;

-- name: SetProjectDisabled :exec
INSERT INTO projects (path, disabled) VALUES (?, ?)
ON CONFLICT (path) DO UPDATE SET disabled = excluded.disabled;

-- name: SetProjectApplied :exec
INSERT INTO projects (path, commit_sha, applied_at) VALUES (?, ?, ?)
ON CONFLICT (path) DO UPDATE SET commit_sha = excluded.commit_sha, applied_at = excluded.applied_at;

-- name: SetProjectLimits :exec
INSERT INTO projects (path, limits) VALUES (?, ?)
ON CONFLICT (path) DO UPDATE SET limits = excluded.limits;

-- name: DeleteProject :exec
DELETE FROM projects WHERE path = ?;

-- name: SetEnv :exec
INSERT INTO env (project, key, value, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (project, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at;

-- name: UnsetEnv :exec
DELETE FROM env WHERE project = ? AND key = ?;

-- name: ListEnvKeys :many
SELECT key, updated_at FROM env WHERE project = ? ORDER BY key;

-- name: ListEnv :many
SELECT key, value FROM env WHERE project = ?;

-- name: ListAllEnv :many
SELECT project, key, value FROM env ORDER BY project, key;

-- name: InsertEnvIfMissing :execrows
INSERT INTO env (project, key, value, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT (project, key) DO NOTHING;

-- name: CreateUser :exec
INSERT INTO users (name, role, global, secret, created_at) VALUES (?, ?, ?, ?, ?);

-- name: UpdateUser :execrows
-- an empty secret keeps the current one
UPDATE users SET role = sqlc.arg(role), global = sqlc.arg(global),
    secret = CASE WHEN sqlc.arg(secret) = '' THEN secret ELSE sqlc.arg(secret) END
WHERE name = sqlc.arg(name);

-- name: DeleteUser :execrows
DELETE FROM users WHERE name = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name;

-- name: GetUser :one
SELECT * FROM users WHERE name = ?;

-- name: CountAdmins :one
SELECT count(*) FROM users WHERE role = 'admin';

-- name: ListUserRepos :many
SELECT * FROM user_repos ORDER BY user, repo;

-- name: ListReposOfUser :many
SELECT repo FROM user_repos WHERE user = ? ORDER BY repo;

-- name: DeleteUserRepos :exec
DELETE FROM user_repos WHERE user = ?;

-- name: AddUserRepo :exec
INSERT INTO user_repos (user, repo) VALUES (?, ?);

-- name: CreateSession :exec
INSERT INTO sessions (id_hash, user, expires) VALUES (?, ?, ?);

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires < ?;

-- name: GetSession :one
SELECT user, expires FROM sessions WHERE id_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = ?;

-- name: CreateEvent :exec
INSERT INTO events (at, project, kind, message) VALUES (?, ?, ?, ?);

-- name: ListEvents :many
-- newest first; before > 0 pages back (ids below it)
SELECT * FROM events
WHERE (sqlc.arg(project) = '' OR project = sqlc.arg(project)) AND (sqlc.arg(before) = 0 OR id < sqlc.arg(before))
ORDER BY id DESC LIMIT sqlc.arg(lim);

-- name: CreateSnapshot :one
INSERT INTO snapshots (project, reason, note, commit_sha, created_at) VALUES (?, ?, ?, ?, ?)
RETURNING id;

-- name: AddSnapshotVolume :exec
INSERT INTO snapshot_volumes (snapshot_id, kind, name, source, path) VALUES (?, ?, ?, ?, ?);

-- name: GetSnapshot :one
SELECT * FROM snapshots WHERE id = ?;

-- name: ListSnapshots :many
SELECT * FROM snapshots
WHERE sqlc.arg(project) = '' OR project = sqlc.arg(project)
ORDER BY id DESC;

-- name: ListSnapshotVolumes :many
SELECT * FROM snapshot_volumes WHERE snapshot_id = ? ORDER BY name;

-- name: ListAutoSnapshotsToPrune :many
-- automatic snapshots (not manual or uploaded) of a project beyond the newest `keep`
SELECT * FROM snapshots
WHERE project = sqlc.arg(project) AND reason NOT IN ('manual', 'upload')
ORDER BY id DESC LIMIT -1 OFFSET sqlc.arg(keep);

-- name: DeleteSnapshot :exec
DELETE FROM snapshots WHERE id = ?;

-- name: ListAudit :many
SELECT * FROM audit_log ORDER BY id DESC LIMIT ?;

-- name: PutPreview :exec
-- creates a preview or updates it; created_at, snapshot_id, deploy_id and data belong to its creation
INSERT INTO previews (project, name, ref, commit_sha, images, services, snapshot_id, deploy_id, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project, name) DO UPDATE SET ref = excluded.ref, commit_sha = excluded.commit_sha, images = excluded.images, services = excluded.services, updated_at = excluded.updated_at;

-- name: GetPreview :one
SELECT * FROM previews WHERE project = ? AND name = ?;

-- name: ListPreviews :many
SELECT * FROM previews
WHERE sqlc.arg(project) = '' OR project = sqlc.arg(project)
ORDER BY project, name;

-- name: DeletePreview :exec
DELETE FROM previews WHERE project = ? AND name = ?;

-- name: CreateDeploy :one
INSERT INTO deploys (project, commit_sha, trigger, images, snapshot_id, restored_id, undoes, before_id, parts, result, error, summary, started_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: GetDeploy :one
SELECT * FROM deploys WHERE id = ?;

-- name: ListDeploys :many
SELECT * FROM deploys WHERE project = ? ORDER BY id DESC LIMIT ?;

-- name: PreviousDeploy :one
SELECT * FROM deploys WHERE project = ? AND id < ? ORDER BY id DESC LIMIT 1;

-- name: LatestDeploys :many
-- the newest deploy of every project
SELECT * FROM deploys WHERE id IN (SELECT max(id) FROM deploys GROUP BY project);

-- name: RecentDeployImages :many
-- the images of the newest `keep` deploys of every project (registry gc keeps them)
SELECT images FROM deploys d
WHERE (SELECT count(*) FROM deploys x WHERE x.project = d.project AND x.id > d.id) < CAST(sqlc.arg(keep) AS INTEGER);

-- name: PutPin :exec
INSERT INTO pins (project, service, image, digest, compose_image, deploy_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (project, service) DO UPDATE SET image = excluded.image, digest = excluded.digest, compose_image = excluded.compose_image,
    deploy_id = excluded.deploy_id, created_at = excluded.created_at;

-- name: ListPins :many
SELECT * FROM pins
WHERE sqlc.arg(project) = '' OR project = sqlc.arg(project)
ORDER BY project, service;

-- name: DeletePin :execrows
DELETE FROM pins WHERE project = ? AND service = ?;

-- name: PutSubscription :exec
-- a browser resubscribing (same endpoint) moves to whoever is logged in now
INSERT INTO push_subscriptions (user, endpoint, p256dh, auth, label, created_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (endpoint) DO UPDATE SET user = excluded.user, p256dh = excluded.p256dh, auth = excluded.auth, label = excluded.label, last_error = '';

-- name: ListSubscriptions :many
SELECT * FROM push_subscriptions ORDER BY user, id;

-- name: DeleteSubscription :execrows
DELETE FROM push_subscriptions WHERE id = ?;

-- name: DeleteSubscriptionByEndpoint :execrows
DELETE FROM push_subscriptions WHERE endpoint = ?;

-- name: SubscriptionOK :exec
UPDATE push_subscriptions SET last_ok_at = ?, last_error = '' WHERE id = ?;

-- name: SubscriptionFailed :exec
UPDATE push_subscriptions SET last_error = ? WHERE id = ?;

-- name: CreateAlert :one
INSERT INTO alerts (kind, key, project, service, title, body, url, first_at, last_at, sent_at, sends, occurrences, state, ended_at, closed_at, closed_by)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING id;

-- name: UpdateAlert :exec
UPDATE alerts SET title = ?, body = ?, url = ?, last_at = ?, sent_at = ?, sends = ?, occurrences = ?, state = ?, ended_at = ?, closed_at = ?, closed_by = ?
WHERE id = ?;

-- name: GetAlert :one
SELECT * FROM alerts WHERE id = ?;

-- name: LiveAlerts :many
-- alerts whose condition still holds (open, silenced or expired)
SELECT * FROM alerts WHERE ended_at = 0 ORDER BY id;

-- name: RecentAlerts :many
SELECT * FROM alerts ORDER BY id DESC LIMIT ?;

-- name: ListPrefs :many
SELECT * FROM notify_prefs;

-- name: SetPref :exec
INSERT INTO notify_prefs (user, kind, enabled) VALUES (?, ?, ?)
ON CONFLICT (user, kind) DO UPDATE SET enabled = excluded.enabled;

-- name: ListNotifySettings :many
SELECT * FROM notify_settings;

-- name: SetNotifySetting :exec
INSERT INTO notify_settings (key, value) VALUES (?, ?)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;

-- name: DeleteNotifySetting :exec
DELETE FROM notify_settings WHERE key = ?;
