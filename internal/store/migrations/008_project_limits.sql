-- the proxy's request limits of a project as last applied (json proxy.Limits; '' = unlimited), from compose's
-- top-level x-vops. The routing table takes them from here, so changing them never recreates containers.
ALTER TABLE projects ADD COLUMN limits TEXT NOT NULL DEFAULT '';

DROP TRIGGER audit_projects_update;
CREATE TRIGGER audit_projects_update AFTER UPDATE ON projects
WHEN OLD.disabled IS NOT NEW.disabled OR OLD.commit_sha IS NOT NEW.commit_sha OR OLD.limits IS NOT NEW.limits
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('projects', 'update', NEW.path, trim(
        CASE WHEN OLD.disabled IS NOT NEW.disabled THEN CASE WHEN NEW.disabled THEN 'disabled ' ELSE 'enabled ' END ELSE '' END ||
        CASE WHEN OLD.commit_sha IS NOT NEW.commit_sha THEN 'commit ' || substr(OLD.commit_sha, 1, 12) || ' -> ' || substr(NEW.commit_sha, 1, 12) || ' ' ELSE '' END ||
        CASE WHEN OLD.limits IS NOT NEW.limits THEN 'limits ' || CASE WHEN NEW.limits = '' THEN 'none' ELSE NEW.limits END ELSE '' END));
END;
