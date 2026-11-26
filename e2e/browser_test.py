import json, os, urllib.request, time, sys
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
    try: return json.loads(urllib.request.urlopen(r).read())
    except urllib.error.HTTPError as e: return json.loads(e.read())

def wait_for(fn, timeout=8):
    end = time.time() + timeout
    while time.time() < end:
        try:
            v = fn()
            if v: return v
        except Exception: pass
        time.sleep(0.15)
    return None

PAGE1 = "<!doctype html><html><head><title>live</title></head><body><h1 id=h>version one</h1></body></html>"
PAGE2 = """<!doctype html><html><head><title>collab</title></head><body><pre id=out>{}</pre>
<script>
(async () => {
  const R = window.__r = {};
  const log = (k, v) => { R[k] = v; document.getElementById('out').textContent = JSON.stringify(R); };
  const [db, user, room] = await Promise.all([claude.use('db'), claude.use('user'), claude.use('room')]);
  log('have', [!!db, !!user, !!room].join(','));
  if (!db) return;
  const me = await user.me(); log('me', me.name);
  const id = await user.id();
  log('owner', await user.isOwner());
  db.collection('votes').onSnapshot(s => log('votes', s.docs.map(d => d.data().choice).sort().join(',')), e => log('err', e.code));
  await db.doc('votes/' + id).set({choice: me.name});
  try { await db.doc('votes/zzz-other').set({choice: 'x'}); log('other', 'ok'); } catch (e) { log('other', e.code); }
  const priv = db.doc('data/users/' + id + '/note'); await priv.set({t: 'secret-' + me.name});
  log('priv', (await priv.get()).data().t);
  const lease = await db.doc('meta/lock').acquire({holder: id, ttlMs: 20000}); log('lease', lease.acquired);
  room.presence({name: me.name});
  room.onPeers(({peers}) => log('peers', peers.map(p => (p.sameTab ? 'me:' : '') + (p.presence.name || '?')).sort().join(',')));
  room.on('ping', m => log('ping', m.data.n + (m.sameTab ? ':self' : ':other')));
  window.emitPing = async n => { try { await room.emit('ping', {n}); return 'sent'; } catch (e) { return e.code; } };
  const ps = await user.profiles([id]); log('profile', ps[id].name);
})();
</script></body></html>"""
PAGE3 = """<!doctype html><html><head><title>ai</title></head><body><pre id=out></pre><button id=go>go</button>
<script>
(async () => {
  const R = window.__r = {};
  const log = (k, v) => { R[k] = v; document.getElementById('out').textContent = JSON.stringify(R); };
  const sample = await claude.use('sample');
  log('have', !!sample);
  document.getElementById('go').onclick = async () => {
    let n = 0;
    try {
      const r = await sample('Reply with exactly the word: pong', {modelTier: 'quick', onText: () => n++});
      log('text', r.text.trim().toLowerCase()); log('chunks', n); log('tier', r.modelTierApplied);
      const j = await sample.json('Reply with a JSON array of the numbers 1, 2 and 3.', {modelTier: 'quick'});
      log('json', JSON.stringify(j));
    } catch (e) { log('err', e.code + ' ' + e.message); }
  };
})();
</script></body></html>"""

a1 = tool("create", title="Live reload test", html=PAGE1)["result"]
a2 = tool("create", title="Collab test", html=PAGE2, capabilities={"db": {"rules": [{"path": "votes", "read": "view", "write": "owner"}, {"path": "votes/{self}", "write": "interact"}]}, "user": {"scopes": ["profile"]}, "room": {}})["result"]
a3 = tool("create", title="AI test", html=PAGE3, capabilities={"sample": {}})["result"]
tool("share", id=a2["id"], who="friend@example.com", level="interact")

with sync_playwright() as p:
    br = p.chromium.launch()
    errors = []
    def ctx(email):
        c = br.new_context()
        pg = c.new_page()
        pg.on("console", lambda m: errors.append(email + ": " + m.text) if m.type == "error" else None)
        pg.goto(BASE + "/auth/login?email=" + email + "&next=/")
        return c, pg
    oc, owner = ctx("marko@test")
    fc, friend = ctx("friend@example.com")

    # --- live reload ---
    owner.goto(BASE + "/a/" + a1["id"])
    fr = lambda pg: pg.frame_locator("#frame")
    check(wait_for(lambda: fr(owner).locator("#h").inner_text() == "version one"), "shell renders the frame")
    check(wait_for(lambda: "on" in owner.locator("#conn").get_attribute("class")), "live connection up")
    tool("edit", id=a1["id"], base_version=1, edits=[{"old_text": "version one", "new_text": "version two"}])
    check(wait_for(lambda: fr(owner).locator("#h").inner_text() == "version two"), "auto-refresh shows v2")
    check(owner.locator("#ver").inner_text() == "v2", "version label v2")
    owner.locator("#auto").uncheck()
    tool("edit", id=a1["id"], base_version=2, edits=[{"old_text": "version two", "new_text": "version three"}])
    check(wait_for(lambda: owner.locator("#badge").is_visible() and "v3" in owner.locator("#badge").inner_text()), "auto off: badge offers v3")
    check(fr(owner).locator("#h").inner_text() == "version two", "auto off: frame stays on v2")
    owner.locator("#badge").click()
    check(wait_for(lambda: fr(owner).locator("#h").inner_text() == "version three"), "badge click loads v3")
    owner.locator("#auto").check()
    owner.locator("#historyBtn").click()
    check(wait_for(lambda: owner.locator("#versions li").count() == 3), "history lists 3 versions")

    # --- title menu: rename, duplicate ---
    owner.locator("#title").click()
    check(wait_for(lambda: owner.locator("#renameBtn").is_visible()), "title menu opens")
    owner.locator("#renameBtn").click()
    owner.locator("#titleEdit").fill("Renamed live")
    owner.locator("#titleEdit").press("Enter")
    check(wait_for(lambda: tool("read", id=a1["id"])["result"]["title"] == "Renamed live"), "rename saves the title")
    check(owner.locator("#titleText").inner_text() == "Renamed live", "rename shows the new title")
    owner.locator("#title").click()
    owner.locator("#dupBtn").click()
    owner.wait_for_url(lambda u: "/a/" in u and a1["id"] not in u)
    check(True, "duplicate opens the copy")
    check(wait_for(lambda: owner.locator("#titleText").inner_text() == "Copy of Renamed live"), "copy is titled 'Copy of ...'")
    check(wait_for(lambda: fr(owner).locator("#h").inner_text() == "version three"), "copy has the latest content")

    # --- db / room / user across two users ---
    owner.goto(BASE + "/a/" + a2["id"])
    friend.goto(BASE + "/a/" + a2["id"])
    R = lambda pg: pg.frame_locator("#frame").locator("#out").inner_text()
    rj = lambda pg: json.loads(R(pg) or "{}")
    check(wait_for(lambda: rj(owner).get("have") == "true,true,true"), "owner: db, user, room resolve")
    check(wait_for(lambda: rj(friend).get("have") == "true,true,true"), "friend: db, user, room resolve")
    check(wait_for(lambda: rj(owner).get("votes") == "friend,marko,x" and rj(friend).get("votes") == "friend,marko,x"), "votes sync live across users")
    check(wait_for(lambda: rj(owner).get("other") == "ok"), "owner may write someone else's vote (prefix write: owner)")
    check(wait_for(lambda: rj(friend).get("other") == "invalid_argument"), "friend may not write someone else's vote")
    check(wait_for(lambda: rj(owner).get("priv") == "secret-marko" and rj(friend).get("priv") == "secret-friend"), "private data/users subtree")
    check(wait_for(lambda: rj(owner).get("lease") is not None and rj(friend).get("lease") is not None and rj(owner)["lease"] != rj(friend)["lease"]), "lease: exactly one holder")
    check(wait_for(lambda: rj(owner).get("peers") == "friend,me:marko" and rj(friend).get("peers") == "marko,me:friend"), "room presence: both see both")
    check(rj(owner).get("profile") == "marko" and rj(owner).get("owner") is True and rj(friend).get("owner") is False, "user profile + isOwner")
    owner_frame = owner.frame_locator("#frame")
    res = owner.frames[1].evaluate("emitPing(7)")
    check(res == "sent" and wait_for(lambda: rj(friend).get("ping") == "7:other" and rj(owner).get("ping") == "7:self"), "owner emit reaches both")
    res = friend.frames[1].evaluate("emitPing(8)")
    check(res == "not_permitted", "friend (interact) may not emit on an admin-only topic")
    # agent writes show up live
    tool("db_write", id=a2["id"], path="votes/agent", op="set", data={"choice": "agent"})
    check(wait_for(lambda: rj(friend).get("votes") == "agent,friend,marko,x"), "agent db_write reaches open pages live: " + str(rj(friend).get("votes")))
    # a second tab of the same user is its own peer
    t2 = oc.new_page(); t2.goto(BASE + "/a/" + a2["id"])
    check(wait_for(lambda: rj(friend).get("peers") == "marko,marko,me:friend"), "second tab is a separate peer")
    t2.close()
    check(wait_for(lambda: rj(friend).get("peers") == "marko,me:friend"), "closing the tab leaves the room")
    # new version keeps data
    tool("edit", id=a2["id"], base_version=1, edits=[{"old_text": "<pre id=out>", "new_text": "<p>v2</p><pre id=out>"}])
    check(wait_for(lambda: owner.frame_locator("#frame").locator("p").inner_text() == "v2" and rj(owner).get("votes") == "agent,friend,marko,x"), "new version reloads and keeps db data")
    # raw view: no runtime
    owner.goto(BASE + "/a/" + a2["id"] + "/raw")
    check(wait_for(lambda: '"have":"false,false,false"' in owner.frame_locator("#frame").locator("#out").inner_text()), "raw view runs the page, without the runtime")
    # --- sample (needs an LLM) ---
    if os.environ.get("VITRINE_LLM_URL"):
        owner.goto(BASE + "/a/" + a3["id"])
        check(wait_for(lambda: rj(owner).get("have") is True), "sample resolves")
        owner.frame_locator("#frame").locator("#go").click()
        check(wait_for(lambda: owner.locator("#consent").is_visible()), "consent dialog")
        owner.locator("#consent button[value=allow]").click()
        check(wait_for(lambda: rj(owner).get("json"), 60), "sample result: " + R(owner))
        r = rj(owner)
        check(r.get("text") == "pong" and r.get("tier") == "quick" and r.get("json") == "[1,2,3]", "sample text + json")
    bad = [e for e in errors if "Content Security Policy" in e or "Refused" in e]
    check(not bad, "no CSP violations in the shell" + ("" if not bad else ": " + " | ".join(bad[:3])))
    print("console errors:", errors[:5])
    br.close()
print("FAILS:", fails)
