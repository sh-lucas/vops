// Package store is the sqlite state of the daemon: env vars, registry users, sessions, events and project flags.
// Containers are not stored here; podman labels are the source of truth for them.
package store

import (
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

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
	gcm cipher.AEAD
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS projects(path TEXT PRIMARY KEY, disabled INTEGER NOT NULL DEFAULT 0, commit_sha TEXT NOT NULL DEFAULT '', applied_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS env(project TEXT NOT NULL, key TEXT NOT NULL, value BLOB NOT NULL, updated_at INTEGER NOT NULL, PRIMARY KEY(project, key));
CREATE TABLE IF NOT EXISTS users(name TEXT PRIMARY KEY, token_hash TEXT NOT NULL, pattern TEXT NOT NULL DEFAULT '', repos TEXT NOT NULL DEFAULT '[]', created_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS sessions(id_hash TEXT PRIMARY KEY, expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY, at INTEGER NOT NULL, project TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL);
`

// Open opens (and migrates) the db at path. keyPath holds the AES key for env values; it is created if missing.
func Open(path, keyPath string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
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
	return &DB{sql: db, gcm: gcm}, nil
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

// Token returns a random url-safe token.
func Token() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ---- meta

func (d *DB) Meta(key string) string {
	var v string
	d.sql.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	return v
}

func (d *DB) SetMeta(key, value string) error {
	_, err := d.sql.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---- projects

type Project struct {
	Path      string `json:"path"`
	Disabled  bool   `json:"disabled"`
	Commit    string `json:"commit"`
	AppliedAt int64  `json:"applied_at"`
}

func (d *DB) Projects() (map[string]Project, error) {
	rows, err := d.sql.Query(`SELECT path, disabled, commit_sha, applied_at FROM projects`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.Path, &p.Disabled, &p.Commit, &p.AppliedAt); err != nil {
			return nil, err
		}
		out[p.Path] = p
	}
	return out, rows.Err()
}

func (d *DB) SetDisabled(path string, disabled bool) error {
	_, err := d.sql.Exec(`INSERT INTO projects(path, disabled) VALUES(?,?) ON CONFLICT(path) DO UPDATE SET disabled=excluded.disabled`, path, disabled)
	return err
}

func (d *DB) SetApplied(path, commit string) error {
	_, err := d.sql.Exec(`INSERT INTO projects(path, commit_sha, applied_at) VALUES(?,?,?) ON CONFLICT(path) DO UPDATE SET commit_sha=excluded.commit_sha, applied_at=excluded.applied_at`, path, commit, now())
	return err
}

func (d *DB) DeleteProject(path string) error {
	_, err := d.sql.Exec(`DELETE FROM projects WHERE path=?`, path)
	return err
}

// ---- env (values are encrypted and never leave the daemon except into containers)

func (d *DB) SetEnv(project, key, value string) error {
	nonce := make([]byte, d.gcm.NonceSize())
	rand.Read(nonce)
	sealed := d.gcm.Seal(nonce, nonce, []byte(value), []byte(project+"\x00"+key))
	_, err := d.sql.Exec(`INSERT INTO env(project,key,value,updated_at) VALUES(?,?,?,?) ON CONFLICT(project,key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, project, key, sealed, now())
	return err
}

func (d *DB) UnsetEnv(project, key string) error {
	_, err := d.sql.Exec(`DELETE FROM env WHERE project=? AND key=?`, project, key)
	return err
}

type EnvKey struct {
	Key       string `json:"key"`
	UpdatedAt int64  `json:"updated_at"`
}

// EnvKeys lists the keys of a project, never the values.
func (d *DB) EnvKeys(project string) ([]EnvKey, error) {
	rows, err := d.sql.Query(`SELECT key, updated_at FROM env WHERE project=? ORDER BY key`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnvKey{}
	for rows.Next() {
		var k EnvKey
		if err := rows.Scan(&k.Key, &k.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// Env returns the decrypted env of a project; only the deploy code should call it.
func (d *DB) Env(project string) (map[string]string, error) {
	rows, err := d.sql.Query(`SELECT key, value FROM env WHERE project=?`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k string
		var sealed []byte
		if err := rows.Scan(&k, &sealed); err != nil {
			return nil, err
		}
		n := d.gcm.NonceSize()
		if len(sealed) < n {
			return nil, fmt.Errorf("env %s/%s: corrupt", project, k)
		}
		plain, err := d.gcm.Open(nil, sealed[:n], sealed[n:], []byte(project+"\x00"+k))
		if err != nil {
			return nil, fmt.Errorf("env %s/%s: %w", project, k, err)
		}
		out[k] = string(plain)
	}
	return out, rows.Err()
}

// ---- registry users

type User struct {
	Name      string   `json:"name"`
	Pattern   string   `json:"pattern"`
	Repos     []string `json:"repos"`
	CreatedAt int64    `json:"created_at"`
	tokenHash string
}

// PutUser creates or updates a user. An empty token keeps the current one.
func (d *DB) PutUser(u User, token string) error {
	repos, _ := json.Marshal(u.Repos)
	if u.Repos == nil {
		repos = []byte("[]")
	}
	if token == "" {
		res, err := d.sql.Exec(`UPDATE users SET pattern=?, repos=? WHERE name=?`, u.Pattern, string(repos), u.Name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("user %q not found", u.Name)
		}
		return nil
	}
	_, err := d.sql.Exec(`INSERT INTO users(name, token_hash, pattern, repos, created_at) VALUES(?,?,?,?,?) ON CONFLICT(name) DO UPDATE SET token_hash=excluded.token_hash, pattern=excluded.pattern, repos=excluded.repos`,
		u.Name, Hash(token), u.Pattern, string(repos), now())
	return err
}

func (d *DB) DeleteUser(name string) error {
	_, err := d.sql.Exec(`DELETE FROM users WHERE name=?`, name)
	return err
}

func (d *DB) Users() ([]User, error) {
	rows, err := d.sql.Query(`SELECT name, token_hash, pattern, repos, created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var repos string
		if err := rows.Scan(&u.Name, &u.tokenHash, &u.Pattern, &repos, &u.CreatedAt); err != nil {
			return nil, err
		}
		json.Unmarshal([]byte(repos), &u.Repos)
		out = append(out, u)
	}
	return out, rows.Err()
}

// CheckUser returns the user if name/token match.
func (d *DB) CheckUser(name, token string) (User, bool) {
	var u User
	var repos string
	err := d.sql.QueryRow(`SELECT name, token_hash, pattern, repos, created_at FROM users WHERE name=?`, name).Scan(&u.Name, &u.tokenHash, &u.Pattern, &repos, &u.CreatedAt)
	if err != nil || !equalHash(u.tokenHash, Hash(token)) {
		return User{}, false
	}
	json.Unmarshal([]byte(repos), &u.Repos)
	return u, true
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

// ---- sessions

func (d *DB) NewSession(ttl time.Duration) (string, error) {
	id := Token()
	_, err := d.sql.Exec(`INSERT INTO sessions(id_hash, expires) VALUES(?,?)`, Hash(id), time.Now().Add(ttl).Unix())
	d.sql.Exec(`DELETE FROM sessions WHERE expires < ?`, now())
	return id, err
}

func (d *DB) CheckSession(id string) bool {
	var exp int64
	err := d.sql.QueryRow(`SELECT expires FROM sessions WHERE id_hash=?`, Hash(id)).Scan(&exp)
	return err == nil && exp > now()
}

func (d *DB) DeleteSession(id string) {
	d.sql.Exec(`DELETE FROM sessions WHERE id_hash=?`, Hash(id))
}

func (d *DB) DeleteSessions() {
	d.sql.Exec(`DELETE FROM sessions`)
}

// ---- events

type Event struct {
	ID      int64  `json:"id"`
	At      int64  `json:"at"`
	Project string `json:"project"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (d *DB) Event(project, kind, format string, args ...any) {
	d.sql.Exec(`INSERT INTO events(at, project, kind, message) VALUES(?,?,?,?)`, now(), project, kind, fmt.Sprintf(format, args...))
	d.sql.Exec(`DELETE FROM events WHERE id <= (SELECT max(id) FROM events) - 5000`)
}

func (d *DB) Events(project string, limit int) ([]Event, error) {
	rows, err := d.sql.Query(`SELECT id, at, project, kind, message FROM events WHERE ?='' OR project=? ORDER BY id DESC LIMIT ?`, project, project, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.At, &e.Project, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

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
