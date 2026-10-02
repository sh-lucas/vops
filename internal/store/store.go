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
	"slices"
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
	Limits    string `json:"limits,omitempty"` // proxy limits as applied (json proxy.Limits), '' = none
}

func (d *DB) Projects() (map[string]Project, error) {
	rows, err := d.q.ListProjects(ctx)
	out := map[string]Project{}
	for _, r := range rows {
		out[r.Path] = Project{r.Path, r.Disabled, r.CommitSha, r.AppliedAt, r.Limits}
	}
	return out, err
}

func (d *DB) SetDisabled(path string, disabled bool) error {
	return d.q.SetProjectDisabled(ctx, queries.SetProjectDisabledParams{Path: path, Disabled: disabled})
}

func (d *DB) SetApplied(path, commit string) error {
	return d.q.SetProjectApplied(ctx, queries.SetProjectAppliedParams{Path: path, CommitSha: commit, AppliedAt: now()})
}

// SetLimits records a project's applied proxy limits (json, ” = none).
func (d *DB) SetLimits(path, limits string) error {
	return d.q.SetProjectLimits(ctx, queries.SetProjectLimitsParams{Path: path, Limits: limits})
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

// ---- users: admins (dashboard, every repo) and deployers (registry only: every repo, or a list)

const (
	Admin    = "admin"
	Deployer = "deployer"
)

type User struct {
	Name      string   `json:"name"`
	Role      string   `json:"role"`   // admin | deployer
	Global    bool     `json:"global"` // push/pull every repo (always true for admins)
	Repos     []string `json:"repos"`  // otherwise only these
	CreatedAt int64    `json:"created_at"`
}

// Allows reports whether the user may push and pull repo (the same permission).
func (u User) Allows(repo string) bool { return u.Global || slices.Contains(u.Repos, repo) }

// NormalizeRepos trims names, drops what follows a ':' (shop/web:latest is shop/web), empties and duplicates.
func NormalizeRepos(repos []string) []string {
	out := []string{}
	for _, r := range repos {
		r, _, _ = strings.Cut(r, ":")
		if r = strings.TrimSpace(r); r != "" && !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return out
}

// dbErr turns a trigger's RAISE into its message.
func dbErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	for _, m := range []string{"the last admin can't be deleted", "the last admin can't be demoted", "a global user has no repo list"} {
		if strings.Contains(msg, m) {
			return errors.New(m)
		}
	}
	return err
}

// secretHash is how a secret is stored: pbkdf2 for admin passwords (typed by people), sha256 for generated tokens.
func secretHash(role, secret string) (string, error) {
	if role != Admin {
		return Hash(secret), nil
	}
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, secret, salt, pbkdf2Iter, 32)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(key), nil
}

func checkSecret(stored, secret string) bool {
	salt, key, ok := strings.Cut(stored, ":")
	if !ok {
		return equalHash(stored, Hash(secret))
	}
	s, _ := hex.DecodeString(salt)
	got, err := pbkdf2.Key(sha256.New, secret, s, pbkdf2Iter, 32)
	return err == nil && equalHash(hex.EncodeToString(got), key)
}

// PutUser creates or updates a user and its repo list. secret is an admin's password or a deployer's token;
// empty keeps the current one. The db enforces the rules (admins are global and have a password, the last
// admin stays, global users have no list) and logs the user out on a demotion or a new secret.
func (d *DB) PutUser(u User, secret string) error {
	if u.Role == Admin {
		u.Global = true
	}
	repos := NormalizeRepos(u.Repos)
	if u.Global {
		repos = nil
	}
	hash := ""
	if secret != "" {
		var err error
		if hash, err = secretHash(u.Role, secret); err != nil {
			return err
		}
	}
	tx, err := d.sql.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := d.q.WithTx(tx)
	old, err := q.GetUser(ctx, u.Name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if hash == "" {
			return fmt.Errorf("user %q needs a password or a token", u.Name)
		}
		err = q.CreateUser(ctx, queries.CreateUserParams{Name: u.Name, Role: u.Role, Global: u.Global, Secret: hash, CreatedAt: now()})
	case err != nil:
		return err
	default:
		if hash == "" && old.Role != u.Role {
			return fmt.Errorf("user %q changes role: it needs a new %s", u.Name, map[bool]string{true: "password", false: "token"}[u.Role == Admin])
		}
		_, err = q.UpdateUser(ctx, queries.UpdateUserParams{Role: u.Role, Global: u.Global, Secret: hash, Name: u.Name})
	}
	if err != nil {
		return dbErr(err)
	}
	if err := q.DeleteUserRepos(ctx, u.Name); err != nil {
		return err
	}
	for _, r := range repos {
		if err := q.AddUserRepo(ctx, queries.AddUserRepoParams{User: u.Name, Repo: r}); err != nil {
			return dbErr(err)
		}
	}
	return tx.Commit()
}

// DeleteUser removes a user, its repos and its sessions (cascade).
func (d *DB) DeleteUser(name string) error {
	n, err := d.q.DeleteUser(ctx, name)
	if err == nil && n == 0 {
		err = fmt.Errorf("no user %q", name)
	}
	return dbErr(err)
}

func (d *DB) Users() ([]User, error) {
	rows, err := d.q.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	repos, err := d.q.ListUserRepos(ctx)
	if err != nil {
		return nil, err
	}
	out := []User{}
	for _, r := range rows {
		u := User{Name: r.Name, Role: r.Role, Global: r.Global, Repos: []string{}, CreatedAt: r.CreatedAt}
		for _, ur := range repos {
			if ur.User == r.Name {
				u.Repos = append(u.Repos, ur.Repo)
			}
		}
		out = append(out, u)
	}
	return out, nil
}

// User returns one user; found is false when it doesn't exist.
func (d *DB) User(name string) (u User, found bool, err error) {
	r, err := d.q.GetUser(ctx, name)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, false, nil
	}
	if err != nil {
		return User{}, false, err
	}
	repos, err := d.q.ListReposOfUser(ctx, name)
	return User{Name: r.Name, Role: r.Role, Global: r.Global, Repos: repos, CreatedAt: r.CreatedAt}, true, err
}

// CheckUser returns the user if name and secret (password or token) match.
func (d *DB) CheckUser(name, secret string) (User, bool) {
	r, err := d.q.GetUser(ctx, name)
	if err != nil || !checkSecret(r.Secret, secret) {
		return User{}, false
	}
	u, found, err := d.User(name)
	return u, found && err == nil
}

// HasAdmin reports whether someone can log into the dashboard.
func (d *DB) HasAdmin() bool {
	n, err := d.q.CountAdmins(ctx)
	return err == nil && n > 0
}

// ---- sessions (a session belongs to a user; the db drops them when the user goes, is demoted or gets a new secret)

func (d *DB) NewSession(user string, ttl time.Duration) (string, error) {
	id := Token()
	d.q.DeleteExpiredSessions(ctx, now())
	return id, d.q.CreateSession(ctx, queries.CreateSessionParams{IDHash: Hash(id), User: user, Expires: time.Now().Add(ttl).Unix()})
}

// CheckSession returns the user of a live session.
func (d *DB) CheckSession(id string) (string, bool) {
	s, err := d.q.GetSession(ctx, Hash(id))
	if err != nil || s.Expires <= now() {
		return "", false
	}
	return s.User, true
}

func (d *DB) DeleteSession(id string) { d.q.DeleteSession(ctx, Hash(id)) }

// PurgeSessions deletes expired sessions and returns how many (the housekeeping tick).
func (d *DB) PurgeSessions() (int64, error) { return d.q.DeleteExpiredSessions(ctx, now()) }

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

// pbkdf2 iterations for admin passwords
const pbkdf2Iter = 600_000

// ---- previews

type Preview struct {
	Project    string            `json:"project"`
	Name       string            `json:"name"`
	Ref        string            `json:"ref"`
	Commit     string            `json:"commit"`
	Images     map[string]string `json:"images"`      // service -> image overrides
	Services   []string          `json:"services"`    // what the preview is for (pushes, --image); empty = every service with x-vops.preview
	SnapshotID int64             `json:"snapshot_id"` // data copied from this snapshot; 0 = live data
	DeployID   int64             `json:"deploy_id"`   // the timeline node it branched from; 0 = none
	Data       string            `json:"data"`
	CreatedAt  int64             `json:"created_at"`
	UpdatedAt  int64             `json:"updated_at"`
}

func previewFrom(r queries.Preview) Preview {
	p := Preview{Project: r.Project, Name: r.Name, Ref: r.Ref, Commit: r.CommitSha, Images: map[string]string{}, Services: []string{}, SnapshotID: r.SnapshotID, DeployID: r.DeployID, Data: r.Data, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	json.Unmarshal([]byte(r.Images), &p.Images)
	json.Unmarshal([]byte(r.Services), &p.Services)
	return p
}

// PutPreview creates a preview or updates its ref, commit and images; either way it counts as used now.
func (d *DB) PutPreview(p Preview) error {
	if p.Images == nil {
		p.Images = map[string]string{}
	}
	if p.Services == nil {
		p.Services = []string{}
	}
	images, _ := json.Marshal(p.Images, json.Deterministic(true))
	services, _ := json.Marshal(p.Services)
	t := now()
	return d.q.PutPreview(ctx, queries.PutPreviewParams{Project: p.Project, Name: p.Name, Ref: p.Ref, CommitSha: p.Commit, Images: string(images), Services: string(services),
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
