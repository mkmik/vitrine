package main

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	errNotFound = errors.New("not found")
	errConflict = errors.New("conflict")
)

type Store struct{ db *sql.DB }

const sqlSchema = `
CREATE TABLE IF NOT EXISTS users (
  id TEXT PRIMARY KEY,
  sub TEXT UNIQUE NOT NULL,
  email TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS users_email ON users(email);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  csrf TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
  id TEXT PRIMARY KEY,
  token_hash TEXT UNIQUE NOT NULL,
  kind TEXT NOT NULL,
  user_id TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  client_id TEXT NOT NULL DEFAULT '',
  family TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  last_used_at INTEGER NOT NULL DEFAULT 0,
  used INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tokens_user ON tokens(user_id);
CREATE INDEX IF NOT EXISTS tokens_family ON tokens(family);
CREATE TABLE IF NOT EXISTS oauth_clients (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  redirect_uris TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_codes (
  code_hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL,
  user_id TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  challenge TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS login_states (
  state TEXT PRIMARY KEY,
  nonce TEXT NOT NULL,
  verifier TEXT NOT NULL,
  next TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS artifacts (
  id TEXT PRIMARY KEY,
  owner_id TEXT NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  head INTEGER NOT NULL,
  capabilities TEXT NOT NULL DEFAULT '{}',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS artifacts_owner ON artifacts(owner_id, updated_at);
CREATE TABLE IF NOT EXISTS versions (
  artifact_id TEXT NOT NULL,
  n INTEGER NOT NULL,
  html BLOB NOT NULL,
  size INTEGER NOT NULL,
  author TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  PRIMARY KEY (artifact_id, n)
);
CREATE VIRTUAL TABLE IF NOT EXISTS artifacts_fts USING fts5(id UNINDEXED, title, description, body, tokenize='porter unicode61');
CREATE TABLE IF NOT EXISTS grants (
  artifact_id TEXT NOT NULL,
  audience TEXT NOT NULL,
  level TEXT NOT NULL,
  pinned INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (artifact_id, audience)
);
CREATE INDEX IF NOT EXISTS grants_audience ON grants(audience);
CREATE TABLE IF NOT EXISTS docs (
  artifact_id TEXT NOT NULL,
  path TEXT NOT NULL,
  coll TEXT NOT NULL,
  body TEXT NOT NULL,
  version INTEGER NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  updated_at INTEGER NOT NULL,
  lease_holder TEXT NOT NULL DEFAULT '',
  lease_until INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (artifact_id, path)
);
CREATE INDEX IF NOT EXISTS docs_coll ON docs(artifact_id, coll);
`

func openStore(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(sqlSchema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	return &Store{db: db}, nil
}

func now() int64 { return time.Now().Unix() }

func randID(nbytes int) string {
	b := make([]byte, nbytes)
	rand.Read(b)
	return strings.ToLower(strings.TrimRight(base32.StdEncoding.EncodeToString(b), "="))
}

func randToken(prefix string) string {
	b := make([]byte, 32)
	rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func gz(b []byte) []byte {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

func gunz(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

// ---- users & sessions ----

type User struct {
	ID    string `json:"id"`
	Sub   string `json:"-"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (u *User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	return strings.SplitN(u.Email, "@", 2)[0]
}

func (s *Store) upsertUser(sub, email, name string) (*User, error) {
	email = strings.ToLower(email)
	u := &User{}
	err := s.db.QueryRow(`SELECT id, sub, email, name FROM users WHERE sub=?`, sub).Scan(&u.ID, &u.Sub, &u.Email, &u.Name)
	if err == sql.ErrNoRows && !strings.HasPrefix(sub, "local|") {
		// A real sign-in adopts a placeholder the CLI created for this email (import, token -create).
		var id string
		if s.db.QueryRow(`SELECT id FROM users WHERE sub=?`, "local|"+email).Scan(&id) == nil {
			if _, err := s.db.Exec(`UPDATE users SET sub=?, name=CASE WHEN ?<>'' THEN ? ELSE name END WHERE id=?`, sub, name, name, id); err == nil {
				return s.userByID(id)
			}
		}
	}
	if err == sql.ErrNoRows {
		u = &User{ID: "u_" + randID(10), Sub: sub, Email: email, Name: name}
		_, err = s.db.Exec(`INSERT INTO users(id, sub, email, name, created_at) VALUES(?,?,?,?,?)`, u.ID, sub, email, name, now())
		return u, err
	}
	if err != nil {
		return nil, err
	}
	if u.Email != email || (name != "" && u.Name != name) {
		if name == "" {
			name = u.Name
		}
		s.db.Exec(`UPDATE users SET email=?, name=? WHERE id=?`, email, name, u.ID)
		u.Email, u.Name = email, name
	}
	return u, nil
}

func (s *Store) userByID(id string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id, sub, email, name FROM users WHERE id=?`, id).Scan(&u.ID, &u.Sub, &u.Email, &u.Name)
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	return u, err
}

func (s *Store) userByEmail(email string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(`SELECT id, sub, email, name FROM users WHERE email=? ORDER BY created_at LIMIT 1`, email).Scan(&u.ID, &u.Sub, &u.Email, &u.Name)
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	return u, err
}

type Session struct {
	User *User
	CSRF string
}

func (s *Store) createSession(userID string, ttl time.Duration) (token, csrf string, err error) {
	token = randToken("")
	csrf = randToken("")[:32]
	_, err = s.db.Exec(`INSERT INTO sessions(token_hash, user_id, csrf, created_at, expires_at) VALUES(?,?,?,?,?)`,
		hashToken(token), userID, csrf, now(), time.Now().Add(ttl).Unix())
	return
}

func (s *Store) session(token string) (*Session, error) {
	var uid, csrf string
	var exp int64
	err := s.db.QueryRow(`SELECT user_id, csrf, expires_at FROM sessions WHERE token_hash=?`, hashToken(token)).Scan(&uid, &csrf, &exp)
	if err != nil || exp < now() {
		return nil, errNotFound
	}
	u, err := s.userByID(uid)
	if err != nil {
		return nil, err
	}
	return &Session{User: u, CSRF: csrf}, nil
}

func (s *Store) deleteSession(token string) {
	s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(token))
}

// ---- tokens ----

type TokenInfo struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	ClientID   string `json:"client_id,omitempty"`
	ClientName string `json:"client_name,omitempty"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

func (s *Store) createPAT(userID, name string) (string, error) {
	t := randToken("vit_")
	_, err := s.db.Exec(`INSERT INTO tokens(id, token_hash, kind, user_id, name, created_at) VALUES(?,?,?,?,?,?)`,
		"t_"+randID(8), hashToken(t), "pat", userID, name, now())
	return t, err
}

func (s *Store) insertToken(kind, userID, clientID, family string, ttl time.Duration) (string, error) {
	prefix := map[string]string{"access": "vat_", "refresh": "vrt_"}[kind]
	t := randToken(prefix)
	_, err := s.db.Exec(`INSERT INTO tokens(id, token_hash, kind, user_id, client_id, family, created_at, expires_at) VALUES(?,?,?,?,?,?,?,?)`,
		"t_"+randID(8), hashToken(t), kind, userID, clientID, family, now(), time.Now().Add(ttl).Unix())
	return t, err
}

type tokenRow struct {
	ID, Kind, UserID, ClientID, Family string
	ExpiresAt                          int64
	Used                               bool
}

func (s *Store) lookupToken(t string) (*tokenRow, error) {
	r := &tokenRow{}
	var used int
	err := s.db.QueryRow(`SELECT id, kind, user_id, client_id, family, expires_at, used FROM tokens WHERE token_hash=?`, hashToken(t)).
		Scan(&r.ID, &r.Kind, &r.UserID, &r.ClientID, &r.Family, &r.ExpiresAt, &used)
	if err != nil {
		return nil, errNotFound
	}
	r.Used = used != 0
	return r, nil
}

func (s *Store) touchToken(id string) {
	s.db.Exec(`UPDATE tokens SET last_used_at=? WHERE id=? AND last_used_at < ?`, now(), id, now()-60)
}

func (s *Store) listTokens(userID string) ([]TokenInfo, error) {
	rows, err := s.db.Query(`SELECT t.id, t.kind, t.name, t.client_id, COALESCE(c.name,''), t.created_at, t.expires_at,
		COALESCE((SELECT MAX(x.last_used_at) FROM tokens x WHERE x.family<>'' AND x.family=t.family), t.last_used_at)
		FROM tokens t LEFT JOIN oauth_clients c ON c.id=t.client_id
		WHERE t.user_id=? AND (t.kind='pat' OR (t.kind='refresh' AND t.used=0 AND t.expires_at>?)) ORDER BY t.created_at DESC`, userID, now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TokenInfo
	for rows.Next() {
		var t TokenInfo
		rows.Scan(&t.ID, &t.Kind, &t.Name, &t.ClientID, &t.ClientName, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt)
		out = append(out, t)
	}
	return out, nil
}

// revokeToken deletes a PAT, or a whole OAuth token family given one of its token ids.
func (s *Store) revokeToken(userID, id string) error {
	var family string
	if err := s.db.QueryRow(`SELECT family FROM tokens WHERE id=? AND user_id=?`, id, userID).Scan(&family); err != nil {
		return errNotFound
	}
	if family != "" {
		_, err := s.db.Exec(`DELETE FROM tokens WHERE family=?`, family)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM tokens WHERE id=?`, id)
	return err
}

// ---- artifacts ----

type Artifact struct {
	ID           string          `json:"id"`
	OwnerID      string          `json:"owner_id"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Head         int             `json:"head"`
	Capabilities json.RawMessage `json:"capabilities"`
	CreatedAt    int64           `json:"created_at"`
	UpdatedAt    int64           `json:"updated_at"`
	caps         *Caps
}

type VersionInfo struct {
	N         int    `json:"version"`
	Size      int    `json:"size"`
	Author    string `json:"author"`
	Note      string `json:"note,omitempty"`
	CreatedAt int64  `json:"created_at"`
}

func (a *Artifact) Caps() *Caps {
	if a.caps == nil {
		a.caps, _ = parseCaps(a.Capabilities)
	}
	return a.caps
}

const artifactCols = `id, owner_id, title, description, head, capabilities, created_at, updated_at`

func scanArtifact(sc interface{ Scan(...any) error }) (*Artifact, error) {
	a := &Artifact{}
	var caps string
	if err := sc.Scan(&a.ID, &a.OwnerID, &a.Title, &a.Description, &a.Head, &caps, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return nil, err
	}
	a.Capabilities = json.RawMessage(caps)
	return a, nil
}

func (s *Store) artifact(id string) (*Artifact, error) {
	a, err := scanArtifact(s.db.QueryRow(`SELECT `+artifactCols+` FROM artifacts WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, errNotFound
	}
	return a, err
}

var tagRE = regexp.MustCompile(`(?is)<script.*?</script>|<style.*?</style>|<[^>]*>`)
var wsRE = regexp.MustCompile(`\s+`)

func plainText(html string) string {
	t := tagRE.ReplaceAllString(html, " ")
	t = wsRE.ReplaceAllString(t, " ")
	if len(t) > 200000 {
		t = t[:200000]
	}
	return t
}

func (s *Store) indexFTS(tx *sql.Tx, a *Artifact, html string) error {
	if _, err := tx.Exec(`DELETE FROM artifacts_fts WHERE id=?`, a.ID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO artifacts_fts(id, title, description, body) VALUES(?,?,?,?)`, a.ID, a.Title, a.Description, plainText(html))
	return err
}

func (s *Store) createArtifact(ownerID, title, desc, html string, caps json.RawMessage, author string) (*Artifact, error) {
	if len(caps) == 0 {
		caps = json.RawMessage(`{}`)
	}
	a := &Artifact{ID: randID(7)[:10], OwnerID: ownerID, Title: title, Description: desc, Head: 1, Capabilities: caps, CreatedAt: now(), UpdatedAt: now()}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO artifacts(`+artifactCols+`) VALUES(?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerID, a.Title, a.Description, a.Head, string(caps), a.CreatedAt, a.UpdatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO versions(artifact_id, n, html, size, author, created_at) VALUES(?,?,?,?,?,?)`,
		a.ID, 1, gz([]byte(html)), len(html), author, a.CreatedAt); err != nil {
		return nil, err
	}
	if err := s.indexFTS(tx, a, html); err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

// writeVersion appends version head+1 if base matches the head. title/desc nil = unchanged.
func (s *Store) writeVersion(id string, base int, html string, title, desc *string, author, note string) (*Artifact, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	a, err := scanArtifact(tx.QueryRow(`SELECT `+artifactCols+` FROM artifacts WHERE id=?`, id))
	if err != nil {
		return nil, errNotFound
	}
	if base != a.Head {
		return a, errConflict
	}
	a.Head++
	a.UpdatedAt = now()
	if title != nil && *title != "" {
		a.Title = *title
	}
	if desc != nil {
		a.Description = *desc
	}
	if _, err := tx.Exec(`INSERT INTO versions(artifact_id, n, html, size, author, note, created_at) VALUES(?,?,?,?,?,?,?)`,
		a.ID, a.Head, gz([]byte(html)), len(html), author, note, a.UpdatedAt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE artifacts SET head=?, title=?, description=?, updated_at=? WHERE id=?`, a.Head, a.Title, a.Description, a.UpdatedAt, a.ID); err != nil {
		return nil, err
	}
	if err := s.indexFTS(tx, a, html); err != nil {
		return nil, err
	}
	return a, tx.Commit()
}

func (s *Store) setMeta(id string, title, desc *string) (*Artifact, error) {
	a, err := s.artifact(id)
	if err != nil {
		return nil, err
	}
	if title != nil && *title != "" {
		a.Title = *title
	}
	if desc != nil {
		a.Description = *desc
	}
	html, _ := s.versionHTML(id, a.Head)
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE artifacts SET title=?, description=?, updated_at=? WHERE id=?`, a.Title, a.Description, now(), id); err != nil {
		return nil, err
	}
	s.indexFTS(tx, a, html)
	return a, tx.Commit()
}

func (s *Store) setCapabilities(id string, caps json.RawMessage) error {
	_, err := s.db.Exec(`UPDATE artifacts SET capabilities=?, updated_at=? WHERE id=?`, string(caps), now(), id)
	return err
}

func (s *Store) versionHTML(id string, n int) (string, error) {
	var b []byte
	if err := s.db.QueryRow(`SELECT html FROM versions WHERE artifact_id=? AND n=?`, id, n).Scan(&b); err != nil {
		return "", errNotFound
	}
	h, err := gunz(b)
	return string(h), err
}

func (s *Store) versions(id string) ([]VersionInfo, error) {
	rows, err := s.db.Query(`SELECT n, size, author, note, created_at FROM versions WHERE artifact_id=? ORDER BY n DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VersionInfo
	for rows.Next() {
		var v VersionInfo
		rows.Scan(&v.N, &v.Size, &v.Author, &v.Note, &v.CreatedAt)
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) deleteArtifact(id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM artifacts WHERE id=?`, `DELETE FROM versions WHERE artifact_id=?`, `DELETE FROM artifacts_fts WHERE id=?`,
		`DELETE FROM grants WHERE artifact_id=?`, `DELETE FROM docs WHERE artifact_id=?`} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type ListItem struct {
	*Artifact
	OwnerEmail string  `json:"owner_email"`
	Level      string  `json:"level"`
	Grants     []Grant `json:"grants,omitempty"`
	Snippet    string  `json:"snippet,omitempty"`
}

// listFor returns artifacts the user owns or has a person/users grant for, newest first.
func (s *Store) listFor(u *User, query string, limit int, before int64) ([]ListItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if before <= 0 {
		before = 1 << 62
	}
	args := []any{u.ID, "email:" + u.Email}
	q := `SELECT a.id, a.owner_id, a.title, a.description, a.head, a.capabilities, a.created_at, a.updated_at, COALESCE(ou.email,''), ''
		FROM artifacts a LEFT JOIN users ou ON ou.id=a.owner_id
		WHERE (a.owner_id=? OR a.id IN (SELECT artifact_id FROM grants WHERE audience IN (?, 'users')))`
	if query != "" {
		q = `SELECT a.id, a.owner_id, a.title, a.description, a.head, a.capabilities, a.created_at, a.updated_at, COALESCE(ou.email,''), snippet(artifacts_fts, 3, '«', '»', '…', 12)
		FROM artifacts_fts f JOIN artifacts a ON a.id=f.id LEFT JOIN users ou ON ou.id=a.owner_id
		WHERE (a.owner_id=? OR a.id IN (SELECT artifact_id FROM grants WHERE audience IN (?, 'users'))) AND artifacts_fts MATCH ?`
		args = append(args, ftsQuery(query))
	}
	q += ` AND a.updated_at < ? ORDER BY a.updated_at DESC LIMIT ?`
	args = append(args, before, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ListItem
	for rows.Next() {
		a := &Artifact{}
		var caps string
		li := ListItem{Artifact: a}
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Title, &a.Description, &a.Head, &caps, &a.CreatedAt, &a.UpdatedAt, &li.OwnerEmail, &li.Snippet); err != nil {
			return nil, err
		}
		a.Capabilities = json.RawMessage(caps)
		out = append(out, li)
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a prefix-matching FTS5 query.
func ftsQuery(q string) string {
	var parts []string
	for _, w := range strings.Fields(q) {
		w = strings.ReplaceAll(w, `"`, "")
		if w != "" {
			parts = append(parts, `"`+w+`"*`)
		}
	}
	if len(parts) == 0 {
		return `""`
	}
	return strings.Join(parts, " ")
}

// ---- grants ----

type Grant struct {
	Audience string `json:"audience"`
	Level    string `json:"level"`
	Pinned   int    `json:"pinned,omitempty"`
}

func (s *Store) grants(id string) ([]Grant, error) {
	rows, err := s.db.Query(`SELECT audience, level, pinned FROM grants WHERE artifact_id=? ORDER BY audience`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		rows.Scan(&g.Audience, &g.Level, &g.Pinned)
		out = append(out, g)
	}
	return out, nil
}

func (s *Store) setGrant(id string, g Grant) error {
	if g.Level == "none" || g.Level == "" {
		_, err := s.db.Exec(`DELETE FROM grants WHERE artifact_id=? AND audience=?`, id, g.Audience)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO grants(artifact_id, audience, level, pinned, created_at) VALUES(?,?,?,?,?)
		ON CONFLICT(artifact_id, audience) DO UPDATE SET level=excluded.level, pinned=excluded.pinned`, id, g.Audience, g.Level, g.Pinned, now())
	return err
}

func (s *Store) gc() {
	t := now()
	s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, t)
	s.db.Exec(`DELETE FROM oauth_codes WHERE expires_at < ?`, t)
	s.db.Exec(`DELETE FROM login_states WHERE expires_at < ?`, t)
	s.db.Exec(`DELETE FROM tokens WHERE kind IN ('access','refresh') AND expires_at < ?`, t-86400)
}
