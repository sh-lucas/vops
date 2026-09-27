-- deploy history: one row per project per apply that changed it, and per rollback.
-- snapshot_id is always the data right before this event (pre-deploy, or pre-rollback = the undo point); 0 = none.
CREATE TABLE deploys (
    id INTEGER PRIMARY KEY,
    project TEXT NOT NULL,
    commit_sha TEXT NOT NULL DEFAULT '',
    trigger TEXT NOT NULL, -- sync | apply | ui | push | rollback
    images TEXT NOT NULL DEFAULT '{}', -- json service -> {image, digest, built}: what runs after this event
    snapshot_id INTEGER NOT NULL DEFAULT 0,
    restored_id INTEGER NOT NULL DEFAULT 0, -- rollback: the snapshot put back
    undoes INTEGER NOT NULL DEFAULT 0, -- rollback: the rollback (deploy id) this one undid
    result TEXT NOT NULL, -- ok | failed
    error TEXT NOT NULL DEFAULT '',
    summary TEXT NOT NULL DEFAULT '',
    started_at INTEGER NOT NULL,
    finished_at INTEGER NOT NULL
);

CREATE INDEX deploys_project_idx ON deploys(project, id);

CREATE TRIGGER deploys_retention AFTER INSERT ON deploys
BEGIN
    DELETE FROM deploys WHERE id <= NEW.id - 5000;
END;

CREATE TRIGGER audit_deploys_insert AFTER INSERT ON deploys
BEGIN
    INSERT INTO audit_log(tbl, op, key, detail) VALUES ('deploys', 'insert', NEW.project || ' #' || NEW.id,
        NEW.trigger || ' commit ' || substr(NEW.commit_sha, 1, 12) || ' ' || NEW.result ||
        CASE WHEN NEW.restored_id != 0 THEN ' restored #' || NEW.restored_id ELSE '' END);
END;
CREATE TRIGGER audit_deploys_delete AFTER DELETE ON deploys
BEGIN
    INSERT INTO audit_log(tbl, op, key) VALUES ('deploys', 'delete', OLD.project || ' #' || OLD.id);
END;

-- where a preview branched from: the timeline node (deploy) it was made from; 0 = none (see snapshot_id)
ALTER TABLE previews ADD COLUMN deploy_id INTEGER NOT NULL DEFAULT 0;
