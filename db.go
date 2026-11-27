package main

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// DB is database/sql with the two dialects vitrine supports, SQLite and
// Postgres. Queries are written once with ? placeholders; on Postgres they
// are rebound to $1, $2, ... The few statements that differ ask the DB which
// dialect it is (search, schema, backups).
type DB struct {
	*sql.DB
	pg     bool
	rebind sync.Map // query -> rebound query
}

func isPostgresURL(s string) bool {
	return strings.HasPrefix(s, "postgres://") || strings.HasPrefix(s, "postgresql://")
}

func (d *DB) q(query string) string {
	if !d.pg {
		return query
	}
	if v, ok := d.rebind.Load(query); ok {
		return v.(string)
	}
	var b strings.Builder
	n, quoted := 0, false
	for i := 0; i < len(query); i++ {
		c := query[i]
		switch {
		case c == '\'':
			quoted = !quoted // '' inside a literal toggles twice: still correct
		case c == '?' && !quoted:
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			continue
		}
		b.WriteByte(c)
	}
	out := b.String()
	d.rebind.Store(query, out)
	return out
}

func (d *DB) Exec(query string, args ...any) (sql.Result, error) {
	return d.DB.Exec(d.q(query), args...)
}
func (d *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return d.DB.Query(d.q(query), args...)
}
func (d *DB) QueryRow(query string, args ...any) *sql.Row { return d.DB.QueryRow(d.q(query), args...) }

func (d *DB) Begin() (*Tx, error) {
	tx, err := d.DB.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, d: d}, nil
}

// forUpdate locks the row a transaction reads before it writes it. On
// Postgres that is SELECT ... FOR UPDATE; SQLite transactions take the write
// lock at BEGIN (_txlock=immediate), so they need nothing.
func (d *DB) forUpdate() string {
	if d.pg {
		return " FOR UPDATE"
	}
	return ""
}

type Tx struct {
	*sql.Tx
	d *DB
}

func (t *Tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.Tx.Exec(t.d.q(query), args...)
}
func (t *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.Tx.Query(t.d.q(query), args...)
}
func (t *Tx) QueryRow(query string, args ...any) *sql.Row {
	return t.Tx.QueryRow(t.d.q(query), args...)
}

// isUnique reports a primary key or unique constraint violation.
func isUnique(err error) bool {
	if err == nil {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code == "23505"
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed") || strings.Contains(err.Error(), "constraint failed: PRIMARY KEY")
}

// ---- Postgres schema: numbered migrations, applied under an advisory lock
// so replicas starting together don't race ----

var pgMigrations = []string{
	// 1: everything up to markdown artifacts (kind)
	`
CREATE TABLE users (
  id TEXT COLLATE "C" PRIMARY KEY,
  sub TEXT COLLATE "C" UNIQUE NOT NULL,
  email TEXT NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL
);
CREATE INDEX users_email ON users(email);
CREATE TABLE sessions (
  token_hash TEXT COLLATE "C" PRIMARY KEY,
  user_id TEXT COLLATE "C" NOT NULL,
  csrf TEXT NOT NULL,
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL
);
CREATE TABLE tokens (
  id TEXT COLLATE "C" PRIMARY KEY,
  token_hash TEXT COLLATE "C" UNIQUE NOT NULL,
  kind TEXT NOT NULL,
  user_id TEXT COLLATE "C" NOT NULL,
  name TEXT NOT NULL DEFAULT '',
  client_id TEXT COLLATE "C" NOT NULL DEFAULT '',
  family TEXT COLLATE "C" NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  expires_at BIGINT NOT NULL DEFAULT 0,
  last_used_at BIGINT NOT NULL DEFAULT 0,
  used INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX tokens_user ON tokens(user_id);
CREATE INDEX tokens_family ON tokens(family);
CREATE TABLE oauth_clients (
  id TEXT COLLATE "C" PRIMARY KEY,
  name TEXT NOT NULL,
  redirect_uris TEXT NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE TABLE oauth_codes (
  code_hash TEXT COLLATE "C" PRIMARY KEY,
  client_id TEXT COLLATE "C" NOT NULL,
  user_id TEXT COLLATE "C" NOT NULL,
  redirect_uri TEXT NOT NULL,
  challenge TEXT NOT NULL,
  expires_at BIGINT NOT NULL
);
CREATE TABLE login_states (
  state TEXT COLLATE "C" PRIMARY KEY,
  nonce TEXT NOT NULL,
  verifier TEXT NOT NULL,
  next TEXT NOT NULL,
  expires_at BIGINT NOT NULL
);
CREATE TABLE artifacts (
  id TEXT COLLATE "C" PRIMARY KEY,
  owner_id TEXT COLLATE "C" NOT NULL,
  title TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  head INTEGER NOT NULL,
  capabilities TEXT NOT NULL DEFAULT '{}',
  created_at BIGINT NOT NULL,
  updated_at BIGINT NOT NULL,
  kind TEXT NOT NULL DEFAULT 'html'
);
CREATE INDEX artifacts_owner ON artifacts(owner_id, updated_at);
CREATE TABLE versions (
  artifact_id TEXT COLLATE "C" NOT NULL,
  n INTEGER NOT NULL,
  html BYTEA NOT NULL,
  size INTEGER NOT NULL,
  author TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL,
  PRIMARY KEY (artifact_id, n)
);
CREATE TABLE artifacts_fts (
  id TEXT COLLATE "C" PRIMARY KEY,
  title TEXT NOT NULL,
  description TEXT NOT NULL,
  body TEXT NOT NULL,
  tsv tsvector GENERATED ALWAYS AS (
    setweight(to_tsvector('english', title), 'A') ||
    setweight(to_tsvector('english', description), 'B') ||
    to_tsvector('english', body)) STORED
);
CREATE INDEX artifacts_fts_tsv ON artifacts_fts USING GIN (tsv);
CREATE TABLE grants (
  artifact_id TEXT COLLATE "C" NOT NULL,
  audience TEXT COLLATE "C" NOT NULL,
  level TEXT NOT NULL,
  pinned INTEGER NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL,
  PRIMARY KEY (artifact_id, audience)
);
CREATE INDEX grants_audience ON grants(audience);
CREATE TABLE docs (
  artifact_id TEXT COLLATE "C" NOT NULL,
  path TEXT COLLATE "C" NOT NULL,
  coll TEXT COLLATE "C" NOT NULL,
  body TEXT NOT NULL,
  version INTEGER NOT NULL,
  updated_by TEXT NOT NULL DEFAULT '',
  updated_at BIGINT NOT NULL,
  lease_holder TEXT NOT NULL DEFAULT '',
  lease_until BIGINT NOT NULL DEFAULT 0,
  PRIMARY KEY (artifact_id, path)
);
CREATE INDEX docs_coll ON docs(artifact_id, coll);
`,
	// 2: thumbnails
	`
CREATE TABLE thumbs (
  artifact_id TEXT COLLATE "C" PRIMARY KEY,
  y INTEGER NOT NULL DEFAULT 0,
  mime TEXT NOT NULL DEFAULT '',
  data BYTEA NOT NULL,
  updated_at BIGINT NOT NULL
);
`,
	// 3: folders
	`
CREATE TABLE folders (
  id TEXT COLLATE "C" PRIMARY KEY,
  user_id TEXT COLLATE "C" NOT NULL,
  name TEXT NOT NULL,
  created_at BIGINT NOT NULL
);
CREATE INDEX folders_user ON folders(user_id);
CREATE TABLE folder_items (
  user_id TEXT COLLATE "C" NOT NULL,
  artifact_id TEXT COLLATE "C" NOT NULL,
  folder_id TEXT COLLATE "C" NOT NULL,
  added_at BIGINT NOT NULL,
  PRIMARY KEY (user_id, artifact_id)
);
CREATE INDEX folder_items_folder ON folder_items(folder_id);
CREATE INDEX folder_items_artifact ON folder_items(artifact_id);
`,
	// 4: thumbnails the viewer takes by itself
	`ALTER TABLE thumbs ADD COLUMN auto INTEGER NOT NULL DEFAULT 0;`,
}

const pgLockKey = 0x76697472696e65 // "vitrine"

func migratePostgres(db *sql.DB) error {
	ctx := context.Background()
	conn, err := db.Conn(ctx) // the advisory lock belongs to one session
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, pgLockKey); err != nil {
		return err
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, pgLockKey)
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)`); err != nil {
		return err
	}
	var have int
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		return err
	}
	for i := have; i < len(pgMigrations); i++ {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(pgMigrations[i]); err != nil {
			tx.Rollback()
			return errors.New("migration " + strconv.Itoa(i+1) + ": " + err.Error())
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES($1, $2)`, i+1, now()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
