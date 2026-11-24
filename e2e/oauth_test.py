import json, urllib.request, urllib.parse, http.cookiejar, hashlib, base64, os, re
BASE = "http://127.0.0.1:8899"
AID = open("/tmp/vt/aid").read().strip()
fails = 0
def check(c, m):
    global fails
    print(("PASS " if c else "FAIL ") + m)
    if not c: fails += 1
class NoRedir(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k): return None
def opener(jar):
    return urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar), NoRedir)
def req(op, url, data=None, headers={}, method=None):
    r = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        resp = op.open(r)
    except urllib.error.HTTPError as e:
        return e.code, e.headers, e.read().decode()
    return resp.status, resp.headers, resp.read().decode()

# register
code, h, body = req(opener(None), BASE + "/oauth/register", json.dumps({"client_name": "Claude", "redirect_uris": ["https://claude.ai/api/mcp/auth_callback"]}).encode(), {"Content-Type": "application/json"})
cid = json.loads(body)["client_id"]; check(code == 201, "register")
code, h, body = req(opener(None), BASE + "/oauth/register", json.dumps({"redirect_uris": ["https://evil.example/cb"]}).encode(), {"Content-Type": "application/json"})
check(code == 400, "evil redirect rejected")
ver = base64.urlsafe_b64encode(os.urandom(32)).rstrip(b"=").decode()
chal = base64.urlsafe_b64encode(hashlib.sha256(ver.encode()).digest()).rstrip(b"=").decode()
q = urllib.parse.urlencode({"client_id": cid, "redirect_uri": "https://claude.ai/api/mcp/auth_callback", "response_type": "code", "code_challenge": chal, "code_challenge_method": "S256", "state": "st1", "resource": BASE + "/mcp"})
jar = http.cookiejar.CookieJar(); op = opener(jar)
code, h, body = req(op, BASE + "/oauth/authorize?" + q)
check(code == 302 and h["Location"].startswith("/auth/login"), "authorize without session -> login")
code, h, body = req(op, BASE + "/auth/login?email=marko@test&next=" + urllib.parse.quote("/oauth/authorize?" + q))
check(code == 302 and any(c.name == "vit_s" for c in jar), "dev login sets session")
code, h, body = req(op, BASE + "/oauth/authorize?" + q)
check(code == 200 and "Connect <b>Claude</b>" in body and "form-action 'self' https://claude.ai" in h["Content-Security-Policy"], "consent page with form-action to redirect origin")
csrf = re.search(r'name=csrf value="([^"]+)"', body).group(1)
form = {"csrf": csrf, "decision": "allow", "client_id": cid, "redirect_uri": "https://claude.ai/api/mcp/auth_callback", "state": "st1", "code_challenge": chal, "code_challenge_method": "S256"}
code, h, body = req(op, BASE + "/oauth/authorize", urllib.parse.urlencode({**form, "csrf": "bad"}).encode(), {"Origin": BASE})
check(code == 403, "bad csrf refused")
code, h, body = req(op, BASE + "/oauth/authorize", urllib.parse.urlencode(form).encode(), {"Origin": BASE})
loc = urllib.parse.urlparse(h["Location"]); qs = urllib.parse.parse_qs(loc.query)
check(code == 302 and loc.netloc == "claude.ai" and qs["state"] == ["st1"], "approve -> redirect with code")
tokform = {"grant_type": "authorization_code", "code": qs["code"][0], "client_id": cid, "redirect_uri": "https://claude.ai/api/mcp/auth_callback", "code_verifier": "wrong"}
code, h, body = req(opener(None), BASE + "/oauth/token", urllib.parse.urlencode(tokform).encode())
check(code == 400, "wrong verifier refused (and burns the code)")
# redo the flow properly
code, h, body = req(op, BASE + "/oauth/authorize", urllib.parse.urlencode(form).encode(), {"Origin": BASE})
c2 = urllib.parse.parse_qs(urllib.parse.urlparse(h["Location"]).query)["code"][0]
code, h, body = req(opener(None), BASE + "/oauth/token", urllib.parse.urlencode({**tokform, "code": c2, "code_verifier": ver}).encode())
t = json.loads(body); check(code == 200 and t["access_token"].startswith("vat_"), "token exchange")
code, h, body = req(opener(None), BASE + "/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": {"name": "list", "arguments": {}}}).encode(), {"Authorization": "Bearer " + t["access_token"], "Content-Type": "application/json"})
check(code == 200 and "Test" in body, "access token works on /mcp")
code, h, body = req(opener(None), BASE + "/oauth/token", urllib.parse.urlencode({"grant_type": "refresh_token", "refresh_token": t["refresh_token"], "client_id": cid}).encode())
t2 = json.loads(body); check(code == 200 and t2["refresh_token"] != t["refresh_token"], "refresh rotates")
code, h, body = req(opener(None), BASE + "/oauth/token", urllib.parse.urlencode({"grant_type": "refresh_token", "refresh_token": t["refresh_token"], "client_id": cid}).encode())
check(code == 400, "refresh reuse refused")
code, h, body = req(opener(None), BASE + "/mcp", json.dumps({"jsonrpc": "2.0", "id": 1, "method": "ping"}).encode(), {"Authorization": "Bearer " + t2["access_token"]})
check(code == 401, "reuse revoked the whole family")

# views and access
anon = opener(None)
code, h, body = req(anon, BASE + "/a/" + AID)
check(code == 200 and "data-boot" in body, "public pinned grant: anon sees shell")
boot = json.loads(re.search(r'data-boot="([^"]+)"', body).group(1).replace("&#34;", '"').replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">").replace("&#39;", "'"))
check(boot["artifact"]["shown"] == 2 and boot["artifact"]["level"] == "view" and not boot["artifact"]["follow"], "anon pinned to v2")
code, h, body = req(anon, BASE + "/c/" + AID + "/2")
csp = h["Content-Security-Policy"]
check(code == 200 and csp.startswith("sandbox allow-scripts") and "frame-ancestors 'self'" in csp and "window.__vitrine_cfg" in body and "two" in body, "frame content with sandbox CSP + runtime")
check(body.index("<script>window.__vitrine_cfg") > body.index("<head>"), "runtime injected after <head>")
code, h, body = req(anon, BASE + "/c/" + AID + "/4")
check(code == 404, "anon cannot load head v4")
code, h, body = req(anon, BASE + "/a/" + AID + "/raw")
check(code == 200 and "frame-ancestors *" in h["Content-Security-Policy"] and "?raw=1" in body, "raw view: embeddable wrapper")
code, h, body = req(anon, BASE + "/c/" + AID + "/2?raw=1")
check(code == 200 and '"mode":"raw"' in body, "raw view frame runs without the runtime")
code, h, body = req(op, BASE + "/c/" + AID + "/1")
check(code == 200, "owner loads any version")
code, h, body = req(op, BASE + "/api/artifacts/" + AID + "/share", json.dumps({"who": "users", "level": "view"}).encode(), {"Content-Type": "application/json", "Origin": BASE})
check(code == 403, "UI API mutation without X-CSRF refused")
code, h, body = req(op, BASE + "/", None)
check(code == 200 and "frame-ancestors 'none'" in h["Content-Security-Policy"], "index with strict CSP")
other = http.cookiejar.CookieJar(); op2 = opener(other)
req(op2, BASE + "/auth/login?email=stranger@test&next=/")
code, h, body = req(op2, BASE + "/c/" + AID + "/4")
check(code == 404, "signed-in stranger: no head access")
code, h, body = req(op2, BASE + "/api/artifacts")
check(code == 200 and json.loads(body)["items"] == [], "stranger lists nothing")
fr = http.cookiejar.CookieJar(); op3 = opener(fr)
req(op3, BASE + "/auth/login?email=friend@example.com&next=/")
code, h, body = req(op3, BASE + "/api/artifacts")
items = json.loads(body)["items"]
check(len(items) == 1 and items[0]["level"] == "edit", "friend with edit grant sees it")
code, h, body = req(op3, BASE + "/api/artifacts/" + AID + "/history")
check(code == 200, "editor sees history")
print("FAILS:", fails)
