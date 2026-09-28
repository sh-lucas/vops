-- image rollback: a pin runs a service on a past image (by digest) instead of what compose says, until a newer
-- version arrives (a push of the tag it runs, or another image in compose) or it is unpinned.
CREATE TABLE pins (
    project TEXT NOT NULL,
    service TEXT NOT NULL,
    image TEXT NOT NULL, -- the image as the history recorded it (registry.x/shop/web:v1)
    digest TEXT NOT NULL, -- what runs: image@digest
    compose_image TEXT NOT NULL, -- compose's image when pinned; a different one there drops the pin
    deploy_id INTEGER NOT NULL DEFAULT 0, -- the deploy that ran this image
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project, service)
);

CREATE TRIGGER audit_pins_insert AFTER INSERT ON pins
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('pins', 'insert', NEW.project || '/' || NEW.service,
        NEW.image || '@' || substr(NEW.digest, 8, 12) || ' from #' || NEW.deploy_id || ' (compose: ' || NEW.compose_image || ')');
END;
CREATE TRIGGER audit_pins_update AFTER UPDATE ON pins
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('pins', 'update', NEW.project || '/' || NEW.service,
        NEW.image || '@' || substr(NEW.digest, 8, 12) || ' from #' || NEW.deploy_id || ' (compose: ' || NEW.compose_image || ')');
END;
CREATE TRIGGER audit_pins_delete AFTER DELETE ON pins
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('pins', 'delete', OLD.project || '/' || OLD.service, OLD.image);
END;

-- rollback rows: returned to right before deploy before_id; parts is what it rolled back (images, data, or images,data).
-- Older rollback rows have parts '' and restored data only.
ALTER TABLE deploys ADD COLUMN before_id INTEGER NOT NULL DEFAULT 0;
ALTER TABLE deploys ADD COLUMN parts TEXT NOT NULL DEFAULT '';

DROP TRIGGER audit_deploys_insert;
CREATE TRIGGER audit_deploys_insert AFTER INSERT ON deploys
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('deploys', 'insert', NEW.project || ' #' || NEW.id,
        NEW.trigger || ' commit ' || substr(NEW.commit_sha, 1, 12) || ' ' || NEW.result ||
        CASE WHEN NEW.before_id != 0 THEN ' back to before #' || NEW.before_id || ' (' || NEW.parts || ')' ELSE '' END ||
        CASE WHEN NEW.restored_id != 0 THEN ' restored #' || NEW.restored_id ELSE '' END);
END;
