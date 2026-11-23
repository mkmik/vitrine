package main

// Store tests, run against both backends: a SQLite file and a real Postgres.
// Postgres comes from embedded-postgres (downloads and runs the real server
// binaries), or from VITRINE_TEST_PG_URL (postgres://user:pass@host:port/,
// a server where the tests may create and drop databases).

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

var pgBase string // server URL ending in "/", database name appended per test

func TestMain(m *testing.M) {
	pgBase = os.Getenv("VITRINE_TEST_PG_URL")
	stop := func() {}
	if pgBase == "" {
		port := freePort()
		home, _ := os.UserHomeDir()
		run := filepath.Join(os.TempDir(), fmt.Sprintf("vitrine-epg-%d", os.Getpid()))
		pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V17).Port(uint32(port)).
			RuntimePath(run).DataPath(filepath.Join(run, "data")).
			CachePath(filepath.Join(home, ".cache", "embedded-postgres-go")).
			Logger(nil))
		if err := pg.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "embedded postgres:", err)
			os.Exit(1)
		}
		stop = func() { pg.Stop(); os.RemoveAll(run) }
		pgBase = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/", port)
	}
	code := m.Run()
	stop()
	os.Exit(code)
}

func freePort() int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// pgDatabase creates a fresh database and returns its URL.
func pgDatabase(t *testing.T) string {
	t.Helper()
	admin, err := sql.Open("pgx", pgBase+"postgres?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	name := "t_" + strings.ToLower(randID(6))
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		a, err := sql.Open("pgx", pgBase+"postgres?sslmode=disable")
		if err == nil {
			a.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
			a.Close()
		}
	})
	return pgBase + name + "?sslmode=disable"
}

// each runs f once per backend, with a fresh empty store.
func each(t *testing.T, f func(t *testing.T, st *Store)) {
	for _, b := range []string{"sqlite", "postgres"} {
		t.Run(b, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v.db")
			if b == "postgres" {
				path = pgDatabase(t)
			}
			st, err := openStore(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.close() })
			f(t, st)
		})
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRebind(t *testing.T) {
	d := &DB{pg: true}
	got := d.q(`SELECT a FROM t WHERE x=? AND y IN (?, 'users') AND z='it''s ? not' AND w<?`)
	want := `SELECT a FROM t WHERE x=$1 AND y IN ($2, 'users') AND z='it''s ? not' AND w<$3`
	if got != want {
		t.Fatalf("rebind:\n got %s\nwant %s", got, want)
	}
	if (&DB{}).q("a=?") != "a=?" {
		t.Fatal("sqlite queries must stay as written")
	}
}

func TestUsersSessionsTokens(t *testing.T) {
	each(t, func(t *testing.T, st *Store) {
		u, err := st.upsertUser("oidc|iss|1", "Ann@Example.com", "Ann")
		must(t, err)
		if u.Email != "ann@example.com" {
			t.Fatalf("email not lowercased: %q", u.Email)
		}
		u2, err := st.upsertUser("oidc|iss|1", "ann@example.com", "Ann B")
		must(t, err)
		if u2.ID != u.ID || u2.Name != "Ann B" {
			t.Fatalf("same subject must be the same user, renamed: %+v", u2)
		}
		// a CLI placeholder is adopted by the first real sign-in with that email
		p, err := st.upsertUser("local|bob@example.com", "bob@example.com", "")
		must(t, err)
		b, err := st.upsertUser("exedev|u9", "bob@example.com", "Bob")
		must(t, err)
		if b.ID != p.ID {
			t.Fatal("placeholder not adopted")
		}
		if got, _ := st.userByEmail("bob@example.com"); got == nil || got.ID != p.ID {
			t.Fatal("userByEmail")
		}
		tok, csrf, err := st.createSession(u.ID, time.Hour)
		must(t, err)
		ss, err := st.session(tok)
		if err != nil || ss.User.ID != u.ID || ss.CSRF != csrf {
			t.Fatalf("session: %v %+v", err, ss)
		}
		st.deleteSession(tok)
		if _, err := st.session(tok); err == nil {
			t.Fatal("deleted session still valid")
		}
		pat, err := st.createPAT(u.ID, "laptop")
		must(t, err)
		tr, err := st.lookupToken(pat)
		if err != nil || tr.Kind != "pat" || tr.UserID != u.ID || tr.Used {
			t.Fatalf("lookupToken: %v %+v", err, tr)
		}
		ts, _ := st.listTokens(u.ID)
		if len(ts) != 1 || ts[0].Name != "laptop" {
			t.Fatalf("listTokens: %+v", ts)
		}
		must(t, st.revokeToken(u.ID, ts[0].ID))
		if _, err := st.lookupToken(pat); err == nil {
			t.Fatal("revoked token still valid")
		}
		// expired sessions go in gc
		old, _, _ := st.createSession(u.ID, -time.Hour)
		st.gc()
		if _, err := st.session(old); err == nil {
			t.Fatal("gc kept an expired session")
		}
	})
}

func TestArtifactsAndVersions(t *testing.T) {
	each(t, func(t *testing.T, st *Store) {
		u, _ := st.upsertUser("dev|a", "a@example.com", "")
		page := "<!doctype html><h1>Città ✓ 🦭</h1>\x00"
		a, err := st.createArtifact(u.ID, kindHTML, "Page\x00", "first", page, json.RawMessage(`{"db":{}}`), "agent")
		must(t, err)
		if a.Title != "Page" {
			t.Fatalf("NUL kept in title: %q", a.Title)
		}
		got, _ := st.artifact(a.ID)
		if got.Kind != kindHTML || got.Head != 1 || string(got.Capabilities) != `{"db":{}}` {
			t.Fatalf("artifact: %+v", got)
		}
		big := strings.Repeat("x", 2<<20) + "end"
		a2, err := st.writeVersion(a.ID, 1, big, nil, nil, "agent", "")
		must(t, err)
		if a2.Head != 2 {
			t.Fatalf("head %d", a2.Head)
		}
		if _, err := st.writeVersion(a.ID, 1, "stale", nil, nil, "agent", ""); err != errConflict {
			t.Fatalf("stale base: %v", err)
		}
		h1, _ := st.versionHTML(a.ID, 1)
		h2, _ := st.versionHTML(a.ID, 2)
		if h1 != page || h2 != big {
			t.Fatal("version content changed on the way through the store")
		}
		vs, _ := st.versions(a.ID)
		if len(vs) != 2 || vs[0].N != 2 || vs[1].Size != len(page) {
			t.Fatalf("versions: %+v", vs)
		}
		title := "Renamed"
		st.setMeta(a.ID, &title, nil)
		must(t, st.setCapabilities(a.ID, json.RawMessage(`{}`)))
		md, err := st.createArtifact(u.ID, kindMarkdown, "Doc", "", "# Doc", nil, "agent")
		must(t, err)
		if m, _ := st.artifact(md.ID); m.Kind != kindMarkdown {
			t.Fatal("kind lost")
		}
		must(t, st.setGrant(a.ID, Grant{Audience: "users", Level: "view", Pinned: 1}))
		must(t, st.setGrant(a.ID, Grant{Audience: "users", Level: "interact"}))
		gs, _ := st.grants(a.ID)
		if len(gs) != 1 || gs[0].Level != "interact" || gs[0].Pinned != 0 {
			t.Fatalf("grant upsert: %+v", gs)
		}
		docs := &Caps{DB: &DBCap{}}
		v := Viewer{UID: u.ID, Level: lvOwner}
		if _, e := st.docWrite(a.ID, docs, v, WriteOp{Op: "set", Path: "items/a", Data: map[string]any{"n": 1}}, 1<<20, 100); e != nil {
			t.Fatal(e)
		}
		must(t, st.deleteArtifact(a.ID))
		for _, tbl := range []string{"artifacts", "versions", "grants", "docs", "artifacts_fts"} {
			var n int
			col := "artifact_id"
			if tbl == "artifacts" || tbl == "artifacts_fts" {
				col = "id"
			}
			st.db.QueryRow(`SELECT COUNT(*) FROM `+tbl+` WHERE `+col+`=?`, a.ID).Scan(&n)
			if n != 0 {
				t.Fatalf("%s rows left after delete", tbl)
			}
		}
	})
}

func TestSearch(t *testing.T) {
	each(t, func(t *testing.T, st *Store) {
		u, _ := st.upsertUser("dev|a", "a@example.com", "")
		other, _ := st.upsertUser("dev|b", "b@example.com", "")
		st.createArtifact(u.ID, kindHTML, "Walrus migration", "", "<p>The walruses are migrating north for the summer.</p>", nil, "x")
		st.createArtifact(u.ID, kindMarkdown, "Notes", "", "# Notes\n\nLa città di Pisa ha una torre pendente.", nil, "x")
		hidden, _ := st.createArtifact(other.ID, kindHTML, "Walrus secrets", "", "<p>walrus</p>", nil, "x")
		shared, _ := st.createArtifact(other.ID, kindHTML, "Shared walrus", "", "<p>walrus for everyone</p>", nil, "x")
		st.setGrant(shared.ID, Grant{Audience: "users", Level: "view"})
		titles := func(q string) []string {
			items, err := st.listFor(u, q, 50, 0)
			if err != nil {
				t.Fatalf("search %q: %v", q, err)
			}
			var out []string
			for _, it := range items {
				out = append(out, it.Title)
				if q != "" && it.Snippet == "" && it.Title == "Walrus migration" {
					t.Errorf("no snippet for %q", q)
				}
			}
			sort.Strings(out)
			return out
		}
		if got := titles("walr"); strings.Join(got, ",") != "Shared walrus,Walrus migration" {
			t.Fatalf("prefix search, own + shared only: %v", got)
		}
		if got := titles("walrus summer"); strings.Join(got, ",") != "Walrus migration" {
			t.Fatalf("all words must match: %v", got)
		}
		if got := titles("città"); strings.Join(got, ",") != "Notes" {
			t.Fatalf("non-ASCII word: %v", got)
		}
		for _, q := range []string{`" OR *`, `a:* | !b & (c`, `'; DROP TABLE artifacts; --`, `***`, `NEAR(x y)`} {
			titles(q) // must not error
		}
		items, _ := st.listFor(u, "migrating", 50, 0)
		if len(items) != 1 || !strings.Contains(items[0].Snippet, "«") {
			t.Fatalf("snippet should mark the match: %+v", items)
		}
		if got := titles(""); len(got) != 3 {
			t.Fatalf("list: %v", got)
		}
		_ = hidden
	})
}

func TestDocs(t *testing.T) {
	each(t, func(t *testing.T, st *Store) {
		u, _ := st.upsertUser("dev|a", "a@example.com", "")
		a, _ := st.createArtifact(u.ID, kindHTML, "db", "", "<p>x</p>", json.RawMessage(`{"db":{}}`), "x")
		c := &Caps{DB: &DBCap{}}
		v := Viewer{UID: u.ID, Level: lvOwner}
		w := func(op WriteOp) (*WriteResult, *DBError) { return st.docWrite(a.ID, c, v, op, 1<<20, 5) }
		if r, e := w(WriteOp{Op: "set", Path: "items/a", Data: map[string]any{"n": 1, "tags": []any{"x"}, "m": map[string]any{"k": 1}}}); e != nil || r.Version != 1 {
			t.Fatalf("set: %v %+v", e, r)
		}
		if r, e := w(WriteOp{Op: "update", Path: "items/a", Data: map[string]any{"m": map[string]any{"j": 2}}}); e != nil || r.Version != 2 {
			t.Fatalf("update: %v %+v", e, r)
		}
		d, _ := st.docGet(a.ID, c, v, "items/a")
		if m := d.Data["m"].(map[string]any); m["k"] == nil || m["j"] == nil {
			t.Fatalf("deep merge: %+v", d.Data)
		}
		one := 1
		if _, e := w(WriteOp{Op: "set", Path: "items/a", Data: map[string]any{}, IfVersion: &one}); e == nil || e.Code != "failed_precondition" {
			t.Fatalf("ifVersion mismatch: %v", e)
		}
		// ordering is bytewise on both backends
		for i, p := range []string{"items/b", "items/B", "items/_x", "items/Z1"} {
			w(WriteOp{Op: "set", Path: p, Data: map[string]any{"n": i + 2}})
		}
		docs, e := st.docQuery(a.ID, c, v, Query{Coll: "items"})
		if e != nil {
			t.Fatal(e)
		}
		var paths []string
		for _, d := range docs {
			paths = append(paths, d.Path)
		}
		want := append([]string(nil), paths...)
		sort.Strings(want)
		if strings.Join(paths, " ") != strings.Join(want, " ") || paths[0] != "items/B" {
			t.Fatalf("collection order not bytewise: %v", paths)
		}
		got, _ := st.docQuery(a.ID, c, v, Query{Coll: "items", Where: []Filter{{Field: "n", Op: ">", Value: 2.0}}, OrderBy: "n", Dir: "desc", Limit: 2})
		if len(got) != 2 || got[0].Data["n"].(float64) != 5 {
			t.Fatalf("where/orderBy/limit: %+v", got)
		}
		if _, e := w(WriteOp{Op: "set", Path: "items/f", Data: map[string]any{}}); e == nil || e.Code != "quota_exceeded" {
			t.Fatalf("quota: %v", e)
		}
		if _, e := w(WriteOp{Op: "delete", Path: "items/a"}); e != nil {
			t.Fatal(e)
		}
		if d, _ := st.docGet(a.ID, c, v, "items/a"); d.Exists {
			t.Fatal("deleted doc still there")
		}
		// leases
		acq := func(h string) bool {
			r, e := st.docAcquire(a.ID, c, v, "jobs/j1", h, 1000, nil, 1<<20)
			if e != nil {
				t.Fatal(e)
			}
			return r.Acquired
		}
		if !acq("A") || acq("B") || !acq("A") {
			t.Fatal("lease: A gets it, B doesn't, A renews")
		}
		time.Sleep(1100 * time.Millisecond)
		if !acq("B") {
			t.Fatal("lease: B gets it after it expires")
		}
	})
}

// parallel runs f n times at once and returns how many returned true.
func parallel(n int, f func(i int) bool) int {
	var ok atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if f(i) {
				ok.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return int(ok.Load())
}

func TestConcurrentWriters(t *testing.T) {
	const N = 16
	each(t, func(t *testing.T, st *Store) {
		u, _ := st.upsertUser("dev|a", "a@example.com", "")
		a, _ := st.createArtifact(u.ID, kindHTML, "c", "", "<p>1</p>", json.RawMessage(`{"db":{}}`), "x")
		c := &Caps{DB: &DBCap{}}
		v := Viewer{UID: u.ID, Level: lvOwner}
		var unexpected atomic.Value
		won := parallel(N, func(i int) bool {
			_, err := st.writeVersion(a.ID, 1, fmt.Sprintf("<p>%d</p>", i), nil, nil, "x", "")
			if err != nil && err != errConflict {
				unexpected.Store(err)
			}
			return err == nil
		})
		if won != 1 || unexpected.Load() != nil {
			t.Fatalf("versions from the same base: %d won (want 1), unexpected error %v", won, unexpected.Load())
		}
		if vs, _ := st.versions(a.ID); len(vs) != 2 {
			t.Fatalf("%d versions, want 2", len(vs))
		}
		zero := 0
		won = parallel(N, func(i int) bool {
			_, e := st.docWrite(a.ID, c, v, WriteOp{Op: "set", Path: "c/new", Data: map[string]any{"i": i}, IfVersion: &zero}, 1<<20, 1000)
			if e != nil && e.Code != "failed_precondition" {
				unexpected.Store(errors.New(e.Error()))
			}
			return e == nil
		})
		if won != 1 || unexpected.Load() != nil {
			t.Fatalf("create with ifVersion 0: %d won (want 1), unexpected %v", won, unexpected.Load())
		}
		won = parallel(N, func(i int) bool {
			_, e := st.docWrite(a.ID, c, v, WriteOp{Op: "set", Path: "c/plain", Data: map[string]any{"i": i}}, 1<<20, 1000)
			return e == nil
		})
		d, _ := st.docGet(a.ID, c, v, "c/plain")
		if won != N || d.Version != N {
			t.Fatalf("plain sets: %d succeeded, version %d (want %d, %d: no lost writes)", won, d.Version, N, N)
		}
		won = parallel(N, func(i int) bool {
			ver := d.Version
			_, e := st.docWrite(a.ID, c, v, WriteOp{Op: "update", Path: "c/plain", Data: map[string]any{"j": i}, IfVersion: &ver}, 1<<20, 1000)
			return e == nil
		})
		if won != 1 {
			t.Fatalf("compare-and-set on an existing doc: %d won, want 1", won)
		}
		won = parallel(N, func(i int) bool {
			r, e := st.docAcquire(a.ID, c, v, "locks/l", fmt.Sprintf("h%d", i), 30000, nil, 1<<20)
			return e == nil && r.Acquired
		})
		if won != 1 {
			t.Fatalf("lease on a new doc: %d holders, want 1", won)
		}
		ids := sync.Map{}
		parallel(N, func(i int) bool {
			u, err := st.upsertUser("oidc|x|same", "same@example.com", "")
			if err == nil {
				ids.Store(u.ID, true)
			}
			return err == nil
		})
		n := 0
		ids.Range(func(_, _ any) bool { n++; return true })
		if n != 1 {
			t.Fatalf("first sign-in raced into %d users", n)
		}
		// the token endpoint's atomic consumes
		st.db.Exec(`INSERT INTO oauth_codes(code_hash, client_id, user_id, redirect_uri, challenge, expires_at) VALUES(?,?,?,?,?,?)`, "h1", "c", u.ID, "r", "x", now()+60)
		won = parallel(N, func(i int) bool {
			var cid string
			return st.db.QueryRow(`DELETE FROM oauth_codes WHERE code_hash=? RETURNING client_id`, "h1").Scan(&cid) == nil
		})
		if won != 1 {
			t.Fatalf("code redeemed %d times", won)
		}
		rt, _ := st.insertToken("refresh", u.ID, "c", "fam", time.Hour)
		tr, _ := st.lookupToken(rt)
		won = parallel(N, func(i int) bool {
			res, err := st.db.Exec(`UPDATE tokens SET used=1 WHERE id=? AND used=0`, tr.ID)
			if err != nil {
				return false
			}
			n, _ := res.RowsAffected()
			return n == 1
		})
		if won != 1 {
			t.Fatalf("refresh token rotated %d times", won)
		}
	})
}

func TestPostgresMigrationsUnderLock(t *testing.T) {
	url := pgDatabase(t)
	errs := make(chan error, 4)
	parallel(4, func(int) bool {
		st, err := openStore(url)
		if err == nil {
			st.close()
		}
		errs <- err
		return err == nil
	})
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("replicas starting together: %v", err)
		}
	}
	st, _ := openStore(url)
	defer st.close()
	var n, v int
	st.db.QueryRow(`SELECT COUNT(*), MAX(version) FROM schema_migrations`).Scan(&n, &v)
	if n != len(pgMigrations) || v != len(pgMigrations) {
		t.Fatalf("schema_migrations: %d rows, max %d", n, v)
	}
}

func TestMigrateStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src, _ := openStore(filepath.Join(dir, "src.db"))
	defer src.close()
	u, _ := src.upsertUser("exedev|1", "a@example.com", "A")
	a, _ := src.createArtifact(u.ID, kindHTML, "Walrus page", "d", "<p>walrus 🦭</p>", json.RawMessage(`{"db":{}}`), "x")
	src.writeVersion(a.ID, 1, "<p>walrus v2</p>", nil, nil, "x", "")
	m, _ := src.createArtifact(u.ID, kindMarkdown, "Doc", "", "# Doc\n\nnarwhal", nil, "x")
	src.setGrant(a.ID, Grant{Audience: "public", Level: "view", Pinned: 1})
	src.docWrite(a.ID, &Caps{DB: &DBCap{}}, Viewer{UID: u.ID, Level: lvOwner}, WriteOp{Op: "set", Path: "items/a", Data: map[string]any{"n": 1}}, 1<<20, 100)
	src.createPAT(u.ID, "cli")

	pgURL := pgDatabase(t)
	pg, err := openStore(pgURL)
	must(t, err)
	defer pg.close()
	n, err := migrateStore(src, pg)
	must(t, err)
	if n["versions"] != 3 || n["docs"] != 1 || n["tokens"] != 1 {
		t.Fatalf("counts: %v", n)
	}
	if _, err := migrateStore(src, pg); err == nil {
		t.Fatal("copying into a non-empty store must be refused")
	}
	back, _ := openStore(filepath.Join(dir, "back.db"))
	defer back.close()
	_, err = migrateStore(pg, back)
	must(t, err)
	for _, st := range []*Store{pg, back} {
		pu, _ := st.userByEmail("a@example.com")
		items, err := st.listFor(pu, "walrus", 10, 0)
		if err != nil || len(items) != 1 || items[0].ID != a.ID {
			t.Fatalf("search after copy: %v %+v", err, items)
		}
		if items, _ := st.listFor(pu, "narwhal", 10, 0); len(items) != 1 || items[0].Kind != kindMarkdown {
			t.Fatalf("markdown after copy: %+v", items)
		}
		h, _ := st.versionHTML(a.ID, 1)
		if h != "<p>walrus 🦭</p>" {
			t.Fatalf("version 1 after copy: %q", h)
		}
		gs, _ := st.grants(a.ID)
		if len(gs) != 1 || gs[0].Pinned != 1 {
			t.Fatalf("grants after copy: %+v", gs)
		}
		d, _ := st.docGet(a.ID, &Caps{DB: &DBCap{}}, Viewer{UID: pu.ID, Level: lvOwner}, "items/a")
		if !d.Exists || d.Data["n"].(float64) != 1 {
			t.Fatalf("doc after copy: %+v", d)
		}
	}
	_ = m
}
