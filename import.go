package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var titleRE = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// cmdImport imports HTML files as new artifacts owned by a user:
//
//	vitrine import -owner you@example.com [-title T] file.html...
//
// For a path like .../u/Hello_World-16c166eb/index.html the title defaults to
// the page's <title>, else the directory name without its hash suffix.
func cmdImport(args []string) {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	owner := fs.String("owner", "", "owner email (required; created if missing)")
	title := fs.String("title", "", "title (single file only)")
	fs.Parse(args)
	if *owner == "" || fs.NArg() == 0 {
		log.Fatal("usage: vitrine import -owner EMAIL file.html...")
	}
	cfg := loadConfig()
	st, err := openStore(cfg.DBPath)
	if err != nil {
		log.Fatal(err)
	}
	u, err := st.userByEmail(strings.ToLower(*owner))
	if err != nil {
		if u, err = st.upsertUser("local|"+strings.ToLower(*owner), *owner, ""); err != nil {
			log.Fatal(err)
		}
	}
	for _, f := range fs.Args() {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatal(err)
		}
		t := *title
		if t == "" {
			if m := titleRE.FindSubmatch(b); m != nil {
				t = strings.TrimSpace(string(m[1]))
			}
		}
		if t == "" {
			d := filepath.Base(filepath.Dir(f))
			if i := strings.LastIndex(d, "-"); i > 0 {
				d = d[:i]
			}
			t = strings.ReplaceAll(d, "_", " ")
		}
		a, err := st.createArtifact(u.ID, t, "imported", string(b), nil, "import")
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%s\t%s\t%s/a/%s\n", f, t, cfg.BaseURL, a.ID)
	}
}
