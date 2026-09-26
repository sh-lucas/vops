-- every write to the state, recorded by triggers so no code path can forget it.
-- secrets never land here: env values, token hashes and the admin password hash are left out.
CREATE TABLE audit_log (
    id INTEGER PRIMARY KEY,
    at INTEGER NOT NULL DEFAULT (CAST(strftime('%s', 'now') AS INTEGER)),
    tbl TEXT NOT NULL,
    op TEXT NOT NULL, -- insert | update | delete
    key TEXT NOT NULL,
    detail TEXT NOT NULL DEFAULT ''
);

CREATE TRIGGER audit_retention AFTER INSERT ON audit_log
BEGIN
    DELETE FROM audit_log WHERE id <= NEW.id - 20000;
END;

CREATE TRIGGER audit_projects_insert AFTER INSERT ON projects
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('projects', 'insert', NEW.path,
        CASE WHEN NEW.disabled THEN 'disabled' ELSE 'commit ' || substr(NEW.commit_sha, 1, 12) END);
END;
CREATE TRIGGER audit_projects_update AFTER UPDATE ON projects
WHEN OLD.disabled IS NOT NEW.disabled OR OLD.commit_sha IS NOT NEW.commit_sha
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('projects', 'update', NEW.path, trim(
        CASE WHEN OLD.disabled IS NOT NEW.disabled THEN CASE WHEN NEW.disabled THEN 'disabled ' ELSE 'enabled ' END ELSE '' END ||
        CASE WHEN OLD.commit_sha IS NOT NEW.commit_sha THEN 'commit ' || substr(OLD.commit_sha, 1, 12) || ' -> ' || substr(NEW.commit_sha, 1, 12) ELSE '' END));
END;
CREATE TRIGGER audit_projects_delete AFTER DELETE ON projects
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('projects', 'delete', OLD.path);
END;

CREATE TRIGGER audit_env_insert AFTER INSERT ON env
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('env', 'insert', NEW.project || ' ' || NEW.key);
END;
CREATE TRIGGER audit_env_update AFTER UPDATE ON env
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('env', 'update', NEW.project || ' ' || NEW.key, 'value changed');
END;
CREATE TRIGGER audit_env_delete AFTER DELETE ON env
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('env', 'delete', OLD.project || ' ' || OLD.key);
END;

CREATE TRIGGER audit_users_insert AFTER INSERT ON users
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('users', 'insert', NEW.name, 'pattern=' || NEW.pattern || ' repos=' || NEW.repos);
END;
CREATE TRIGGER audit_users_update AFTER UPDATE ON users
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('users', 'update', NEW.name, trim(
        CASE WHEN OLD.token_hash IS NOT NEW.token_hash THEN 'new token ' ELSE '' END ||
        'pattern=' || NEW.pattern || ' repos=' || NEW.repos));
END;
CREATE TRIGGER audit_users_delete AFTER DELETE ON users
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('users', 'delete', OLD.name);
END;

CREATE TRIGGER audit_meta_insert AFTER INSERT ON meta
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('meta', 'insert', NEW.key);
END;
CREATE TRIGGER audit_meta_update AFTER UPDATE ON meta
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('meta', 'update', NEW.key, 'value changed');
END;

CREATE TRIGGER audit_sessions_insert AFTER INSERT ON sessions
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('sessions', 'insert', substr(NEW.id_hash, 1, 8), 'login');
END;
CREATE TRIGGER audit_sessions_delete AFTER DELETE ON sessions
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('sessions', 'delete', substr(OLD.id_hash, 1, 8),
        CASE WHEN OLD.expires < CAST(strftime('%s', 'now') AS INTEGER) THEN 'expired' ELSE 'logout' END);
END;

CREATE TRIGGER audit_snapshots_insert AFTER INSERT ON snapshots
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('snapshots', 'insert', NEW.project || ' #' || NEW.id, NEW.reason);
END;
CREATE TRIGGER audit_snapshots_delete AFTER DELETE ON snapshots
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('snapshots', 'delete', OLD.project || ' #' || OLD.id, OLD.reason);
END;
