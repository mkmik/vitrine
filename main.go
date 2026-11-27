// vitrine is a self-hosted artifact server: agents create, read and iterate on
// single-file HTML artifacts over MCP; people view them in a shell that live
// reloads, shares, and gives pages a Claude-artifacts-compatible runtime
// (db, room, sample, user).
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

type Config struct {
	Addr              string
	BaseURL           string
	DBPath            string
	Auth              string // exedev | oidc | proxy | dev (see auth.go)
	OIDCIssuer        string
	OIDCClientID      string
	OIDCSecret        string
	OIDCScopes        string
	ProxyEmailHeader  string // proxy mode, plain headers
	ProxyUserHeader   string
	ProxyNameHeader   string
	ProxyGroupsHeader string
	ProxySecret       string // optional "Header: value" the proxy adds
	ProxyLoginURL     string // optional proxy start page, {rd} = return URL
	ProxyJWTHeader    string // proxy mode, signed JWT header instead of plain ones
	ProxyJWKS         string
	ProxyJWTIssuer    string
	ProxyJWTAudience  string
	TrustedProxies    []string // addresses/CIDRs of proxies in front, or "*"
	GroupsClaim       string
	AllowGroups       []string // groups allowed to sign in; empty = any
	LogoutURL         string   // where the browser goes after sign-out
	AllowSignIn       []string // emails or @domains; empty = anyone the IdP signs in
	Creators          []string // emails or @domains allowed to create; empty = any user
	OrgName           string   // shown as "Anyone at <OrgName>" in the share dialog
	RequireSignIn     bool     // every page needs a signed-in user, even links shared with "public"
	LLMURL            string   // Anthropic-Messages-compatible base, e.g. https://llm.int.exe.xyz
	LLMHeader         string   // optional "Name: value" added to LLM requests
	ModelQuick        string
	ModelDefault      string
	ModelComplex      string
	CDNs              []string
	RedirectHosts     []string // extra hosts allowed as OAuth redirect URIs
	MaxVersionBytes   int
	MaxDocBytes       int
	MaxDocs           int
	SampleRPM         int
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func loadConfig() *Config {
	addr := env("VITRINE_ADDR", "127.0.0.1:8787")
	if os.Getenv("VITRINE_ADDR") == "" && os.Getenv("PORT") != "" { // Cloud Run, Heroku-style platforms
		addr = "0.0.0.0:" + os.Getenv("PORT")
	}
	return &Config{
		Addr:              addr,
		BaseURL:           strings.TrimRight(env("VITRINE_BASE_URL", "http://127.0.0.1:8787"), "/"),
		DBPath:            env("VITRINE_DB", "vitrine.db"),
		Auth:              env("VITRINE_AUTH", ""),
		OIDCIssuer:        env("VITRINE_OIDC_ISSUER", ""),
		OIDCClientID:      env("VITRINE_OIDC_CLIENT_ID", ""),
		OIDCSecret:        env("VITRINE_OIDC_CLIENT_SECRET", ""),
		OIDCScopes:        env("VITRINE_OIDC_SCOPES", "openid email profile"),
		ProxyEmailHeader:  env("VITRINE_PROXY_EMAIL_HEADER", "X-Forwarded-Email"),
		ProxyUserHeader:   env("VITRINE_PROXY_USER_HEADER", "X-Forwarded-User"),
		ProxyNameHeader:   env("VITRINE_PROXY_NAME_HEADER", "X-Forwarded-Preferred-Username"),
		ProxyGroupsHeader: env("VITRINE_PROXY_GROUPS_HEADER", "X-Forwarded-Groups"),
		ProxySecret:       env("VITRINE_PROXY_SECRET", ""),
		ProxyLoginURL:     env("VITRINE_PROXY_LOGIN_URL", ""),
		ProxyJWTHeader:    env("VITRINE_PROXY_JWT_HEADER", ""),
		ProxyJWKS:         env("VITRINE_PROXY_JWT_JWKS", ""),
		ProxyJWTIssuer:    env("VITRINE_PROXY_JWT_ISSUER", ""),
		ProxyJWTAudience:  env("VITRINE_PROXY_JWT_AUDIENCE", ""),
		TrustedProxies:    splitList(env("VITRINE_TRUSTED_PROXIES", "")),
		GroupsClaim:       env("VITRINE_GROUPS_CLAIM", "groups"),
		AllowGroups:       splitList(env("VITRINE_ALLOW_GROUPS", "")),
		LogoutURL:         env("VITRINE_LOGOUT_URL", ""),
		AllowSignIn:       splitList(env("VITRINE_ALLOW_SIGNIN", "")),
		Creators:          splitList(env("VITRINE_CREATORS", "")),
		OrgName:           env("VITRINE_ORG_NAME", ""),
		RequireSignIn:     env("VITRINE_REQUIRE_SIGNIN", "") == "true",
		LLMURL:            strings.TrimRight(env("VITRINE_LLM_URL", ""), "/"),
		LLMHeader:         env("VITRINE_LLM_HEADER", ""),
		ModelQuick:        env("VITRINE_MODEL_QUICK", "claude-haiku-5-5"),
		ModelDefault:      env("VITRINE_MODEL_DEFAULT", "claude-sonnet-5-5"),
		ModelComplex:      env("VITRINE_MODEL_COMPLEX", "claude-opus-5-5"),
		CDNs:              splitList(env("VITRINE_CDNS", "https://cdnjs.cloudflare.com https://cdn.jsdelivr.net https://unpkg.com https://esm.sh https://cdn.tailwindcss.com https://fonts.googleapis.com https://fonts.gstatic.com")),
		RedirectHosts:     splitList(env("VITRINE_OAUTH_REDIRECT_HOSTS", "")),
		MaxVersionBytes:   envInt("VITRINE_MAX_VERSION_BYTES", 16<<20),
		MaxDocBytes:       envInt("VITRINE_MAX_DOC_BYTES", 256<<10),
		MaxDocs:           envInt("VITRINE_MAX_DOCS", 25000),
		SampleRPM:         envInt("VITRINE_SAMPLE_RPM", 30),
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			cmdBackup(os.Args[2:])
			return
		case "import":
			cmdImport(os.Args[2:])
			return
		case "migrate-store":
			cmdMigrateStore(os.Args[2:])
			return
		case "token":
			cmdToken(os.Args[2:])
			return
		case "version":
			fmt.Println("vitrine", version)
			return
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
	flag.Parse()
	cfg := loadConfig()
	st, err := openStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	srv, err := newServer(cfg, st)
	if err != nil {
		log.Fatalf("server: %v", err)
	}
	hs := &http.Server{Addr: cfg.Addr, Handler: srv.routes(), ReadHeaderTimeout: 15 * time.Second}
	go srv.gcLoop()
	go func() {
		log.Printf("vitrine %s listening on %s (base %s, sign-in: %s)", version, cfg.Addr, cfg.BaseURL, srv.auth.describe())
		if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(ctx)
	st.db.Close()
}

func cmdBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	out := fs.String("o", "", "output file (required)")
	fs.Parse(args)
	if *out == "" {
		log.Fatal("backup: -o required")
	}
	path := loadConfig().DBPath
	if isPostgresURL(path) {
		log.Fatal("backup copies a SQLite store; for Postgres use the database's own backups (pg_dump, point-in-time recovery), or vitrine migrate-store -to file.db")
	}
	st, err := openStore(path)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := st.db.Exec(`VACUUM INTO ?`, *out); err != nil {
		log.Fatal(err)
	}
	fmt.Println("backup written to", *out)
}

// token mints a personal API token for a user (by email) from the command line.
func cmdToken(args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	email := fs.String("email", "", "user email (must have signed in once, or use -create)")
	name := fs.String("name", "cli", "token name")
	create := fs.Bool("create", false, "create the user if missing")
	fs.Parse(args)
	e := strings.ToLower(strings.TrimSpace(*email))
	if e == "" {
		log.Fatal("token: -email required") // with -create, "" would make a user with no email
	}
	st, err := openStore(loadConfig().DBPath)
	if err != nil {
		log.Fatal(err)
	}
	u, err := st.userByEmail(e)
	if err != nil && *create {
		u, err = st.upsertUser("local|"+e, e, "")
	}
	if err != nil {
		log.Fatalf("user %s: %v", *email, err)
	}
	tok, err := st.createPAT(u.ID, *name)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(tok)
}
