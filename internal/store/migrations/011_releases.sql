-- release modes: a registry push to a project with x-vops.release manual or canary waits here for an admin.
-- The services keep what they run (see decisions.md, "Releases"); the newest row per project and repo:tag decides.
CREATE TABLE releases (
    id INTEGER PRIMARY KEY,
    project TEXT NOT NULL,
    repo TEXT NOT NULL,
    tag TEXT NOT NULL,
    digest TEXT NOT NULL, -- what was pushed and waits
    mode TEXT NOT NULL CHECK (mode IN ('manual', 'canary')),
    weight INTEGER NOT NULL DEFAULT 0, -- canary: % of requests
    pushed_by TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'approved', 'rejected', 'superseded', 'failed')),
    approve_at INTEGER NOT NULL DEFAULT 0, -- a scheduled approval (unix time); 0 = none
    decided_by TEXT NOT NULL DEFAULT '', -- who approved (or scheduled it), rejected, or "vops" (a failed canary)
    decided_at INTEGER NOT NULL DEFAULT 0,
    note TEXT NOT NULL DEFAULT '' -- why it was rejected, superseded or failed
);
CREATE INDEX releases_project_idx ON releases(project, repo, tag, id);
CREATE UNIQUE INDEX releases_one_pending ON releases(project, repo, tag) WHERE status = 'pending';

CREATE TRIGGER releases_retention AFTER INSERT ON releases
BEGIN
    DELETE FROM releases WHERE id <= NEW.id - 2000 AND status != 'pending';
END;

CREATE TRIGGER audit_releases_insert AFTER INSERT ON releases
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('releases', 'insert', NEW.project || ' #' || NEW.id,
        NEW.repo || ':' || NEW.tag || '@' || substr(NEW.digest, 8, 12) || ' ' || NEW.mode || ' by ' || NEW.pushed_by);
END;
CREATE TRIGGER audit_releases_update AFTER UPDATE ON releases
WHEN OLD.status IS NOT NEW.status OR OLD.approve_at IS NOT NEW.approve_at
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('releases', 'update', NEW.project || ' #' || NEW.id,
        NEW.status || CASE WHEN NEW.approve_at != 0 AND NEW.status = 'pending' THEN ' approve at ' || NEW.approve_at ELSE '' END ||
        CASE WHEN NEW.decided_by != '' THEN ' by ' || NEW.decided_by ELSE '' END || CASE WHEN NEW.note != '' THEN ': ' || NEW.note ELSE '' END);
END;
CREATE TRIGGER audit_releases_delete AFTER DELETE ON releases
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('releases', 'delete', OLD.project || ' #' || OLD.id);
END;
