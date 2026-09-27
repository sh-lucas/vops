-- a preview is project <project>@<name>: its code is a git worktree at commit_sha, its data a copy-on-write copy
-- of the project's data (live or from snapshot_id), its images the project's with overrides (json service -> image)
CREATE TABLE previews (
    project TEXT NOT NULL,
    name TEXT NOT NULL,
    ref TEXT NOT NULL DEFAULT '', -- git ref asked for; '' = the project's applied commit at creation
    commit_sha TEXT NOT NULL DEFAULT '',
    images TEXT NOT NULL DEFAULT '{}',
    snapshot_id INTEGER NOT NULL DEFAULT 0, -- 0 = copied from the live data
    data TEXT NOT NULL DEFAULT '', -- where the data came from, for humans
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL, -- last up (cli, api or registry push); the ttl counts from here
    PRIMARY KEY (project, name)
);

CREATE TRIGGER audit_previews_insert AFTER INSERT ON previews
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('previews', 'insert', NEW.project || '@' || NEW.name,
        'commit ' || substr(NEW.commit_sha, 1, 12) || ' images=' || NEW.images || ' data: ' || NEW.data);
END;
CREATE TRIGGER audit_previews_update AFTER UPDATE ON previews
WHEN OLD.commit_sha IS NOT NEW.commit_sha OR OLD.images IS NOT NEW.images
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('previews', 'update', NEW.project || '@' || NEW.name,
        'commit ' || substr(NEW.commit_sha, 1, 12) || ' images=' || NEW.images);
END;
CREATE TRIGGER audit_previews_delete AFTER DELETE ON previews
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('previews', 'delete', OLD.project || '@' || OLD.name);
END;
