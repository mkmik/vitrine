"""Sign-in methods: oidc, proxy (headers / secret / signed JWT), dev, and the
configurations vitrine must refuse. Starts its own vitrine processes."""
import json, os, re, subprocess, sys, time, urllib.request, urllib.parse, http.cookiejar, hashlib, base64, socket
BIN = "/tmp/vitrine-new"
PORT = 8899
LOCAL = "http://127.0.0.1:%d" % PORT
VMIP = [a for a in subprocess.run(["hostname", "-I"], capture_output=True, text=True).stdout.split() if not a.startswith("127.")][0]
IDP = "http://127.0.0.1:8892"
fails = 0
def check(c, m):
    global fails
    print(("PASS " if c else "FAIL ") + m, flush=True)
    if not c: fails += 1

class NoRedir(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None
def opener(jar=None, follow=False):
    hs = [urllib.request.HTTPCookieProcessor(jar if jar is not None else http.cookiejar.CookieJar())]
    if not follow: hs.append(NoRedir)
    return urllib.request.build_opener(*hs)
def req(op, url, data=None, headers={}, method=None):
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        resp = op.open(r)
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read().decode()
    return resp.status, resp.headers, resp.read().decode()

proc = None
def start(env, addr="127.0.0.1"):
    global proc
    stop()
    for f in ["/tmp/vt/auth.db", "/tmp/vt/auth.db-wal", "/tmp/vt/auth.db-shm"]:
        if os.path.exists(f): os.remove(f)
    db = "/tmp/vt/auth.db"
    if os.environ.get("AUTH_PG"):  # run against Postgres: a fresh database per start
        db = subprocess.check_output(["/tmp/pgtest", "fresh", "-port", "5439", "-db", "vtauth"], text=True).strip()
    e = {"PATH": os.environ["PATH"], "VITRINE_DB": db, "VITRINE_ADDR": "%s:%d" % (addr, PORT), "VITRINE_BASE_URL": LOCAL, **env}
    proc = subprocess.Popen([BIN, "serve"], env=e, stdout=open("/tmp/vt/auth.log", "w"), stderr=subprocess.STDOUT)
    for _ in range(50):
        try:
            urllib.request.urlopen(LOCAL + "/healthz", timeout=1); return
        except Exception: time.sleep(0.1)
    raise RuntimeError("vitrine did not start: " + open("/tmp/vt/auth.log").read())
def stop():
    global proc
    if proc: proc.terminate(); proc.wait(); proc = None
def refuses(env, want, msg):
    e = {"PATH": os.environ["PATH"], "VITRINE_DB": "/tmp/vt/auth-x.db", "VITRINE_ADDR": "127.0.0.1:8898", "VITRINE_BASE_URL": "http://127.0.0.1:8898", **env}
    r = subprocess.run([BIN, "serve"], env=e, capture_output=True, text=True, timeout=10)
    out = r.stdout + r.stderr
    check(r.returncode != 0 and want in out, msg + ": " + out.strip().splitlines()[-1][-110:] if out.strip() else msg)
def me(op, base=LOCAL):
    c, h, b = req(op, base + "/api/me")
    return json.loads(b).get("user") if c == 200 else None
def idp_user(**u):
    json.dump(u, open("/tmp/idp_user.json", "w"))

subprocess.run(["pkill", "-f", "^" + BIN], capture_output=True)
idp = subprocess.Popen([sys.executable, os.path.join(os.path.dirname(os.path.abspath(__file__)), "mock_idp.py")])
time.sleep(1)
try:
    # ---------- configurations that must not start ----------
    refuses({}, "VITRINE_AUTH is not set", "no VITRINE_AUTH: refuses to start")
    refuses({"VITRINE_AUTH": "bogus"}, "unknown VITRINE_AUTH", "unknown mode: refuses")
    refuses({"VITRINE_AUTH": "dev", "VITRINE_ADDR": "0.0.0.0:8898"}, "loopback", "dev mode on a public address: refuses")
    refuses({"VITRINE_AUTH": "oidc"}, "VITRINE_OIDC_ISSUER", "oidc without issuer: refuses")
    refuses({"VITRINE_AUTH": "proxy"}, "VITRINE_TRUSTED_PROXIES", "proxy with plain headers and no trust: refuses")
    refuses({"VITRINE_AUTH": "proxy", "VITRINE_TRUSTED_PROXIES": "*"}, "VITRINE_TRUSTED_PROXIES", "proxy with plain headers trusting *: refuses")
    refuses({"VITRINE_AUTH": "proxy", "VITRINE_PROXY_JWT_HEADER": "X-Jwt"}, "VITRINE_PROXY_JWT_JWKS", "proxy JWT without keys: refuses")
    refuses({"VITRINE_AUTH": "proxy", "VITRINE_TRUSTED_PROXIES": "10.0.0.0/33"}, "not an address or CIDR", "bad CIDR: refuses")

    # ---------- OIDC ----------
    start({"VITRINE_AUTH": "oidc", "VITRINE_OIDC_ISSUER": IDP, "VITRINE_OIDC_CLIENT_ID": "vitrine", "VITRINE_OIDC_CLIENT_SECRET": "s3cret",
           "VITRINE_ALLOW_GROUPS": "eng"})
    idp_user(sub="alice-1", email="Alice@Example.com", email_verified=True, name="Alice", groups=["eng", "x"])
    op = opener(follow=True)
    c, h, b = req(op, LOCAL + "/auth/login?next=/settings")
    u = me(op)
    check(u and u["email"] == "alice@example.com" and u["name"] == "Alice", "oidc: browser sign-in round trip (%s)" % u)
    idp_user(sub="bob-1", email="bob@example.com", email_verified=True, groups=["sales"])
    c, h, b = req(opener(follow=True), LOCAL + "/auth/login?next=/")
    check(c == 403 and "group" in b, "oidc: user outside VITRINE_ALLOW_GROUPS refused")
    idp_user(sub="carol-1", email="carol@example.com", email_verified=False, groups=["eng"])
    c, h, b = req(opener(follow=True), LOCAL + "/auth/login?next=/")
    check(c == 401 and "not verified" in b, "oidc: unverified email refused")
    idp_user(sub="dan-1", preferred_username="dan@example.com", groups=["eng"])
    c, h, b = req(op2 := opener(follow=True), LOCAL + "/auth/login?next=/")
    check((me(op2) or {}).get("email") == "dan@example.com", "oidc: email from preferred_username (Entra-style)")
    c, h, b = req(opener(), LOCAL + "/auth/callback?state=forged&code=x")
    check(c == 400, "oidc: callback with an unknown state refused")

    # MCP OAuth for an agent, signing the user in through the same OIDC provider
    idp_user(sub="alice-1", email="alice@example.com", email_verified=True, groups=["eng"])
    code, h, body = req(opener(), LOCAL + "/oauth/register", json.dumps({"client_name": "Agent", "redirect_uris": ["http://127.0.0.1:9/cb"]}).encode(), {"Content-Type": "application/json"})
    cid = json.loads(body)["client_id"]
    ver = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
    chal = base64.urlsafe_b64encode(hashlib.sha256(ver.encode()).digest()).rstrip(b"=").decode()
    q = urllib.parse.urlencode({"client_id": cid, "redirect_uri": "http://127.0.0.1:9/cb", "response_type": "code", "code_challenge": chal, "code_challenge_method": "S256", "state": "s1", "resource": LOCAL + "/mcp"})
    jar = http.cookiejar.CookieJar(); op = opener(jar)
    url = LOCAL + "/oauth/authorize?" + q
    hops = 0
    while hops < 8:   # authorize -> /auth/login -> IdP -> /auth/callback -> authorize (consent)
        code, h, body = req(op, url)
        if code != 302: break
        url = urllib.parse.urljoin(url, h["Location"]); hops += 1
    check(code == 200 and "Connect <b>Agent</b>" in body, "oauth: authorize sends the agent's user through OIDC, then consent (%d hops)" % hops)
    csrf = re.search(r'name=csrf value="([^"]+)"', body).group(1)
    form = {"csrf": csrf, "decision": "allow", "client_id": cid, "redirect_uri": "http://127.0.0.1:9/cb", "state": "s1", "code_challenge": chal, "code_challenge_method": "S256"}
    code, h, body = req(op, LOCAL + "/oauth/authorize", urllib.parse.urlencode(form).encode(), {"Origin": LOCAL})
    acode = urllib.parse.parse_qs(urllib.parse.urlparse(h["Location"]).query)["code"][0]
    code, h, body = req(opener(), LOCAL + "/oauth/token", urllib.parse.urlencode({"grant_type": "authorization_code", "code": acode, "client_id": cid, "redirect_uri": "http://127.0.0.1:9/cb", "code_verifier": ver}).encode())
    at = json.loads(body)["access_token"]
    code, h, body = req(opener(), LOCAL + "/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "list", "arguments": {}}}).encode(), {"Authorization": "Bearer " + at, "Content-Type": "application/json"})
    check(code == 200 and "items" in body, "oauth: agent token from an OIDC sign-in works on /mcp")

    # ---------- proxy: plain headers from a trusted address ----------
    start({"VITRINE_AUTH": "proxy", "VITRINE_TRUSTED_PROXIES": "127.0.0.1", "VITRINE_LOGOUT_URL": "/oauth2/sign_out"}, addr="0.0.0.0")
    c, h, b = req(opener(), LOCAL + "/auth/login?next=/x")
    check(c == 302 and h["Location"] == "/auth/proxy?next=%2Fx", "proxy: login goes to /auth/proxy")
    hdr = {"X-Forwarded-Email": "eve@example.com", "X-Forwarded-User": "eve", "X-Forwarded-Preferred-Username": "Eve"}
    jar = http.cookiejar.CookieJar(); op = opener(jar)
    c, h, b = req(op, LOCAL + "/auth/proxy?next=/", headers=hdr)
    u = me(op)
    check(c == 302 and u and u["email"] == "eve@example.com" and u["name"] == "Eve", "proxy: headers from the trusted proxy sign in")
    csrf = json.loads(req(op, LOCAL + "/api/me")[2])["csrf"]
    c, h, b = req(op, LOCAL + "/auth/logout", b"", {"X-CSRF": csrf, "Origin": LOCAL}, "POST")
    check(c == 200 and json.loads(b)["next"] == "/oauth2/sign_out", "proxy: sign-out hands back VITRINE_LOGOUT_URL")
    VM = "http://%s:%d" % (VMIP, PORT)
    c, h, b = req(opener(), VM + "/auth/proxy?next=/", headers=hdr)
    check(c == 403, "proxy: the same headers from an untrusted address (%s) are refused" % VMIP)
    c, h, b = req(opener(), LOCAL + "/auth/proxy?next=/")
    check(c == 401, "proxy: no identity headers -> 401 with a hint")
    # rate-limit keys: X-Forwarded-For counts only from trusted proxies
    got429 = False
    for i in range(12):
        c, _, _ = req(opener(), VM + "/oauth/register", json.dumps({"redirect_uris": ["http://127.0.0.1:9/cb"]}).encode(), {"Content-Type": "application/json", "X-Forwarded-For": "203.0.113.%d" % i})
        got429 = got429 or c == 429
    check(got429, "client address: forged X-Forwarded-For from an untrusted address doesn't dodge the rate limit")
    ok = True
    for i in range(12):
        c, _, _ = req(opener(), LOCAL + "/oauth/register", json.dumps({"redirect_uris": ["http://127.0.0.1:9/cb"]}).encode(), {"Content-Type": "application/json", "X-Forwarded-For": "198.51.100.%d" % i})
        ok = ok and c == 201
    check(ok, "client address: behind a trusted proxy, each forwarded client gets its own limit")

    # ---------- proxy: shared secret, groups ----------
    start({"VITRINE_AUTH": "proxy", "VITRINE_PROXY_SECRET": "X-Proxy-Secret: abc123", "VITRINE_ALLOW_GROUPS": "eng"})
    base = {"X-Forwarded-Email": "fay@example.com", "X-Forwarded-Groups": "eng,ops"}
    c, h, b = req(opener(), LOCAL + "/auth/proxy", headers=base)
    check(c == 403, "proxy secret: missing secret refused")
    c, h, b = req(opener(), LOCAL + "/auth/proxy", headers={**base, "X-Proxy-Secret": "nope"})
    check(c == 403, "proxy secret: wrong secret refused")
    c, h, b = req(op := opener(), LOCAL + "/auth/proxy", headers={**base, "X-Proxy-Secret": "abc123"})
    check(c == 302 and (me(op) or {}).get("email") == "fay@example.com", "proxy secret: right secret signs in")
    c, h, b = req(opener(), LOCAL + "/auth/proxy", headers={**base, "X-Proxy-Secret": "abc123", "X-Forwarded-Groups": "sales"})
    check(c == 403, "proxy groups: user outside VITRINE_ALLOW_GROUPS refused")

    # ---------- proxy: signed JWT header (IAP / Cloudflare Access / Pomerium style) ----------
    AUD, PISS = "/projects/1/apps/vitrine", "https://proxy.test"
    start({"VITRINE_AUTH": "proxy", "VITRINE_PROXY_JWT_HEADER": "X-Goog-Iap-Jwt-Assertion", "VITRINE_PROXY_JWT_JWKS": IDP + "/jwks-ec",
           "VITRINE_PROXY_JWT_ISSUER": PISS, "VITRINE_PROXY_JWT_AUDIENCE": AUD, "VITRINE_PROXY_LOGIN_URL": "/_proxy/start?rd={rd}"})
    c, h, b = req(opener(), LOCAL + "/auth/login?next=/y")
    check(c == 302 and h["Location"] == "/_proxy/start?rd=" + urllib.parse.quote(LOCAL + "/auth/proxy?next=%2Fy", safe=""), "proxy JWT: login goes through VITRINE_PROXY_LOGIN_URL")
    def sign(key="", **claims):
        n = int(time.time())
        cl = {"iss": PISS, "aud": AUD, "sub": "accounts.google.com:123", "email": "gus@example.com", "iat": n, "exp": n + 300, **claims}
        return urllib.request.urlopen(IDP + "/sign?" + urllib.parse.urlencode({"claims": json.dumps(cl), "key": key})).read().decode()
    H = "X-Goog-Iap-Jwt-Assertion"
    c, h, b = req(op := opener(), LOCAL + "/auth/proxy", headers={H: sign()})
    check(c == 302 and (me(op) or {}).get("email") == "gus@example.com", "proxy JWT: valid token signs in")
    for label, tok in [("wrong audience", sign(aud="/projects/2/apps/other")), ("wrong issuer", sign(iss="https://evil.test")),
                       ("expired", sign(exp=int(time.time()) - 60)), ("signed by an unpublished key", sign(key="rogue"))]:
        c, h, b = req(opener(), LOCAL + "/auth/proxy", headers={H: tok})
        check(c == 401, "proxy JWT: %s refused" % label)
    c, h, b = req(opener(), LOCAL + "/auth/proxy", headers={"X-Forwarded-Email": "mallory@example.com"})
    check(c == 401, "proxy JWT: plain email header ignored without a token")

    # ---------- VITRINE_REQUIRE_SIGNIN: anonymous visitors always go to sign-in ----------
    start({"VITRINE_AUTH": "proxy", "VITRINE_TRUSTED_PROXIES": "127.0.0.1", "VITRINE_REQUIRE_SIGNIN": "true"})
    c, h, b = req(opener(), LOCAL + "/")
    check(c == 302 and h["Location"] == "/auth/login?next=%2F", "require sign-in: the index page sends anonymous visitors to sign in")
    op = opener(http.cookiejar.CookieJar())
    req(op, LOCAL + "/auth/proxy?next=/", headers={"X-Forwarded-Email": "ivy@example.com"})
    c, h, b = req(op, LOCAL + "/")
    check(c == 200, "require sign-in: the index page opens once signed in")

    # ---------- the browser flow end to end in proxy mode (Playwright) ----------
    start({"VITRINE_AUTH": "proxy", "VITRINE_TRUSTED_PROXIES": "127.0.0.1"})
    from playwright.sync_api import sync_playwright
    with sync_playwright() as p:
        b = p.chromium.launch()
        ctx = b.new_context(extra_http_headers={"X-Forwarded-Email": "hal@example.com"})  # stands in for the proxy
        pg = ctx.new_page(); pg.goto(LOCAL + "/")
        pg.locator("text=Sign in").first.click() if pg.locator("text=Sign in").count() else None
        pg.wait_for_load_state()
        check("hal@example.com" in pg.content(), "proxy: sign in from the index page in a real browser")
        pg.click("button.avatar")
        check(pg.locator("#usermenu").get_by_text("hal@example.com").is_visible() and pg.get_by_role("button", name="Sign out").is_visible(),
              "avatar opens a menu with the user's email and Sign out")
        b.close()
finally:
    stop(); idp.terminate()
print("FAILS:", fails)
