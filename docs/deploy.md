# Deploy and upgrade

On an exe.dev VM:

```
deploy/install.sh          # builds, installs /usr/local/bin/vitrine, systemd unit, daily backup timer
vitrinectl token -email you@example.com -name laptop   # personal API token
vitrinectl import -owner you@example.com page.html doc.md...  # import HTML pages and markdown documents
vitrinectl backup -o /var/lib/vitrine/backups/now.db
vitrine migrate-store -from vitrine.db -to postgres://...   # or back: -from postgres://... -to copy.db (destination must be empty)
```

Backups: `vitrine-backup.timer` writes `/var/lib/vitrine/backups/vitrine-YYYY-MM-DD.db` daily, keeps 14 days.

Elsewhere: run the binary behind any TLS proxy or ingress that passes
WebSockets, with `VITRINE_AUTH=oidc` or `proxy`. With SQLite that is one
replica with a persistent volume; several replicas (and Cloud Run) need the
Postgres store from the PRD addendum.
