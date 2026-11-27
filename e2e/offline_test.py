import json, urllib.request, time
from playwright.sync_api import sync_playwright
BASE = "http://127.0.0.1:8899"
TOK = open("/tmp/vt/tok").read().strip()
fails = 0
def check(c, m):
    global fails
    print(("PASS " if c else "FAIL ") + m, flush=True)
    if not c: fails += 1
def tool(name, **a):
    r = urllib.request.Request(BASE + "/api/v1/tools/" + name, data=json.dumps(a).encode(), method="POST", headers={"Authorization": "Bearer " + TOK})
    return json.loads(urllib.request.urlopen(r).read())["result"]
def wait_for(fn, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        try:
            v = fn()
            if v: return v
        except Exception: pass
        time.sleep(0.15)
    return None

PAGE = """<!doctype html><html><head></head><body><pre id=out>[]</pre><script>
window.L = [];
(async () => {
  const db = await claude.use('db');
  if (!db) { document.getElementById('out').textContent = 'nodb'; return; }
  window.db = db;
  db.collection('items').orderBy('n').onSnapshot(s => {
    L.push({n: s.docs.map(d => d.data().n).join(','), fc: s.metadata.fromCache, pw: s.metadata.hasPendingWrites, t: Date.now()});
    document.getElementById('out').textContent = JSON.stringify(L);
  });
  window.add = async n => { try { await db.collection('items').doc('i' + n).set({n}); return 'ok'; } catch (e) { return e.code; } };
})();
</script></body></html>"""
a = tool("create", title="offline test", html=PAGE, capabilities={"db": {}})
aid = a["id"]
for n in (1, 2):
    tool("db_write", id=aid, path="items/i%d" % n, op="set", data={"n": n})

with sync_playwright() as p:
    br = p.chromium.launch()
    def ctx(email):
        c = br.new_context()
        pg = c.new_page()
        pg.goto(BASE + "/auth/login?email=" + email + "&next=/")
        return c, pg
    L = lambda pg: json.loads(pg.frame_locator("#frame").locator("#out").inner_text())
    c1, owner = ctx("marko@test")
    owner.goto(BASE + "/a/" + aid)
    check(wait_for(lambda: L(owner) and L(owner)[-1]["n"] == "1,2" and not L(owner)[-1]["fc"]), "first visit: confirmed snapshot (fromCache false)")
    check(L(owner)[0]["fc"] is False, "first visit: nothing cached, no fromCache snapshot")
    tool("db_write", id=aid, path="items/i3", op="set", data={"n": 3})
    check(wait_for(lambda: L(owner)[-1]["n"] == "1,2,3"), "live update over WebSocket")
    owner.reload()
    check(wait_for(lambda: len(L(owner)) >= 1 and L(owner)[0]["fc"] is True and L(owner)[0]["n"] == "1,2,3"), "reload: saved copy first, fromCache true: " + str(L(owner)[:1]))
    check(wait_for(lambda: L(owner)[-1]["fc"] is False and L(owner)[-1]["n"] == "1,2,3"), "then a confirmed snapshot follows")

    # socket blocked: HTTP fallback with fast polling
    c2 = br.new_context()
    c2.add_init_script("try { localStorage.setItem('vitrine.pollMs', '1500') } catch (e) {}")
    c2.route_web_socket("**/ws/**", lambda ws: ws.close())
    blocked = c2.new_page()
    blocked.goto(BASE + "/auth/login?email=marko@test&next=/a/" + aid)
    check(wait_for(lambda: L(blocked) and L(blocked)[-1]["n"] == "1,2,3" and not L(blocked)[-1]["fc"], 12), "socket blocked: db still resolves and reads over HTTP")
    check(blocked.locator("#conn").get_attribute("class") == "dot", "shell shows not-live")
    tool("db_write", id=aid, path="items/i4", op="set", data={"n": 4})
    check(wait_for(lambda: L(blocked)[-1]["n"] == "1,2,3,4", 6), "blocked tab picks up changes by polling")
    r = blocked.frames[1].evaluate("add(5)")
    check(r == "ok", "blocked tab writes over HTTP")
    check(wait_for(lambda: L(owner)[-1]["n"] == "1,2,3,4,5"), "live tab sees the blocked tab's write at once")
    pend = [x for x in L(blocked) if x["n"] == "1,2,3,4,5"]
    check(pend and pend[0]["pw"] is True, "own write shown first as pending")
    # fully offline: write rejects unavailable, listener stays quiet
    c2.route("**/api/db/**", lambda route: route.abort())
    r = blocked.frames[1].evaluate("add(6)")
    check(r == "unavailable", "store unreachable: write rejects unavailable")
    c2.unroute("**/api/db/**")
    # sign-out clears the saved copies
    owner.goto(BASE + "/")
    owner.click("button.avatar")
    owner.get_by_role("button", name="Sign out").click()
    time.sleep(1)
    dbs = owner.evaluate("indexedDB.databases().then(d => d.map(x => x.name))")
    check("vitrine" not in dbs, "sign-out deletes the saved snapshots")
    br.close()
print("FAILS:", fails)
