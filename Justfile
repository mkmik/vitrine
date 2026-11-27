# Local dev vitrine plus agent sessions wired to it via MCP.
# Run `just serve` in one shell, then `just claude` or `just codex` in another.
# Override the port with `just port=9000 <recipe>` or VITRINE_DEV_PORT=9000.
# Sign in as the same user to see what the agent made:
#   http://127.0.0.1:<port>/auth/login?email=me@example.com

port := env("VITRINE_DEV_PORT", "28787")
email := "me@example.com"

export VITRINE_AUTH := "dev"
export VITRINE_ADDR := "127.0.0.1:" + port
export VITRINE_BASE_URL := "http://127.0.0.1:" + port
export VITRINE_DB := env("VITRINE_DB", "/tmp/vitrine-dev.db")

# mint a token (same VITRINE_DB as the server; -create makes the user)
_token := 'set -euo pipefail; export VITRINE_TOKEN=$(go run . token -email ' + email + ' -create)'

# run the dev server (dev sign-in only works on loopback)
serve:
    go run .

# claude with only the vitrine-dev MCP; extra args go to claude
[positional-arguments]
claude *args:
    #!/usr/bin/env bash
    {{_token}}
    claude --strict-mcp-config --mcp-config "{\"mcpServers\":{\"vitrine-dev\":{\"type\":\"http\",\"url\":\"$VITRINE_BASE_URL/mcp\",\"headers\":{\"Authorization\":\"Bearer $VITRINE_TOKEN\"}}}}" "$@"

# codex with a vitrine-dev MCP added; extra args go to codex
[positional-arguments]
codex *args:
    #!/usr/bin/env bash
    {{_token}}
    codex -c "mcp_servers.vitrine-dev.url=\"$VITRINE_BASE_URL/mcp\"" -c 'mcp_servers.vitrine-dev.bearer_token_env_var="VITRINE_TOKEN"' "$@"
