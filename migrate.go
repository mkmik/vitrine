package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"strings"
)

// copyTables are copied in this order; the search index is rebuilt instead.
var copyTables = []string{"users", "sessions", "tokens", "oauth_clients", "oauth_codes", "login_states", "artifacts", "versions", "grants", "docs", "thumbs"}

// cmdMigrateStore copies a whole store between backends, either way:
//
//	vitrine migrate-store -from /var/lib/vitrine/vitrine.db -to postgres://...
//	vitrine migrate-store -from postgres://... -to copy.db
//
// The destination must be empty. Stop the server first (or copy a backup).
func cmdMigrateStore(args []string) {
	fs := flag.NewFlagSet("migrate-store", flag.ExitOnError)
	from := fs.String("from", "", "source: SQLite file or postgres:// URL")
	to := fs.String("to", "", "destination: SQLite file or postgres:// URL (must be empty)")
	fs.Parse(args)
	if *from == "" || *to == "" {
		log.Fatal("usage: vitrine migrate-store -from SRC -to DST")
	}
	src, err := openStore(*from)
	if err != nil {
		log.Fatalf("source: %v", err)
	}
	dst, err := openStore(*to)
	if err != nil {
		log.Fatalf("destination: %v", err)
	}
	n, err := migrateStore(src, dst)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range copyTables {
		fmt.Printf("%-14s %d\n", t, n[t])
	}
	fmt.Printf("%-14s %d (rebuilt)\n", "search index", n["artifacts"])
}

func migrateStore(src, dst *Store) (map[string]int, error) {
	for _, t := range []string{"users", "artifacts"} {
		var n int
		if err := dst.db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return nil, fmt.Errorf("destination is not empty (%d rows in %s)", n, t)
		}
	}
	tx, err := dst.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	counts := map[string]int{}
	for _, t := range copyTables {
		rows, err := src.db.Query(`SELECT * FROM ` + t)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
		cols, _ := rows.Columns()
		ins := `INSERT INTO ` + t + `(` + strings.Join(cols, ", ") + `) VALUES(` + strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",") + `)`
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: %w", t, err)
			}
			for i, v := range vals {
				if b, ok := v.([]byte); ok && cols[i] != "html" && cols[i] != "data" { // SQLite may hand text back as bytes
					vals[i] = string(b)
				} else if s, ok := v.(string); ok {
					vals[i] = noNUL(s)
				}
			}
			if _, err := tx.Exec(ins, vals...); err != nil {
				rows.Close()
				return nil, fmt.Errorf("%s: %w", t, err)
			}
			counts[t]++
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", t, err)
		}
	}
	// search index: from each artifact's head
	arts, err := src.db.Query(`SELECT ` + artifactCols + ` FROM artifacts`)
	if err != nil {
		return nil, err
	}
	var list []*Artifact
	for arts.Next() {
		a, err := scanArtifact(arts)
		if err != nil {
			arts.Close()
			return nil, err
		}
		list = append(list, a)
	}
	arts.Close()
	for _, a := range list {
		html, err := src.versionHTML(a.ID, a.Head)
		if err != nil && err != sql.ErrNoRows {
			return nil, fmt.Errorf("artifact %s: %w", a.ID, err)
		}
		if err := dst.indexFTS(tx, a, html); err != nil {
			return nil, fmt.Errorf("index %s: %w", a.ID, err)
		}
	}
	return counts, tx.Commit()
}
