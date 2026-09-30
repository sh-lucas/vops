-- users are admins (dashboard + push/pull every repo) or deployers (registry only: every repo when global, else
-- the repos in user_repos). secret: a pbkdf2 "salt:key" password for admins, the sha256 of a generated token for
-- deployers. The regex pattern is gone: '.*'/'.+' become global, json repos become user_repos rows, any other
-- pattern is dropped with an event. The dashboard admin (meta 'admin') becomes user admin. Sessions start over.
DROP TRIGGER audit_users_insert;
DROP TRIGGER audit_users_update;
DROP TRIGGER audit_users_delete;
ALTER TABLE users RENAME TO users_old;

CREATE TABLE users (
    name TEXT PRIMARY KEY CHECK (name != '' AND name != 'vops-internal'),
    role TEXT NOT NULL CHECK (role IN ('admin', 'deployer')),
    global BOOLEAN NOT NULL DEFAULT 0 CHECK (global IN (0, 1)),
    secret TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    CHECK (role != 'admin' OR global = 1),
    CHECK ((role = 'admin') = (instr(secret, ':') > 0)) -- admins have a password, deployers a token
);

CREATE TABLE user_repos (
    user TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE ON UPDATE CASCADE,
    repo TEXT NOT NULL CHECK (repo != '' AND instr(repo, ':') = 0),
    PRIMARY KEY (user, repo)
);

INSERT INTO users (name, role, global, secret, created_at)
SELECT name, 'deployer', pattern IN ('.*', '.+'), token_hash, created_at FROM users_old;

INSERT OR IGNORE INTO user_repos (user, repo)
SELECT u.name, trim(CASE WHEN instr(j.value, ':') > 0 THEN substr(j.value, 1, instr(j.value, ':') - 1) ELSE j.value END)
FROM users_old u, json_each(u.repos) j
WHERE json_valid(u.repos) AND u.pattern NOT IN ('.*', '.+') AND j.type = 'text'
  AND trim(CASE WHEN instr(j.value, ':') > 0 THEN substr(j.value, 1, instr(j.value, ':') - 1) ELSE j.value END) != '';

INSERT INTO events (at, project, kind, message)
SELECT CAST(strftime('%s', 'now') AS INTEGER), '', 'auth',
    'registry user ' || name || ' lost its pattern ' || pattern || ' (patterns are gone): give it repos with `vops user add ' || name || ' --repo <repo>` or --global'
FROM users_old WHERE pattern NOT IN ('', '.*', '.+');

INSERT INTO users (name, role, global, secret, created_at)
SELECT 'admin', 'admin', 1, value, CAST(strftime('%s', 'now') AS INTEGER) FROM meta WHERE key = 'admin' AND instr(value, ':') > 0;
DELETE FROM meta WHERE key = 'admin';

DROP TABLE users_old;

-- sessions belong to a user: deleting or demoting the user, or changing their secret, logs them out
INSERT INTO audit_log(tbl, op, key, detail) SELECT 'sessions', 'delete', '*', 'users migration: all sessions ended' WHERE EXISTS (SELECT 1 FROM sessions);
DROP TABLE sessions;
CREATE TABLE sessions (
    id_hash TEXT PRIMARY KEY,
    user TEXT NOT NULL REFERENCES users(name) ON DELETE CASCADE ON UPDATE CASCADE,
    expires INTEGER NOT NULL
);
CREATE INDEX sessions_user_idx ON sessions(user);
CREATE INDEX user_repos_user_idx ON user_repos(user);

CREATE TRIGGER audit_sessions_insert AFTER INSERT ON sessions
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('sessions', 'insert', substr(NEW.id_hash, 1, 8), 'login ' || NEW.user);
END;
CREATE TRIGGER audit_sessions_delete AFTER DELETE ON sessions
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('sessions', 'delete', substr(OLD.id_hash, 1, 8),
        CASE WHEN OLD.expires < CAST(strftime('%s', 'now') AS INTEGER) THEN 'expired ' ELSE 'logout ' END || OLD.user);
END;

-- consistency: there is always an admin once there was one; global users have no repo list
CREATE TRIGGER users_keep_last_admin_delete BEFORE DELETE ON users
WHEN OLD.role = 'admin' AND (SELECT count(*) FROM users WHERE role = 'admin') = 1
BEGIN
    SELECT RAISE(ABORT, 'the last admin can''t be deleted');
END;
CREATE TRIGGER users_keep_last_admin_demote BEFORE UPDATE OF role ON users
WHEN OLD.role = 'admin' AND NEW.role != 'admin' AND (SELECT count(*) FROM users WHERE role = 'admin') = 1
BEGIN
    SELECT RAISE(ABORT, 'the last admin can''t be demoted');
END;
CREATE TRIGGER users_logout AFTER UPDATE ON users
WHEN (OLD.role = 'admin' AND NEW.role != 'admin') OR OLD.secret IS NOT NEW.secret
BEGIN
    DELETE FROM sessions WHERE user = NEW.name;
END;
CREATE TRIGGER users_global_drops_repos AFTER UPDATE OF global ON users
WHEN NEW.global = 1
BEGIN
    DELETE FROM user_repos WHERE user = NEW.name;
END;
CREATE TRIGGER user_repos_not_global BEFORE INSERT ON user_repos
WHEN (SELECT global FROM users WHERE name = NEW.user) = 1
BEGIN
    SELECT RAISE(ABORT, 'a global user has no repo list');
END;

-- audit: never the secret, only that it changed
CREATE TRIGGER audit_users_insert AFTER INSERT ON users
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('users', 'insert', NEW.name, 'role=' || NEW.role || CASE WHEN NEW.global THEN ' global' ELSE '' END);
END;
CREATE TRIGGER audit_users_update AFTER UPDATE ON users
WHEN OLD.role IS NOT NEW.role OR OLD.global IS NOT NEW.global OR OLD.secret IS NOT NEW.secret OR OLD.name IS NOT NEW.name
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('users', 'update', NEW.name, trim(
        CASE WHEN OLD.name IS NOT NEW.name THEN 'renamed from ' || OLD.name || ' ' ELSE '' END ||
        CASE WHEN OLD.secret IS NOT NEW.secret THEN CASE WHEN NEW.role = 'admin' THEN 'new password ' ELSE 'new token ' END ELSE '' END ||
        CASE WHEN OLD.role IS NOT NEW.role THEN 'role ' || OLD.role || ' -> ' || NEW.role || ' ' ELSE '' END ||
        CASE WHEN OLD.global IS NOT NEW.global THEN CASE WHEN NEW.global THEN 'global' ELSE 'not global' END ELSE '' END));
END;
CREATE TRIGGER audit_users_delete AFTER DELETE ON users
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('users', 'delete', OLD.name);
END;
CREATE TRIGGER audit_user_repos_insert AFTER INSERT ON user_repos
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('user_repos', 'insert', NEW.user, NEW.repo);
END;
CREATE TRIGGER audit_user_repos_delete AFTER DELETE ON user_repos
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('user_repos', 'delete', OLD.user, OLD.repo);
END;
