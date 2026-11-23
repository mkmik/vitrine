// pgtest runs a throwaway Postgres for vitrine's end-to-end tests.
//
//	pgtest serve -port 5439 -dir /tmp/pgtest   # real Postgres (embedded-postgres), until SIGTERM
//	pgtest fresh -port 5439 -db vt              # drop and recreate a database, print its URL
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: pgtest serve|fresh [flags]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	port := fs.Int("port", 5439, "port")
	dir := fs.String("dir", filepath.Join(os.TempDir(), "pgtest"), "runtime directory (serve)")
	db := fs.String("db", "vt", "database name (fresh)")
	fs.Parse(os.Args[2:])
	base := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/", *port)
	switch os.Args[1] {
	case "serve":
		home, _ := os.UserHomeDir()
		os.RemoveAll(*dir)
		pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V17).Port(uint32(*port)).
			RuntimePath(*dir).DataPath(filepath.Join(*dir, "data")).
			CachePath(filepath.Join(home, ".cache", "embedded-postgres-go")))
		if err := pg.Start(); err != nil {
			log.Fatal(err)
		}
		log.Printf("postgres ready at %spostgres", base)
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		pg.Stop()
	case "fresh":
		a, err := sql.Open("pgx", base+"postgres?sslmode=disable")
		if err != nil {
			log.Fatal(err)
		}
		if _, err := a.Exec(`DROP DATABASE IF EXISTS ` + *db + ` WITH (FORCE)`); err != nil {
			log.Fatal(err)
		}
		if _, err := a.Exec(`CREATE DATABASE ` + *db); err != nil {
			log.Fatal(err)
		}
		fmt.Println(base + *db + "?sslmode=disable")
	default:
		log.Fatal("usage: pgtest serve|fresh [flags]")
	}
}
