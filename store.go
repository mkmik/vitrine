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

type Store struct {
	db            *DB
	requireSignIn bool // "public" grants reach only signed-in users
	noPublic      bool // "public" grants reach nobody (public sharing is off)
}

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
  updated_at INTEGER NOT NULL,
  kind TEXT NOT NULL DEFAULT 'html'
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
CREATE TABLE IF NOT EXISTS thumbs (
  artifact_id TEXT PRIMARY KEY,
  y INTEGER NOT NULL DEFAULT 0,
  mime TEXT NOT NULL DEFAULT '',
  data BLOB NOT NULL,
  updated_at INTEGER NOT NULL,
  auto INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS folders (
  id TEXT PRIMARY KEY,
  user_id TEXT NOT NULL,
  name TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS folders_user ON folders(user_id);
CREATE TABLE IF NOT EXISTS folder_items (
  user_id TEXT NOT NULL,
  artifact_id TEXT NOT NULL,
  folder_id TEXT NOT NULL,
  added_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, artifact_id)
);
CREATE INDEX IF NOT EXISTS folder_items_folder ON folder_items(folder_id);
CREATE INDEX IF NOT EXISTS folder_items_artifact ON folder_items(artifact_id);
`

// openStore opens the store named by VITRINE_DB: a postgres:// URL, or a
// SQLite file path.
func openStore(path string) (*Store, error) {
	if isPostgresURL(path) {
		db, err := sql.Open("pgx", path)
		if err != nil {
			return nil, err
		}
		db.SetMaxOpenConns(10)
		db.SetConnMaxIdleTime(5 * time.Minute)
		if err := db.Ping(); err != nil {
			db.Close()
			return nil, fmt.Errorf("postgres: %w", err)
		}
		if err := migratePostgres(db); err != nil {
			db.Close()
			return nil, fmt.Errorf("postgres schema: %w", err)
		}
		return &Store{db: &DB{DB: db, pg: true}}, nil
	}
	// _txlock=immediate: a transaction takes the write lock at BEGIN, so a
	// read-then-write transaction never races another writer.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(sqlSchema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: &DB{DB: db}}, nil
}

func (s *Store) close() error { return s.db.Close() }

// migrate brings databases created by older builds up to the current schema.
func migrate(db *sql.DB) error {
	has, err := hasColumn(db, "artifacts", "kind")
	if err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE artifacts ADD COLUMN kind TEXT NOT NULL DEFAULT 'html'`); err != nil {
			return err
		}
	}
	if has, err = hasColumn(db, "thumbs", "auto"); err != nil {
		return err
	}
	if !has {
		if _, err := db.Exec(`ALTER TABLE thumbs ADD COLUMN auto INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return false, err
		}
		if n == col {
			return true, nil
		}
	}
	return false, rows.Err()
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
		_, err = s.db.Exec(`INSERT INTO users(id, sub, email, name, created_at) VALUES(?,?,?,?,?)`, u.ID, sub, email, noNUL(name), now())
		if isUnique(err) {
			u = &User{}
			err = s.db.QueryRow(`SELECT id, sub, email, name FROM users WHERE sub=?`, sub).Scan(&u.ID, &u.Sub, &u.Email, &u.Name)
		}
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
	Kind         string          `json:"kind"`
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

const artifactCols = `id, owner_id, title, description, head, capabilities, created_at, updated_at, kind`

func scanArtifact(sc interface{ Scan(...any) error }) (*Artifact, error) {
	a := &Artifact{}
	var caps string
	if err := sc.Scan(&a.ID, &a.OwnerID, &a.Title, &a.Description, &a.Head, &caps, &a.CreatedAt, &a.UpdatedAt, &a.Kind); err != nil {
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

// noNUL drops NUL characters, which Postgres text columns refuse.
func noNUL(s string) string { return strings.ReplaceAll(s, "\x00", "") }

func plainText(html string) string {
	t := tagRE.ReplaceAllString(noNUL(html), " ")
	t = wsRE.ReplaceAllString(t, " ")
	if len(t) > 200000 {
		t = t[:200000]
	}
	return t
}

func (s *Store) indexFTS(tx *Tx, a *Artifact, html string) error {
	if _, err := tx.Exec(`DELETE FROM artifacts_fts WHERE id=?`, a.ID); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO artifacts_fts(id, title, description, body) VALUES(?,?,?,?)`, a.ID, noNUL(a.Title), noNUL(a.Description), plainText(html))
	return err
}

func (s *Store) createArtifact(ownerID, kind, title, desc, html string, caps json.RawMessage, author string) (*Artifact, error) {
	if len(caps) == 0 {
		caps = json.RawMessage(`{}`)
	}
	if kind == "" {
		kind = kindHTML
	}
	a := &Artifact{ID: randID(7)[:10], OwnerID: ownerID, Kind: kind, Title: noNUL(title), Description: noNUL(desc), Head: 1, Capabilities: caps, CreatedAt: now(), UpdatedAt: now()}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO artifacts(`+artifactCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
		a.ID, a.OwnerID, a.Title, a.Description, a.Head, string(caps), a.CreatedAt, a.UpdatedAt, a.Kind); err != nil {
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
	a, err := scanArtifact(tx.QueryRow(`SELECT `+artifactCols+` FROM artifacts WHERE id=?`+s.db.forUpdate(), id))
	if err != nil {
		return nil, errNotFound
	}
	if base != a.Head {
		return a, errConflict
	}
	a.Head++
	a.UpdatedAt = now()
	if title != nil && *title != "" {
		a.Title = noNUL(*title)
	}
	if desc != nil {
		a.Description = noNUL(*desc)
	}
	if _, err := tx.Exec(`INSERT INTO versions(artifact_id, n, html, size, author, note, created_at) VALUES(?,?,?,?,?,?,?)`,
		a.ID, a.Head, gz([]byte(html)), len(html), author, note, a.UpdatedAt); err != nil {
		if isUnique(err) {
			a.Head--
			return a, errConflict
		}
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
		a.Title = noNUL(*title)
	}
	if desc != nil {
		a.Description = noNUL(*desc)
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
		`DELETE FROM grants WHERE artifact_id=?`, `DELETE FROM docs WHERE artifact_id=?`, `DELETE FROM thumbs WHERE artifact_id=?`, `DELETE FROM folder_items WHERE artifact_id=?`} {
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
	ThumbY     int     `json:"thumb_y,omitempty"`
	ThumbImg   int64   `json:"thumb_img,omitempty"` // when the uploaded image changed; 0 = none
	Folder     string  `json:"folder,omitempty"`    // the viewer's folder holding it
}

// ListFilter narrows a listing. Zero values mean "any".
type ListFilter struct {
	Owner  string // "mine" or "shared"
	Kind   string // kindHTML or kindMarkdown
	Folder string // a folder id, or "none" for artifacts in no folder
}

// accessible is the condition for artifacts u can list: owned, or shared with them or all users.
// "link" grants open for every signed-in user but are left out on purpose: only those with the link find it. Args: user id, "email:"+email.
const accessible = `(a.owner_id=? OR a.id IN (SELECT artifact_id FROM grants WHERE audience IN (?, 'users')))`

// listFor returns artifacts the user owns or has a person/users grant for, newest first.
func (s *Store) listFor(u *User, query string, limit int, before int64, f ListFilter) ([]ListItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if before <= 0 {
		before = 1 << 62
	}
	// thumbnail: scroll offset, and when the uploaded image (if any) last changed
	sel := `SELECT a.id, a.owner_id, a.title, a.description, a.head, a.capabilities, a.created_at, a.updated_at, a.kind, COALESCE(ou.email,''),
		COALESCE(t.y, 0), CASE WHEN t.mime <> '' THEN t.updated_at ELSE 0 END, COALESCE(fi.folder_id, ''), `
	joins := ` LEFT JOIN users ou ON ou.id=a.owner_id LEFT JOIN thumbs t ON t.artifact_id=a.id
		LEFT JOIN folder_items fi ON fi.artifact_id=a.id AND fi.user_id=?`
	args := []any{u.ID}
	var q string
	switch {
	case query == "":
		q = sel + `'' FROM artifacts a` + joins + ` WHERE ` + accessible
		args = append(args, u.ID, "email:"+u.Email)
	case s.db.pg:
		tsq := pgTSQuery(query)
		if tsq == "" {
			return nil, nil
		}
		q = sel + `ts_headline('english', f.body, tq, 'StartSel=«, StopSel=», MaxWords=14, MinWords=6, MaxFragments=1, FragmentDelimiter=…')
		FROM artifacts_fts f JOIN artifacts a ON a.id=f.id` + joins + `, to_tsquery('english', ?) tq
		WHERE ` + accessible + ` AND f.tsv @@ tq`
		args = append(args, tsq, u.ID, "email:"+u.Email)
	default:
		q = sel + `snippet(artifacts_fts, 3, '«', '»', '…', 12)
		FROM artifacts_fts f JOIN artifacts a ON a.id=f.id` + joins + `
		WHERE ` + accessible + ` AND artifacts_fts MATCH ?`
		args = append(args, u.ID, "email:"+u.Email, ftsQuery(query))
	}
	switch f.Owner {
	case "mine":
		q += ` AND a.owner_id=?`
		args = append(args, u.ID)
	case "shared":
		q += ` AND a.owner_id<>?`
		args = append(args, u.ID)
	}
	if f.Kind != "" {
		q += ` AND a.kind=?`
		args = append(args, f.Kind)
	}
	switch f.Folder {
	case "":
	case "none":
		q += ` AND fi.folder_id IS NULL`
	default:
		q += ` AND fi.folder_id=?`
		args = append(args, f.Folder)
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
		if err := rows.Scan(&a.ID, &a.OwnerID, &a.Title, &a.Description, &a.Head, &caps, &a.CreatedAt, &a.UpdatedAt, &a.Kind, &li.OwnerEmail,
			&li.ThumbY, &li.ThumbImg, &li.Folder, &li.Snippet); err != nil {
			return nil, err
		}
		a.Capabilities = json.RawMessage(caps)
		out = append(out, li)
	}
	return out, rows.Err()
}

// ---- folders ----
// Folders are personal: each user files the artifacts they can see (their own
// or shared with them) into their own flat folders, one folder per artifact.

type Folder struct {
	ID    string        `json:"id"`
	Name  string        `json:"name"`
	Count int           `json:"count"`
	Items []FolderThumb `json:"items"` // the newest few, for the folder's preview
}

type FolderThumb struct {
	ID       string `json:"id"`
	ThumbY   int    `json:"thumb_y,omitempty"`
	ThumbImg int64  `json:"thumb_img,omitempty"`
}

const folderPreviews = 4

func folderName(name string) (string, error) {
	name = strings.TrimSpace(noNUL(name))
	if name == "" || len([]rune(name)) > 100 {
		return "", errors.New("a folder name needs 1 to 100 characters")
	}
	return name, nil
}

func (s *Store) createFolder(userID, name string) (*Folder, error) {
	name, err := folderName(name)
	if err != nil {
		return nil, err
	}
	f := &Folder{ID: "f_" + randID(8), Name: name, Items: []FolderThumb{}}
	_, err = s.db.Exec(`INSERT INTO folders(id, user_id, name, created_at) VALUES(?,?,?,?)`, f.ID, userID, f.Name, now())
	return f, err
}

func (s *Store) renameFolder(userID, id, name string) error {
	name, err := folderName(name)
	if err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE folders SET name=? WHERE id=? AND user_id=?`, name, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	return nil
}

// deleteFolder removes the folder; its artifacts go back to the top level.
func (s *Store) deleteFolder(userID, id string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM folders WHERE id=? AND user_id=?`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound
	}
	if _, err := tx.Exec(`DELETE FROM folder_items WHERE folder_id=? AND user_id=?`, id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// fileArtifact puts an artifact in one of the user's folders, or takes it out (folderID "").
// The caller checks the user can see the artifact.
func (s *Store) fileArtifact(userID, artifactID, folderID string) error {
	if folderID == "" {
		_, err := s.db.Exec(`DELETE FROM folder_items WHERE user_id=? AND artifact_id=?`, userID, artifactID)
		return err
	}
	var x string
	if err := s.db.QueryRow(`SELECT id FROM folders WHERE id=? AND user_id=?`, folderID, userID).Scan(&x); err != nil {
		return errNotFound
	}
	_, err := s.db.Exec(`INSERT INTO folder_items(user_id, artifact_id, folder_id, added_at) VALUES(?,?,?,?)
		ON CONFLICT(user_id, artifact_id) DO UPDATE SET folder_id=excluded.folder_id, added_at=excluded.added_at`, userID, artifactID, folderID, now())
	return err
}

// folders lists the user's folders by name, each with how many artifacts in it
// they can still see and the newest few of those.
func (s *Store) folders(u *User) ([]Folder, error) {
	rows, err := s.db.Query(`SELECT id, name FROM folders WHERE user_id=? ORDER BY LOWER(name), id`, u.ID)
	if err != nil {
		return nil, err
	}
	out := []Folder{}
	idx := map[string]int{}
	for rows.Next() {
		f := Folder{Items: []FolderThumb{}}
		if err := rows.Scan(&f.ID, &f.Name); err != nil {
			rows.Close()
			return nil, err
		}
		idx[f.ID] = len(out)
		out = append(out, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	// ponytail: reads every filed artifact the user can see; fine for hundreds, a per-folder LIMIT query if folders get huge
	rows, err = s.db.Query(`SELECT fi.folder_id, a.id, COALESCE(t.y, 0), CASE WHEN t.mime <> '' THEN t.updated_at ELSE 0 END
		FROM folder_items fi JOIN artifacts a ON a.id=fi.artifact_id LEFT JOIN thumbs t ON t.artifact_id=a.id
		WHERE fi.user_id=? AND `+accessible+` ORDER BY a.updated_at DESC`, u.ID, u.ID, "email:"+u.Email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var fid string
		var t FolderThumb
		if err := rows.Scan(&fid, &t.ID, &t.ThumbY, &t.ThumbImg); err != nil {
			return nil, err
		}
		i, ok := idx[fid]
		if !ok {
			continue
		}
		out[i].Count++
		if len(out[i].Items) < folderPreviews {
			out[i].Items = append(out[i].Items, t)
		}
	}
	return out, rows.Err()
}

// ---- thumbnails ----
// A thumbnail is the page itself, scrolled to y, unless there is an image:
// one a person chose (auto = 0), or one the viewer took by itself of version
// auto. A person's choice is never replaced by an automatic one.

func (s *Store) setThumb(id string, y int, mime string, data []byte) error {
	if data == nil {
		data = []byte{}
	}
	_, err := s.db.Exec(`INSERT INTO thumbs(artifact_id, y, mime, data, updated_at, auto) VALUES(?, ?, ?, ?, ?, 0)
		ON CONFLICT(artifact_id) DO UPDATE SET y=excluded.y, mime=excluded.mime, data=excluded.data, updated_at=excluded.updated_at, auto=0`,
		id, y, mime, data, now())
	return err
}

// setAutoThumb stores an automatic image of version ver, unless a person
// chose the thumbnail or there is already one of ver or later.
func (s *Store) setAutoThumb(id string, ver int, mime string, data []byte) error {
	_, err := s.db.Exec(`INSERT INTO thumbs(artifact_id, y, mime, data, updated_at, auto) VALUES(?, 0, ?, ?, ?, ?)
		ON CONFLICT(artifact_id) DO UPDATE SET y=0, mime=excluded.mime, data=excluded.data, updated_at=excluded.updated_at, auto=excluded.auto
		WHERE thumbs.auto > 0 AND thumbs.auto < excluded.auto`,
		id, mime, data, now(), ver)
	return err
}

// thumbMeta returns the scroll offset, when the image changed (0: none),
// and the version an automatic image shows (0: a person chose it, or none).
func (s *Store) thumbMeta(id string) (y int, img int64, auto int) {
	s.db.QueryRow(`SELECT y, CASE WHEN mime <> '' THEN updated_at ELSE 0 END, auto FROM thumbs WHERE artifact_id=?`, id).Scan(&y, &img, &auto)
	return
}

func (s *Store) thumbImage(id string) (mime string, data []byte, err error) {
	err = s.db.QueryRow(`SELECT mime, data FROM thumbs WHERE artifact_id=? AND mime <> ''`, id).Scan(&mime, &data)
	if err == sql.ErrNoRows {
		err = errNotFound
	}
	return
}

var wordRE = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// pgTSQuery turns free text into a prefix-matching tsquery: every word must
// match, each as a prefix (walr finds walrus). Only letters and digits get
// through, so query syntax can't be injected.
func pgTSQuery(q string) string {
	var parts []string
	for _, w := range wordRE.FindAllString(q, 20) {
		parts = append(parts, w+":*")
	}
	return strings.Join(parts, " & ")
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
