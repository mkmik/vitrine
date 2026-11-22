#!/bin/sh
# Installs vitrine as a systemd service on an exe.dev VM. Re-run to upgrade.
set -eu
cd "$(dirname "$0")/.."
go build -trimpath -o vitrine .
sudo install -m 755 vitrine /usr/local/bin/vitrine
id vitrine >/dev/null 2>&1 || sudo useradd --system --home /var/lib/vitrine --shell /usr/sbin/nologin vitrine
sudo install -d -o vitrine -g vitrine -m 750 /var/lib/vitrine /var/lib/vitrine/backups
if [ ! -f /etc/vitrine/env ]; then
  sudo install -d -m 755 /etc/vitrine
  sudo tee /etc/vitrine/env >/dev/null <<'EOF'
# vitrine configuration (read by vitrine.service and vitrinectl)
VITRINE_ADDR=0.0.0.0:8787
VITRINE_BASE_URL=https://artifacts.example.com
VITRINE_DB=/var/lib/vitrine/vitrine.db
# exedev: trust exe.dev's proxy login headers. oidc: any IdP (set VITRINE_OIDC_*).
VITRINE_AUTH=exedev
# who may sign in at all (emails or @domains, comma-separated); empty = anyone the IdP signs in
VITRINE_ALLOW_SIGNIN=
# who may create artifacts; others can only open what is shared with them
VITRINE_CREATORS=you@example.com
# model gateway for the page `sample` capability (exe.dev LLM integration: no key)
VITRINE_LLM_URL=https://llm.int.exe.xyz
VITRINE_MODEL_QUICK=claude-haiku-5-5
VITRINE_MODEL_DEFAULT=claude-sonnet-5-5
VITRINE_MODEL_COMPLEX=claude-opus-5-5
EOF
fi
sudo tee /etc/systemd/system/vitrine.service >/dev/null <<'EOF'
[Unit]
Description=vitrine artifact server
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=/etc/vitrine/env
ExecStart=/usr/local/bin/vitrine serve
User=vitrine
Group=vitrine
WorkingDirectory=/var/lib/vitrine
Restart=always
RestartSec=2
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/var/lib/vitrine

[Install]
WantedBy=multi-user.target
EOF
sudo tee /etc/systemd/system/vitrine-backup.service >/dev/null <<'EOF'
[Unit]
Description=vitrine daily SQLite backup

[Service]
Type=oneshot
EnvironmentFile=/etc/vitrine/env
User=vitrine
ExecStart=/bin/sh -c '/usr/local/bin/vitrine backup -o /var/lib/vitrine/backups/vitrine-$(date +%%F).db && find /var/lib/vitrine/backups -name "vitrine-*.db" -mtime +14 -delete'
EOF
sudo tee /etc/systemd/system/vitrine-backup.timer >/dev/null <<'EOF'
[Unit]
Description=daily vitrine backup

[Timer]
OnCalendar=*-*-* 03:17:00
Persistent=true

[Install]
WantedBy=timers.target
EOF
sudo tee /usr/local/bin/vitrinectl >/dev/null <<'EOF'
#!/bin/sh
# Run a vitrine CLI command (token, import, backup) against the live database.
exec sudo -u vitrine sh -c 'set -a; . /etc/vitrine/env; set +a; cd /var/lib/vitrine; exec /usr/local/bin/vitrine "$@"' vitrine "$@"
EOF
sudo chmod 755 /usr/local/bin/vitrinectl
sudo systemctl daemon-reload
sudo systemctl enable --now vitrine-backup.timer
sudo systemctl enable vitrine
sudo systemctl restart vitrine
sleep 1
systemctl --no-pager --lines=3 status vitrine
