-- why vops must not start or deploy a project ('' = it may): a data rollback that left its data mixed sets it,
-- a successful restore or vops restart clears it. Boot (StartStopped) would otherwise start it on that data.
ALTER TABLE projects ADD COLUMN held TEXT NOT NULL DEFAULT '';

DROP TRIGGER audit_projects_update;
CREATE TRIGGER audit_projects_update AFTER UPDATE ON projects
WHEN OLD.disabled IS NOT NEW.disabled OR OLD.commit_sha IS NOT NEW.commit_sha OR OLD.limits IS NOT NEW.limits OR OLD.held IS NOT NEW.held
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('projects', 'update', NEW.path, trim(
        CASE WHEN OLD.disabled IS NOT NEW.disabled THEN CASE WHEN NEW.disabled THEN 'disabled ' ELSE 'enabled ' END ELSE '' END ||
        CASE WHEN OLD.commit_sha IS NOT NEW.commit_sha THEN 'commit ' || substr(OLD.commit_sha, 1, 12) || ' -> ' || substr(NEW.commit_sha, 1, 12) || ' ' ELSE '' END ||
        CASE WHEN OLD.limits IS NOT NEW.limits THEN 'limits ' || CASE WHEN NEW.limits = '' THEN 'none' ELSE NEW.limits END || ' ' ELSE '' END ||
        CASE WHEN OLD.held IS NOT NEW.held THEN CASE WHEN NEW.held = '' THEN 'not held' ELSE 'held' END ELSE '' END));
END;
