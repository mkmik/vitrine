# End-to-end suites

Black-box tests against a running vitrine: MCP and OAuth over HTTP, and real
browsers (Playwright: Chromium, Firefox, WebKit) for the viewer, the page
runtime, markdown documents, the page hardening and sign-in.

| Suite | Covers |
|---|---|
| `mcp_test.py` | MCP tools, conflicts, edits, sharing, search |
| `oauth_test.py` | OAuth 2.1 for MCP clients: registration, PKCE, consent, rotation, reuse; pinned and public views (runs after `mcp_test`) |
| `browser_test.py` | shell, live reload, db / room / user across users and tabs, `sample` with consent |
| `offline_test.py` | saved copies first (`fromCache`), HTTP fallback, polling |
| `md_test.py` | markdown documents: rendering, sanitizer, images, links, reading position |
| `harden_test.py` | WebRTC, child frames, self-navigation, popups and network hints, in three engines; fails against builds without the hardening |
| `auth_test.py` | sign-in methods (oidc against `mock_idp.py`, proxy headers / secret / JWT, dev) and the configurations that must refuse to start |

Run them with `e2e/run.sh` (SQLite) or `e2e/run.sh postgres`. Only failures
are printed; each suite ends with `FAILS: n`. `browser_test`'s vote check is
known to be flaky (the test writes one value too many), and its `sample`
checks need `VITRINE_LLM_URL`.
