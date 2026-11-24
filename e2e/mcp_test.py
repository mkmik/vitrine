import json, urllib.request, sys
BASE = "http://127.0.0.1:8899"
TOK = open("/tmp/vt/tok").read().strip()
sid = None
n = 0
def rpc(method, params=None, tok=TOK):
    global sid, n
    n += 1
    body = {"jsonrpc": "2.0", "id": n, "method": method}
    if params is not None: body["params"] = params
    req = urllib.request.Request(BASE + "/mcp", data=json.dumps(body).encode(), method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + tok, **({"Mcp-Session-Id": sid} if sid else {})})
    try:
        r = urllib.request.urlopen(req)
    except urllib.error.HTTPError as e:
        return e.code, dict(e.headers), e.read().decode()
    if r.headers.get("Mcp-Session-Id"): sid = r.headers["Mcp-Session-Id"]
    return r.status, dict(r.headers), json.loads(r.read())
def tool(name, **args):
    st, h, res = rpc("tools/call", {"name": name, "arguments": args})
    r = res["result"]
    txt = r["content"][0]["text"]
    try: v = json.loads(txt)
    except Exception: v = txt
    return r.get("isError", False), v, r["content"]
def check(cond, msg):
    print(("PASS " if cond else "FAIL ") + msg)
    if not cond: check.fails += 1
check.fails = 0

st, h, res = rpc("initialize", {"protocolVersion": "2025-06-18", "clientInfo": {"name": "test"}})
check(st == 200 and res["result"]["serverInfo"]["name"] == "vitrine" and sid, "initialize + session id")
st, h, body = rpc("initialize", {}, tok="bogus")
check(st == 401 and "resource_metadata" in h.get("Www-Authenticate", h.get("WWW-Authenticate", "")), "401 with resource_metadata")
st, h, res = rpc("tools/list")
names = [t["name"] for t in res["result"]["tools"]]
check(len(names) == 15, "tools/list: " + ",".join(names))

html = "<!doctype html><html><head><title>t</title></head><body><h1>Hello kangaroo</h1><p id=x>one</p></body></html>"
err, v, content = tool("create", title="Test one", html=html, description="a test page")
check(not err and v["version"] == 1 and len(content) == 2, "create returns v1 + short guide on first create")
aid = v["id"]; url = v["url"]
err, v, _ = tool("create", title="Second", html="<p>two</p>")
check(not err, "second create")
err, v, content = tool("read", id=url)
check(not err and v["version"] == 1 and content[1]["text"] == html, "read by url")
err, v, _ = tool("edit", id=aid, base_version=1, edits=[{"old_text": "one", "new_text": "two"}])
check(not err and v["version"] == 2, "edit -> v2")
err, v, _ = tool("edit", id=aid, base_version=1, edits=[{"old_text": "two", "new_text": "three"}])
check(err and v["error"] == "conflict" and v["head"] == 2, "stale base_version -> conflict with head")
err, v, _ = tool("edit", id=aid, base_version=2, edits=[{"old_text": "zzz", "new_text": "x"}])
check(err and v["error"] == "edit_failed", "edit with missing old_text fails")
err, v, _ = tool("edit", id=aid, base_version=2, edits=[{"old_text": "<", "new_text": "x"}])
check(err and "occurs" in v["message"], "edit with ambiguous old_text fails")
err, v, _ = tool("rewrite", id=aid, base_version=2, html=html.replace("one", "rewritten"), title="Test renamed")
check(not err and v["version"] == 3, "rewrite -> v3")
err, v, content = tool("read", id=aid + "/v/2" if False else BASE + "/a/" + aid + "/v/2")
check(not err and v["version"] == 2 and "two" in content[1]["text"], "read old version via /v/2 url")
err, v, _ = tool("search", query="kangar")
check(not err and len(v["items"]) == 1 and v["items"][0]["id"] == aid, "search prefix match")
err, v, _ = tool("list")
check(not err and len(v["items"]) == 2 and v["items"][0]["title"] in ("Test renamed", "Second"), "list")
err, v, _ = tool("history", id=aid)
check(not err and [x["version"] for x in v["versions"]] == [3, 2, 1] and v["versions"][0]["author"] == "agent:cli", "history + author label")
err, v, _ = tool("restore", id=aid, version=1, base_version=3)
check(not err and v["version"] == 4, "restore v1 -> v4")
err, v, _ = tool("share", id=aid, who="public", level="view", version=2)
check(not err and v["grants"][0] == {"audience": "public", "level": "view", "pinned": 2}, "share public pinned v2")
err, v, _ = tool("share", id=aid, who="public", level="edit")
check(err, "public edit refused")
err, v, _ = tool("share", id=aid, who="friend@example.com", level="edit")
check(not err and len(v["grants"]) == 2, "share edit with a person")
err, v, _ = tool("declare", id=aid, capabilities={"db": {"rules": [{"path": "votes", "read": "view", "write": "owner"}, {"path": "votes/{self}", "write": "interact"}]}, "user": {"scopes": ["profile"]}, "room": {}})
check(not err, "declare db rules")
err, v, _ = tool("declare", id=aid, capabilities={"db": {"rules": [{"path": "votes", "read": "self"}]}})
check(err, "bad declaration rejected: " + str(v.get("message"))[:80])
err, v, _ = tool("db_write", id=aid, path="config/main", op="set", data={"a": 1, "nested": {"x": 1}})
check(not err and v["version"] == 1, "db_write set")
err, v, _ = tool("db_write", id=aid, path="config/main", op="update", data={"nested": {"y": 2}})
err, v, _ = tool("db_get", id=aid, path="config/main")
check(not err and v["data"] == {"a": 1, "nested": {"x": 1, "y": 2}} and v["version"] == 2, "db update deep-merges")
for i in range(5):
    tool("db_write", id=aid, path="items/i%d" % i, op="set", data={"n": i, "tag": "odd" if i % 2 else "even"})
err, v, _ = tool("db_query", id=aid, collection="items", where=[{"field": "tag", "op": "==", "value": "even"}], orderBy="n", dir="desc", limit=2)
check(not err and [d["data"]["n"] for d in v["docs"]] == [4, 2], "db_query where/orderBy/limit")
err, v, _ = tool("db_get", id=aid, path="items")
check(not err and len(v["docs"]) == 5, "db_get collection")
err, v, _ = tool("db_write", id=aid, path="bad", op="set", data={})
check(err, "odd path write rejected")
err, v, _ = tool("delete", id=aid, confirm="nope")
check(err and v["error"] == "confirm_required", "delete needs confirm")
err, v, _ = tool("guide", topic="db")
check(not err and "data/users" in v, "guide db")
# REST
req = urllib.request.Request(BASE + "/api/v1/tools/read", data=json.dumps({"id": aid}).encode(), method="POST", headers={"Authorization": "Bearer " + TOK})
r = json.loads(urllib.request.urlopen(req).read())
check(r["result"]["id"] == aid and "html" in r, "REST tool call")
for p in ["/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"]:
    r = json.loads(urllib.request.urlopen(BASE + p).read())
    check("authorization_servers" in r or "registration_endpoint" in r, "metadata " + p)
open("/tmp/vt/aid", "w").write(aid)
print("FAILS:", check.fails)
