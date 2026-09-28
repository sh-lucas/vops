// Package store is the sqlite state of the daemon: env vars, registry users, sessions, events, snapshots, previews, deploys,
// project flags and the audit log. SQL lives in queries.sql and migrations/ (sqlc generates queries/);
// this package adds what SQL can't do: encryption, hashing and friendlier types.
// Containers are not stored here; podman labels are the source of truth for them.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sh-lucas/vops/internal/store/migrations"
	"github.com/sh-lucas/vops/internal/store/queries"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
	q   *queries.Queries
	gcm cipher.AEAD
}

// ctx: the store is fast and local; callers don't need to thread contexts through it.
var ctx = context.Background()

// Open opens and migrates the db at path. keyPath holds the AES key for env values; it is created if missing.
func Open(path, keyPath string) (*DB, error) {
	dsn := "file:" + path + "?" + strings.Join([]string{
		"_pragma=foreign_keys(1)", "_pragma=busy_timeout(5000)", "_pragma=journal_mode(WAL)", "_pragma=synchronous(NORMAL)",
	}, "&")
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(1)
	if err := migrations.Run(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	key, err := loadKey(keyPath)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &DB{sql: db, q: queries.New(db), gcm: gcm}, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func loadKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err == nil && len(key) == 32 {
		return key, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, 32)
	rand.Read(key)
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func now() int64 { return time.Now().Unix() }

// Hash is the sha256 hex used for tokens and session ids.
func Hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// Token returns a random hex token.
func Token() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func equalHash(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range len(a) {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// ---- meta

func (d *DB) Meta(key string) string {
	v, _ := d.q.GetMeta(ctx, key)
	return v
}

func (d *DB) SetMeta(key, value string) error {
	return d.q.SetMeta(ctx, queries.SetMetaParams{Key: key, Value: value})
}

// ---- projects

type Project struct {
	Path      string `json:"path"`
	Disabled  bool   `json:"disabled"`
	Commit    string `json:"commit"`
	AppliedAt int64  `json:"applied_at"`
}

func (d *DB) Projects() (map[string]Project, error) {
	rows, err := d.q.ListProjects(ctx)
	out := map[string]Project{}
	for _, r := range rows {
		out[r.Path] = Project{r.Path, r.Disabled, r.CommitSha, r.AppliedAt}
	}
	return out, err
}

func (d *DB) SetDisabled(path string, disabled bool) error {
	return d.q.SetProjectDisabled(ctx, queries.SetProjectDisabledParams{Path: path, Disabled: disabled})
}

func (d *DB) SetApplied(path, commit string) error {
	return d.q.SetProjectApplied(ctx, queries.SetProjectAppliedParams{Path: path, CommitSha: commit, AppliedAt: now()})
}

func (d *DB) DeleteProject(path string) error { return d.q.DeleteProject(ctx, path) }

// ---- env (values are encrypted and never leave the daemon except into containers)

func (d *DB) SetEnv(project, key, value string) error {
	nonce := make([]byte, d.gcm.NonceSize())
	rand.Read(nonce)
	sealed := d.gcm.Seal(nonce, nonce, []byte(value), []byte(project+"\x00"+key))
	return d.q.SetEnv(ctx, queries.SetEnvParams{Project: project, Key: key, Value: sealed, UpdatedAt: now()})
}

func (d *DB) UnsetEnv(project, key string) error {
	return d.q.UnsetEnv(ctx, queries.UnsetEnvParams{Project: project, Key: key})
}

type EnvKey struct {
	Key       string `json:"key"`
	UpdatedAt int64  `json:"updated_at"`
}

// EnvKeys lists the keys of a project, never the values.
func (d *DB) EnvKeys(project string) ([]EnvKey, error) {
	rows, err := d.q.ListEnvKeys(ctx, project)
	out := []EnvKey{}
	for _, r := range rows {
		out = append(out, EnvKey{r.Key, r.UpdatedAt})
	}
	return out, err
}

// Env returns the decrypted env of a project; only the deploy code should call it.
func (d *DB) Env(project string) (map[string]string, error) {
	rows, err := d.q.ListEnv(ctx, project)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	n := d.gcm.NonceSize()
	for _, r := range rows {
		if len(r.Value) < n {
			return nil, fmt.Errorf("env %s/%s: corrupt", project, r.Key)
		}
		plain, err := d.gcm.Open(nil, r.Value[:n], r.Value[n:], []byte(project+"\x00"+r.Key))
		if err != nil {
			return nil, fmt.Errorf("env %s/%s: %w", project, r.Key, err)
		}
		out[r.Key] = string(plain)
	}
	return out, nil
}

// ---- registry users

type User struct {
	Name      string   `json:"name"`
	Pattern   string   `json:"pattern"`
	Repos     []string `json:"repos"`
	CreatedAt int64    `json:"created_at"`
}

func userFrom(r queries.User) User {
	u := User{Name: r.Name, Pattern: r.Pattern, CreatedAt: r.CreatedAt, Repos: []string{}}
	json.Unmarshal([]byte(r.Repos), &u.Repos)
	return u
}

// PutUser creates or updates a user. An empty token keeps the current one.
func (d *DB) PutUser(u User, token string) error {
	if u.Repos == nil {
		u.Repos = []string{}
	}
	repos, _ := json.Marshal(u.Repos)
	if token == "" {
		n, err := d.q.UpdateUserRules(ctx, queries.UpdateUserRulesParams{Pattern: u.Pattern, Repos: string(repos), Name: u.Name})
		if err == nil && n == 0 {
			err = fmt.Errorf("user %q not found", u.Name)
		}
		return err
	}
	return d.q.CreateOrReplaceUser(ctx, queries.CreateOrReplaceUserParams{Name: u.Name, TokenHash: Hash(token), Pattern: u.Pattern, Repos: string(repos), CreatedAt: now()})
}

func (d *DB) DeleteUser(name string) error { return d.q.DeleteUser(ctx, name) }

func (d *DB) Users() ([]User, error) {
	rows, err := d.q.ListUsers(ctx)
	out := []User{}
	for _, r := range rows {
		out = append(out, userFrom(r))
	}
	return out, err
}

// CheckUser returns the user if name/token match.
func (d *DB) CheckUser(name, token string) (User, bool) {
	r, err := d.q.GetUser(ctx, name)
	if err != nil || !equalHash(r.TokenHash, Hash(token)) {
		return User{}, false
	}
	return userFrom(r), true
}

// ---- sessions

func (d *DB) NewSession(ttl time.Duration) (string, error) {
	id := Token()
	d.q.DeleteExpiredSessions(ctx, now())
	return id, d.q.CreateSession(ctx, queries.CreateSessionParams{IDHash: Hash(id), Expires: time.Now().Add(ttl).Unix()})
}

func (d *DB) CheckSession(id string) bool {
	exp, err := d.q.GetSessionExpiry(ctx, Hash(id))
	return err == nil && exp > now()
}

func (d *DB) DeleteSession(id string) { d.q.DeleteSession(ctx, Hash(id)) }

func (d *DB) DeleteSessions() { d.q.DeleteAllSessions(ctx) }

// ---- events (for humans; retention is a trigger)

type Event = queries.Event

func (d *DB) Event(project, kind, format string, args ...any) {
	d.q.CreateEvent(ctx, queries.CreateEventParams{At: now(), Project: project, Kind: kind, Message: fmt.Sprintf(format, args...)})
}

func (d *DB) Events(project string, limit int) ([]Event, error) {
	return d.q.ListEvents(ctx, queries.ListEventsParams{Project: project, Lim: int64(limit)})
}

// ---- audit log (written by triggers only)

type Audit = queries.AuditLog

func (d *DB) Audit(limit int) ([]Audit, error) { return d.q.ListAudit(ctx, int64(limit)) }

// ---- snapshots

type Snapshot struct {
	ID        int64            `json:"id"`
	Project   string           `json:"project"`
	Reason    string           `json:"reason"` // manual | pre-deploy | pre-rollback
	Note      string           `json:"note"`
	Commit    string           `json:"commit"`
	CreatedAt int64            `json:"created_at"`
	Volumes   []SnapshotVolume `json:"volumes"`
}

type SnapshotVolume struct {
	Kind   string `json:"kind"` // volume | bind
	Name   string `json:"name"`
	Source string `json:"source"`
	Path   string `json:"path"`
}

// AddSnapshot records a snapshot and its volumes atomically and returns its id.
func (d *DB) AddSnapshot(s Snapshot) (int64, error) {
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	q := d.q.WithTx(tx)
	id, err := q.CreateSnapshot(ctx, queries.CreateSnapshotParams{Project: s.Project, Reason: s.Reason, Note: s.Note, CommitSha: s.Commit, CreatedAt: now()})
	if err != nil {
		return 0, err
	}
	for _, v := range s.Volumes {
		if err := q.AddSnapshotVolume(ctx, queries.AddSnapshotVolumeParams{SnapshotID: id, Kind: v.Kind, Name: v.Name, Source: v.Source, Path: v.Path}); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit()
}

func (d *DB) snapshotFrom(r queries.Snapshot) (Snapshot, error) {
	s := Snapshot{ID: r.ID, Project: r.Project, Reason: r.Reason, Note: r.Note, Commit: r.CommitSha, CreatedAt: r.CreatedAt, Volumes: []SnapshotVolume{}}
	vols, err := d.q.ListSnapshotVolumes(ctx, r.ID)
	for _, v := range vols {
		s.Volumes = append(s.Volumes, SnapshotVolume{v.Kind, v.Name, v.Source, v.Path})
	}
	return s, err
}

func (d *DB) Snapshot(id int64) (Snapshot, error) {
	r, err := d.q.GetSnapshot(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("no snapshot #%d", id)
	}
	if err != nil {
		return Snapshot{}, err
	}
	return d.snapshotFrom(r)
}

// Snapshots lists snapshots, newest first ("" = all projects).
func (d *DB) Snapshots(project string) ([]Snapshot, error) {
	rows, err := d.q.ListSnapshots(ctx, project)
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, r := range rows {
		s, err := d.snapshotFrom(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// SnapshotsToPrune lists automatic snapshots of a project beyond the newest keep.
func (d *DB) SnapshotsToPrune(project string, keep int) ([]Snapshot, error) {
	rows, err := d.q.ListAutoSnapshotsToPrune(ctx, queries.ListAutoSnapshotsToPruneParams{Project: project, Keep: int64(keep)})
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	for _, r := range rows {
		s, err := d.snapshotFrom(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (d *DB) DeleteSnapshot(id int64) error { return d.q.DeleteSnapshot(ctx, id) }

// ---- admin (dashboard login; can pull every image, push none)

const pbkdf2Iter = 600_000

func (d *DB) SetAdminPassword(password string) error {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, 32)
	if err != nil {
		return err
	}
	d.DeleteSessions()
	return d.SetMeta("admin", hex.EncodeToString(salt)+":"+hex.EncodeToString(key))
}

func (d *DB) HasAdmin() bool { return d.Meta("admin") != "" }

func (d *DB) CheckAdmin(password string) bool {
	salt, key, ok := strings.Cut(d.Meta("admin"), ":")
	if !ok {
		return false
	}
	s, _ := hex.DecodeString(salt)
	got, err := pbkdf2.Key(sha256.New, password, s, pbkdf2Iter, 32)
	return err == nil && equalHash(hex.EncodeToString(got), key)
}

// ---- previews

type Preview struct {
	Project    string            `json:"project"`
	Name       string            `json:"name"`
	Ref        string            `json:"ref"`
	Commit     string            `json:"commit"`
	Images     map[string]string `json:"images"`      // service -> image overrides
	SnapshotID int64             `json:"snapshot_id"` // data copied from this snapshot; 0 = live data
	DeployID   int64             `json:"deploy_id"`   // the timeline node it branched from; 0 = none
	Data       string            `json:"data"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
}

func previewFrom(r queries.Preview) Preview {
	p := Preview{Project: r.Project, Name: r.Name, Ref: r.Ref, Commit: r.CommitSha, Images: map[string]string{}, SnapshotID: r.SnapshotID, DeployID: r.DeployID, Data: r.Data, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	json.Unmarshal([]byte(r.Images), &p.Images)
	return p
}

// PutPreview creates a preview or updates its ref, commit and images; either way it counts as used now.
func (d *DB) PutPreview(p Preview) error {
	if p.Images == nil {
		p.Images = map[string]string{}
	}
	images, _ := json.Marshal(p.Images, json.Deterministic(true))
	t := now()
	return d.q.PutPreview(ctx, queries.PutPreviewParams{Project: p.Project, Name: p.Name, Ref: p.Ref, CommitSha: p.Commit, Images: string(images),
		SnapshotID: p.SnapshotID, DeployID: p.DeployID, Data: p.Data, CreatedAt: t, UpdatedAt: t})
}

// Preview returns a preview; found is false when it doesn't exist.
func (d *DB) Preview(project, name string) (p Preview, found bool, err error) {
	r, err := d.q.GetPreview(ctx, queries.GetPreviewParams{Project: project, Name: name})
	if errors.Is(err, sql.ErrNoRows) {
		return Preview{}, false, nil
	}
	if err != nil {
		return Preview{}, false, err
	}
	return previewFrom(r), true, nil
}

// Previews lists previews ("" = of all projects).
func (d *DB) Previews(project string) ([]Preview, error) {
	rows, err := d.q.ListPreviews(ctx, project)
	out := []Preview{}
	for _, r := range rows {
		out = append(out, previewFrom(r))
	}
	return out, err
}

func (d *DB) DeletePreview(project, name string) error {
	return d.q.DeletePreview(ctx, queries.DeletePreviewParams{Project: project, Name: name})
}

// ---- deploys (history: what ran when, and the data right before it)

type Deploy struct {
	ID         int64                  `json:"id"`
	Project    string                 `json:"project"`
	Commit     string                 `json:"commit"`
	Trigger    string                 `json:"trigger"`     // sync | apply | ui | push | rollback
	Images     map[string]DeployImage `json:"images"`      // service -> what runs after this event
	SnapshotID int64                  `json:"snapshot_id"` // the data right before this event (pre-deploy or pre-rollback); 0 = none
	RestoredID int64                  `json:"restored_id"` // rollback: the snapshot put back
	Undoes     int64                  `json:"undoes"`      // rollback: the rollback it undid
	BeforeID   int64                  `json:"before_id"`   // rollback: it went back to right before this deploy
	Parts      string                 `json:"parts"`       // rollback: images, data or images,data ("" on old rows: data)
	Result     string                 `json:"result"`      // ok | failed
	Error      string                 `json:"error"`
	Summary    string                 `json:"summary"`
	StartedAt  int64                  `json:"started_at"`
	FinishedAt int64                  `json:"finished_at"`
}

type DeployImage struct {
	Image  string `json:"image"`            // the ref from compose (or a preview override)
	Digest string `json:"digest,omitempty"` // manifest digest, when known
	Built  bool   `json:"built,omitempty"`  // built from the repo: the commit reproduces it
}

// Pinned is the image by digest when the digest is known.
func (i DeployImage) Pinned() string {
	if i.Digest == "" {
		return i.Image
	}
	ref := i.Image
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	} else if c := strings.LastIndex(ref, ":"); c > strings.LastIndex(ref, "/") {
		ref = ref[:c]
	}
	return ref + "@" + i.Digest
}

func deployFrom(r queries.Deploy) Deploy {
	d := Deploy{ID: r.ID, Project: r.Project, Commit: r.CommitSha, Trigger: r.Trigger, Images: map[string]DeployImage{}, SnapshotID: r.SnapshotID,
		RestoredID: r.RestoredID, Undoes: r.Undoes, BeforeID: r.BeforeID, Parts: r.Parts, Result: r.Result, Error: r.Error, Summary: r.Summary, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt}
	json.Unmarshal([]byte(r.Images), &d.Images)
	return d
}

// AddDeploy records a finished deploy or rollback and returns its id.
func (d *DB) AddDeploy(x Deploy) (int64, error) {
	if x.Images == nil {
		x.Images = map[string]DeployImage{}
	}
	images, _ := json.Marshal(x.Images, json.Deterministic(true))
	return d.q.CreateDeploy(ctx, queries.CreateDeployParams{Project: x.Project, CommitSha: x.Commit, Trigger: x.Trigger, Images: string(images),
		SnapshotID: x.SnapshotID, RestoredID: x.RestoredID, Undoes: x.Undoes, BeforeID: x.BeforeID, Parts: x.Parts, Result: x.Result, Error: x.Error, Summary: x.Summary,
		StartedAt: x.StartedAt, FinishedAt: x.FinishedAt})
}

// Deploy returns one deploy; found is false when it doesn't exist.
func (d *DB) Deploy(id int64) (x Deploy, found bool, err error) {
	r, err := d.q.GetDeploy(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Deploy{}, false, nil
	}
	if err != nil {
		return Deploy{}, false, err
	}
	return deployFrom(r), true, nil
}

// PreviousDeploy is the deploy of a project right before deploy id; found is false when there is none.
func (d *DB) PreviousDeploy(project string, id int64) (x Deploy, found bool, err error) {
	r, err := d.q.PreviousDeploy(ctx, queries.PreviousDeployParams{Project: project, ID: id})
	if errors.Is(err, sql.ErrNoRows) {
		return Deploy{}, false, nil
	}
	if err != nil {
		return Deploy{}, false, err
	}
	return deployFrom(r), true, nil
}

// Deploys lists the newest limit deploys of a project, newest first.
func (d *DB) Deploys(project string, limit int) ([]Deploy, error) {
	rows, err := d.q.ListDeploys(ctx, queries.ListDeploysParams{Project: project, Limit: int64(limit)})
	out := []Deploy{}
	for _, r := range rows {
		out = append(out, deployFrom(r))
	}
	return out, err
}

// LatestDeploys is the newest deploy of every project.
func (d *DB) LatestDeploys() (map[string]Deploy, error) {
	rows, err := d.q.LatestDeploys(ctx)
	out := map[string]Deploy{}
	for _, r := range rows {
		out[r.Project] = deployFrom(r)
	}
	return out, err
}

// RecentDeployImages lists the images of the newest keep deploys of every project.
func (d *DB) RecentDeployImages(keep int) ([]DeployImage, error) {
	rows, err := d.q.RecentDeployImages(ctx, int64(keep))
	var out []DeployImage
	for _, r := range rows {
		var m map[string]DeployImage
		json.Unmarshal([]byte(r), &m)
		for _, img := range m {
			out = append(out, img)
		}
	}
	return out, err
}

// ---- pins (image rollback: a service runs a past image until a newer version arrives)

type Pin = queries.Pin

// PinRef is what a pinned service runs: its image by digest.
func PinRef(p Pin) string { return DeployImage{Image: p.Image, Digest: p.Digest}.Pinned() }

func (d *DB) PutPin(p Pin) error {
	return d.q.PutPin(ctx, queries.PutPinParams{Project: p.Project, Service: p.Service, Image: p.Image, Digest: p.Digest,
		ComposeImage: p.ComposeImage, DeployID: p.DeployID, CreatedAt: now()})
}

// Pins lists pins ("" = of all projects).
func (d *DB) Pins(project string) ([]Pin, error) { return d.q.ListPins(ctx, project) }

// DeletePin removes a pin and reports whether there was one.
func (d *DB) DeletePin(project, service string) (bool, error) {
	n, err := d.q.DeletePin(ctx, queries.DeletePinParams{Project: project, Service: service})
	return n > 0, err
}
