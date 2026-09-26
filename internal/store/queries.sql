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

-- name: CreateOrReplaceUser :exec
INSERT INTO users (name, token_hash, pattern, repos, created_at) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET token_hash = excluded.token_hash, pattern = excluded.pattern, repos = excluded.repos;

-- name: UpdateUserRules :execrows
UPDATE users SET pattern = ?, repos = ? WHERE name = ?;

-- name: DeleteUser :exec
DELETE FROM users WHERE name = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY name;

-- name: GetUser :one
SELECT * FROM users WHERE name = ?;

-- name: CreateSession :exec
INSERT INTO sessions (id_hash, expires) VALUES (?, ?);

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires < ?;

-- name: GetSessionExpiry :one
SELECT expires FROM sessions WHERE id_hash = ?;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_hash = ?;

-- name: DeleteAllSessions :exec
DELETE FROM sessions;

-- name: CreateEvent :exec
INSERT INTO events (at, project, kind, message) VALUES (?, ?, ?, ?);

-- name: ListEvents :many
SELECT * FROM events
WHERE sqlc.arg(project) = '' OR project = sqlc.arg(project)
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
-- automatic snapshots (not manual) of a project beyond the newest `keep`
SELECT * FROM snapshots
WHERE project = sqlc.arg(project) AND reason != 'manual'
ORDER BY id DESC LIMIT -1 OFFSET sqlc.arg(keep);

-- name: DeleteSnapshot :exec
DELETE FROM snapshots WHERE id = ?;

-- name: ListAudit :many
SELECT * FROM audit_log ORDER BY id DESC LIMIT ?;
