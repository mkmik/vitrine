# vitrine

Self-hosted artifact server. Agents create, read and iterate on single-file
HTML pages and markdown documents over MCP. People open a stable link that
live reloads, can be shared, keeps history, and gives pages a runtime
compatible with Claude artifacts (`await claude.use("db" | "room" | "sample" | "user")`).

One Go binary, one SQLite file (or Postgres).

## Quick start

```
go build .
VITRINE_AUTH=dev ./vitrine     # local only: sign in with any email, no password
```

Open http://127.0.0.1:8787, then connect an agent from `/settings`
(MCP at `/mcp`, OAuth or a personal token).

## Docs

- [Feature map](docs/map.md): what works, what is partial, what is missing
- [Configure](docs/configure.md): environment variables and sign-in methods
- [Deploy and upgrade](docs/deploy.md): exe.dev install, backups, moving to Postgres
- [Development](docs/development.md): code layout, routes, tests
- [End-to-end tests](e2e/README.md)

## License

MIT, see [LICENSE](LICENSE).
