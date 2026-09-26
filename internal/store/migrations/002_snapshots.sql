-- a snapshot is a set of read-only btrfs snapshots of a project's data, taken at the same moment
CREATE TABLE snapshots (
    id INTEGER PRIMARY KEY,
    project TEXT NOT NULL,
    reason TEXT NOT NULL, -- manual | pre-deploy | pre-rollback
    note TEXT NOT NULL DEFAULT '',
    commit_sha TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL
);

CREATE INDEX snapshots_project_idx ON snapshots(project, id);

CREATE TABLE snapshot_volumes (
    snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
    kind TEXT NOT NULL, -- volume | bind
    name TEXT NOT NULL, -- podman volume name, or bind path relative to the repo
    source TEXT NOT NULL, -- live subvolume
    path TEXT NOT NULL, -- read-only snapshot
    PRIMARY KEY (snapshot_id, name)
);
