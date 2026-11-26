import json, urllib.request, time
from playwright.sync_api import sync_playwright
BASE = "http://127.0.0.1:8899"
TOK = open("/tmp/vt/tok").read().strip()
fails = 0
ENGINE = ""
def check(c, m):
    global fails
    print(("PASS " if c else "FAIL ") + ENGINE + ": " + m, flush=True)
    if not c: fails += 1
def tool(name, **a):
    r = urllib.request.Request(BASE + "/api/v1/tools/" + name, data=json.dumps(a).encode(), method="POST", headers={"Authorization": "Bearer " + TOK})
    return json.loads(urllib.request.urlopen(r).read())["result"]
def wait_for(fn, timeout=8):
    end = time.time() + timeout
    while time.time() < end:
        try:
            v = fn()
            if v: return v
        except Exception: pass
        time.sleep(0.15)
    return None

for i, c in enumerate(["#e66", "#6a6", "#66e", "#ea4", "#4ae"]):
    tool("create", title="Page %d" % i, html="<!doctype html><body style='margin:0;background:%s'><h1>Page %d</h1>" % (c, i))
tool("create", title="Notes", markdown="# Notes\n\nwalrus")

# a call from the page itself, with the session's CSRF token
FETCH = """([m, u, b]) => fetch(u, {method: m, headers: {'Content-Type': 'application/json', 'X-CSRF': JSON.parse(document.body.dataset.boot).csrf},
  body: b ? JSON.stringify(b) : undefined}).then(r => r.status)"""

# each engine: WebKit, for one, won't start a drag on a frame
def run(engine):
    b = engine.launch()
    pg = b.new_context(viewport={"width": 1100, "height": 900}).new_page()
    errs = []
    pg.on("console", lambda m: errs.append(m.text) if m.type == "error" else None)
    pg.on("pageerror", lambda e: errs.append(str(e)))
    NAME = ["Work"]
    pg.on("dialog", lambda d: d.accept(NAME[0]) if d.type == "prompt" else d.accept())
    pg.goto(BASE + "/auth/login?email=marko@test&next=/")
    loose = lambda: pg.locator(".cell:not(.fcell)")
    check(wait_for(lambda: loose().count() == 6), "six artifacts at the top")

    pg.click("button[aria-label='New folder']")
    check(wait_for(lambda: pg.locator(".fcell .card-title").all_inner_texts() == ["Work"]), "toolbar creates a folder")
    check(pg.locator(".fcell .slot").count() == 4, "an empty folder shows four empty slots")
    for n in ["Page 0", "Page 1"]:
        loose().locator(".card", has_text=n).drag_to(pg.locator(".fcell .card"))
        check(wait_for(lambda: loose().filter(has_text=n).count() == 0), "drag %s onto the folder" % n)
    check(wait_for(lambda: pg.locator(".fcell .paper").count() == 2 and "2 items" in pg.locator(".fcell").inner_text()), "folder previews its artifacts")

    NAME[0] = "Archive"
    c = loose().filter(has_text="Page 2"); c.hover(); c.locator(".more").click()
    pg.locator(".menu button", has_text="New folder").click()
    check(wait_for(lambda: pg.locator(".fcell .card-title").all_inner_texts() == ["Archive", "Work"] and loose().filter(has_text="Page 2").count() == 0),
          "menu: new folder and move into it")

    pg.locator(".fcell .card", has_text="Work").click()
    check(wait_for(lambda: "folder=" in pg.url and pg.locator(".cell").count() == 2 and pg.locator(".crumbs b").inner_text() == "Work"), "open a folder")
    c = pg.locator(".cell", has_text="Page 0"); c.hover(); c.locator(".more").click()
    pg.locator(".menu button", has_text="Archive").click()
    check(wait_for(lambda: pg.locator(".cell").count() == 1), "menu: move to another folder")
    pg.locator(".cell .card", has_text="Page 1").drag_to(pg.locator(".crumbs a"))
    check(wait_for(lambda: pg.locator(".empty", has_text="This folder is empty").count() == 1), "drag onto All artifacts takes it out")
    pg.go_back()
    check(wait_for(lambda: "folder=" not in pg.url and loose().count() == 4), "back to the top level")

    pg.click("button[aria-label='Filter by type']")
    pg.locator(".menu button", has_text="Documents").click()
    check(wait_for(lambda: pg.locator(".cell").count() == 1 and pg.locator(".fcell").count() == 0 and "kind=markdown" in pg.url), "type filter: a flat list")
    pg.click("button[aria-label='Filter by type']")
    pg.locator(".menu button", has_text="All types").click()
    pg.locator(".tab", has_text="Yours").click()
    check(wait_for(lambda: pg.locator(".cell").count() == 6 and "owner=mine" in pg.url), "Yours tab lists artifacts in folders too")
    pg.locator(".tab", has_text="All").click()
    pg.click("button[aria-label='Search']")
    pg.fill(".searchrow input", "walrus")
    check(wait_for(lambda: pg.locator(".cell").count() == 1 and "q=walrus" in pg.url), "search")
    pg.click("button[aria-label='Search']")
    check(wait_for(lambda: pg.locator(".fcell").count() == 2 and "q=" not in pg.url), "closing search brings folders back")
    pg.click("button[aria-label='Show as list']")
    check(pg.locator(".cards.rows").count() == 1, "list view")
    pg.reload()
    check(wait_for(lambda: pg.locator(".cards.rows .fcell").count() == 2), "list view is remembered")
    pg.click("button[aria-label='Show as grid']")

    NAME[0] = "Old"
    f = pg.locator(".fcell", has_text="Archive"); f.hover(); f.locator(".more").click()
    pg.locator(".menu button", has_text="Rename").click()
    check(wait_for(lambda: pg.locator(".fcell .card-title").all_inner_texts() == ["Old", "Work"]), "rename a folder")
    fid = pg.locator(".fcell .card", has_text="Old").get_attribute("href").split("=")[1]
    f = pg.locator(".fcell", has_text="Old"); f.hover(); f.locator(".more").click()
    pg.locator(".menu button", has_text="Delete folder").click()
    check(wait_for(lambda: pg.locator(".fcell").count() == 1 and loose().count() == 6), "delete a folder: its artifacts come back")
    check(not errs, "no console errors: " + "; ".join(errs)[:300])

    # folders are personal: another user can't see, rename, delete or fill them
    work = pg.locator(".fcell .card").get_attribute("href").split("=")[1]
    aid = loose().locator(".card", has_text="Page 3").get_attribute("href").split("/")[-1]
    other = b.new_context().new_page()
    other.goto(BASE + "/auth/login?email=eve@test&next=/")
    wait_for(lambda: other.locator(".empty").count() == 1)
    check(other.evaluate("fetch('/api/folders').then(r => r.json()).then(j => j.folders.length)") == 0, "another user sees no folders")
    check(other.evaluate(FETCH, ["POST", "/api/folders/" + work, {"name": "mine"}]) == 404, "another user can't rename it")
    check(other.evaluate(FETCH, ["DELETE", "/api/folders/" + work, None]) == 404, "another user can't delete it")
    check(other.evaluate(FETCH, ["POST", "/api/artifacts/" + aid + "/folder", {"folder": ""}]) == 404, "another user can't file an artifact they can't see")
    check(pg.evaluate(FETCH, ["POST", "/api/artifacts/" + aid + "/folder", {"folder": fid}]) == 404, "filing into a deleted folder is refused")

    ph = b.new_context(viewport={"width": 390, "height": 800}, storage_state=pg.context.storage_state()).new_page()
    ph.goto(BASE + "/")
    wait_for(lambda: ph.locator(".fcell").count() == 1)
    check(ph.evaluate("document.documentElement.scrollWidth <= innerWidth + 1"), "no horizontal overflow on a phone")
    pg.evaluate(FETCH, ["DELETE", "/api/folders/" + work, None]) # leave things as they were for the next engine
    b.close()

with sync_playwright() as p:
    for e in (p.chromium, p.firefox, p.webkit):
        ENGINE = e.name
        run(e)
print("FAILS:", fails)
