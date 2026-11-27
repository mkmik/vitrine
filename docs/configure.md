# Configure

Everything is environment variables (on an exe.dev VM: `/etc/vitrine/env`).

```
VITRINE_BASE_URL=https://artifacts.example.com   # the public URL: OAuth metadata, cookies, CSP and WebSocket checks use it
VITRINE_ADDR=0.0.0.0:8787       # default 127.0.0.1:8787; if unset and PORT is set (Cloud Run): 0.0.0.0:$PORT
VITRINE_DB=/var/lib/vitrine/vitrine.db   # a SQLite file, or postgres://user:pass@host/db (one replica until the event bus lands)
VITRINE_AUTH=                   # required: exedev | oidc | proxy | dev (below)
VITRINE_ALLOW_SIGNIN=           # emails/@domains allowed to sign in; empty = anyone the sign-in method accepts
VITRINE_ALLOW_GROUPS=           # groups allowed to sign in (oidc claim or proxy header); empty = any
VITRINE_CREATORS=you@example.com   # who may create artifacts; empty = anyone signed in
VITRINE_REQUIRE_SIGNIN=          # true: every page needs a signed-in user; "public" links then open only after sign-in
VITRINE_DISABLE_PUBLIC_SHARING=  # true: no "anyone with the link" sharing, only people, everyone who can sign in, and everyone who can sign in and has the link; public grants made earlier stop working
VITRINE_ORG_NAME=               # e.g. "Example Corp": the share dialog then says "Anyone at Example Corp" for everyone who can sign in
VITRINE_TRUSTED_PROXIES=        # addresses/CIDRs of proxies in front, or * when the platform guarantees one (Cloud Run; * only covers rate limits: proxy sign-in then needs a secret or a signed JWT)
VITRINE_LOGOUT_URL=             # where the browser goes after sign-out (the proxy's or IdP's sign-out page)
VITRINE_LLM_URL=                # Anthropic Messages endpoint for the page `sample` capability; empty = off
VITRINE_LLM_HEADER=             # e.g. "x-api-key: ..." for api.anthropic.com
VITRINE_MODEL_QUICK / _DEFAULT / _COMPLEX
```

## Sign-in

Sign-in is pluggable (`auth.go`). Whatever the method, vitrine then keeps its
own session, and agents connect through vitrine's own OAuth server (or personal
tokens), which sends the user through the same sign-in. vitrine refuses to
start with a configuration that would let anyone sign in as anyone.

| `VITRINE_AUTH` | Use it for | Needs |
|---|---|---|
| `exedev` | an exe.dev VM | nothing: exe.dev's proxy sets `X-ExeDev-UserID`/`-Email` and strips them from clients. Only safe when the server is reachable solely through that proxy. |
| `oidc` | any OpenID Connect provider: Google, Entra ID, Okta, Keycloak, Dex | `VITRINE_OIDC_ISSUER`, `VITRINE_OIDC_CLIENT_ID`; redirect URI `<base>/auth/callback`. `VITRINE_OIDC_CLIENT_SECRET` for a confidential client; leave it empty for a public client (PKCE only). Optional `VITRINE_OIDC_SCOPES`, `VITRINE_GROUPS_CLAIM` (default `groups`). |
| `proxy` | an authenticating proxy in front: oauth2-proxy, Pomerium, Cloudflare Access, Teleport, an ingress with auth | either plain headers plus `VITRINE_TRUSTED_PROXIES` (real addresses, not `*`) and/or `VITRINE_PROXY_SECRET`, or a signed JWT header (below) |
| `dev` | tests | refuses any non-loopback address |

`proxy` mode reads identity only at `GET /auth/proxy`: that is the one path the
proxy must protect (and set the headers on). Everything else must pass through
unauthenticated, because agents reach `/mcp`, `/.well-known/*` and `/oauth/*`
with their own tokens, and public links need no account.

```
# plain headers (oauth2-proxy defaults shown)
VITRINE_PROXY_EMAIL_HEADER=X-Forwarded-Email
VITRINE_PROXY_USER_HEADER=X-Forwarded-User
VITRINE_PROXY_NAME_HEADER=X-Forwarded-Preferred-Username
VITRINE_PROXY_GROUPS_HEADER=X-Forwarded-Groups
VITRINE_TRUSTED_PROXIES=10.0.0.0/8      # requests to /auth/proxy from elsewhere are refused
VITRINE_PROXY_SECRET="X-Proxy-Secret: <random>"   # optional extra check, or instead of addresses
VITRINE_PROXY_LOGIN_URL=/oauth2/start?rd={rd}     # optional: the proxy's start page; {rd} = return URL

# or a signed JWT header, checked against the proxy's published keys
VITRINE_PROXY_JWT_HEADER=Cf-Access-Jwt-Assertion  # or X-Goog-Iap-Jwt-Assertion, X-Pomerium-Jwt-Assertion
VITRINE_PROXY_JWT_JWKS=https://<team>.cloudflareaccess.com/cdn-cgi/access/certs
VITRINE_PROXY_JWT_ISSUER=https://<team>.cloudflareaccess.com
VITRINE_PROXY_JWT_AUDIENCE=<application audience tag>
```

`VITRINE_TRUSTED_PROXIES` also decides whose `X-Forwarded-For` counts for rate
limits; on exe.dev everything arrives through its proxy, so it is trusted there.
