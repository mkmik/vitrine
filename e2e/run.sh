#!/bin/sh
# Run vitrine's end-to-end suites against a local dev server (Linux).
#
#   e2e/run.sh                    all suites on SQLite
#   e2e/run.sh postgres           all suites on a real Postgres (embedded-postgres)
#   e2e/run.sh sqlite md_test     one suite
#
# Needs Go, and a Python with playwright, cryptography and pyjwt, with
# Chromium, Firefox and WebKit installed (playwright install). Set PYTHON to
# pick the interpreter, VITRINE_LLM_URL to exercise the `sample` capability.
set -e
cd "$(dirname "$0")/.."
BACKEND=${1:-sqlite}
[ $# -gt 0 ] && shift
SUITES=${*:-"mcp_test oauth_test browser_test offline_test md_test harden_test auth_test"}
PY=${PYTHON:-python3}
mkdir -p /tmp/vt /tmp/rtcx
go build -o /tmp/vitrine-new . && go build -o /tmp/pgtest ./cmd/pgtest

pids=""
cleanup() { pkill -f '^/tmp/vitrine-new' 2>/dev/null || true; for p in $pids; do kill "$p" 2>/dev/null || true; done; }
trap cleanup EXIT INT TERM
# harden_test: a listener that must see no connections, and a page server for links it opens
"$PY" e2e/listen.py > /tmp/rtcx/listen.log 2>&1 & pids="$pids $!"
"$PY" -m http.server 8893 --bind 127.0.0.1 > /dev/null 2>&1 & pids="$pids $!"
if [ "$BACKEND" = postgres ]; then
  /tmp/pgtest serve -port 5439 -dir /tmp/vt/pg > /tmp/vt/pg.log 2>&1 & pids="$pids $!"
  for i in $(seq 1 120); do grep -q ready /tmp/vt/pg.log && break; sleep 0.5; done
fi

stop() {
  pkill -f '^/tmp/vitrine-new' 2>/dev/null || true
  while pgrep -f '^/tmp/vitrine-new' > /dev/null; do sleep 0.1; done
}

start() {
  stop
  if [ "$BACKEND" = postgres ]; then
    DB=$(/tmp/pgtest fresh -port 5439 -db vt)
  else
    rm -f /tmp/vt/test.db*; DB=/tmp/vt/test.db
  fi
  export VITRINE_AUTH=dev VITRINE_ADDR=127.0.0.1:8899 VITRINE_BASE_URL=http://127.0.0.1:8899 VITRINE_DB="$DB"
  /tmp/vitrine-new token -email marko@test -name cli -create | grep -o 'vit_[A-Za-z0-9_-]*' > /tmp/vt/tok
  /tmp/vitrine-new serve > /tmp/vt/log 2>&1 &
  for i in $(seq 1 100); do curl -s -o /dev/null "$VITRINE_BASE_URL/" && break; sleep 0.1; done
}

failed=0
for s in $SUITES; do
  echo "== $s ($BACKEND)"
  case $s in
    oauth_test) ;;                 # continues from mcp_test's server and artifact
    auth_test) stop ;;   # starts its own servers
    *) start ;;
  esac
  if [ "$s" = auth_test ] && [ "$BACKEND" = postgres ]; then
    AUTH_PG=1 "$PY" "e2e/$s.py" > /tmp/vt/out 2>&1 || true
  else
    "$PY" "e2e/$s.py" > /tmp/vt/out 2>&1 || true
  fi
  grep -v '^PASS' /tmp/vt/out || true
  grep -q '^FAILS: 0$' /tmp/vt/out || failed=1
done
exit $failed
