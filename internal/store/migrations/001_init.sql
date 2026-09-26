CREATE TABLE meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE projects (
    path TEXT PRIMARY KEY,
    disabled BOOLEAN NOT NULL DEFAULT 0,
    commit_sha TEXT NOT NULL DEFAULT '',
    applied_at INTEGER NOT NULL DEFAULT 0
);

-- value is AES-GCM sealed by the store; it never leaves the daemon except into containers
CREATE TABLE env (
    project TEXT NOT NULL,
    key TEXT NOT NULL,
    value BLOB NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (project, key)
);

-- registry users; repos is a json array of exact repository names
CREATE TABLE users (
    name TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL,
    pattern TEXT NOT NULL DEFAULT '',
    repos TEXT NOT NULL DEFAULT '[]',
    created_at INTEGER NOT NULL
);

CREATE TABLE sessions (
    id_hash TEXT PRIMARY KEY,
    expires INTEGER NOT NULL
);

-- what happened, for humans: deploys, pushes, config changes
CREATE TABLE events (
    id INTEGER PRIMARY KEY,
    at INTEGER NOT NULL,
    project TEXT NOT NULL,
    kind TEXT NOT NULL,
    message TEXT NOT NULL
);

CREATE INDEX events_project_idx ON events(project, id);

CREATE TRIGGER events_retention AFTER INSERT ON events
BEGIN
    DELETE FROM events WHERE id <= NEW.id - 5000;
END;
