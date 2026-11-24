import json, urllib.request, time, sys
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
def mcp(method, params):
    body = {"jsonrpc": "2.0", "id": 1, "method": method, "params": params}
    r = urllib.request.Request(BASE + "/mcp", data=json.dumps(body).encode(), method="POST",
        headers={"Content-Type": "application/json", "Authorization": "Bearer " + TOK})
    return json.loads(urllib.request.urlopen(r).read())
def wait_for(fn, timeout=8):
    end = time.time() + timeout
    while time.time() < end:
        try:
            v = fn()
            if v: return v
        except Exception: pass
        time.sleep(0.15)
    return None

filler = "\n\n".join("Paragraph %d about the walrus migration plan, with enough words to take up a line or two of space on the page." % i for i in range(8))
MD = """# Walrus PRD

Intro text with a [link out](https://example.com) and an [anchor](#goals).

## Goals

- [x] store markdown
- [ ] whiteboard later

| Name | Value |
|------|-------|
| alpha | 1 |
| beta | 2 |

Footnote here.[^n]

## Design

```go
func main() { fmt.Println("hi") }
```

```mermaid
graph LR
  A[Agent] --> B[vitrine]
```

<details><summary>More</summary>hidden <kbd>Ctrl</kbd></details>

<script>window.__pwned = 1</script>
<img src="x" onerror="window.__pwned = 2">
<link rel="dns-prefetch" href="https://leak-qz7.example.org/">
<meta http-equiv="refresh" content="0;url=https://example.org/">
<style>body{display:none}</style>
<iframe srcdoc="hi"></iframe>
<form action="https://example.org/"><input name=q></form>

![remote](https://example.org/pixel.png)
![inline](data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7)

%s

## Rollout

%s

[^n]: The footnote.
""" % (filler, filler)

# ---- tool schema + create rules ----
tl = mcp("tools/list", {})["result"]["tools"]
cr = [t for t in tl if t["name"] == "create"][0]
check("markdown" in cr["inputSchema"]["properties"], "create schema has markdown")
v = tool("create", title="x")
check("error" in json.dumps(v) or v.get("result", {}).get("error"), "create without content refused: " + json.dumps(v)[:120])
v = tool("create", title="x", html="<p>a</p>", markdown="# a")
check("not both" in json.dumps(v), "create with both refused")
v = tool("create", markdown="# a", capabilities={"db": {}})
check("capabilities apply to html" in json.dumps(v), "markdown + capabilities refused")

v = tool("create", markdown=MD, description="markdown test")["result"]
aid = v["id"]
check(v["kind"] == "markdown" and v["title"] == "Walrus PRD" and v["version"] == 1, "create markdown: kind + title from H1")

r = tool("read", id=v["url"])
check(r["result"]["kind"] == "markdown" and r.get("markdown") == MD, "read returns kind + exact markdown")
v = tool("edit", id=aid, base_version=1, edits=[{"old_text": "whiteboard later", "new_text": "whiteboard in phase two"}])
check(v["result"]["version"] == 2, "edit markdown text")
v = tool("rewrite", id=aid, base_version=2, html="<p>nope</p>")
check("pass its new content as markdown" in json.dumps(v), "rewrite with html on a markdown artifact refused")
v = tool("declare", id=aid, capabilities={"db": {}})
check("no runtime" in json.dumps(v), "declare on markdown refused")
v = tool("search", query="walrus")
check(any(i["id"] == aid and i["kind"] == "markdown" for i in v["result"]["items"]), "search finds markdown body, list has kind")
v = tool("search", query="phase two")
check(any(i["id"] == aid for i in v["result"]["items"]), "search sees edited text")

# an html artifact still works the old way
h = tool("create", title="page", html="<!doctype html><h1>p</h1>")["result"]
check(h["kind"] == "html", "html create unchanged")
v = tool("rewrite", id=h["id"], base_version=1, markdown="# no")
check("pass its new content as html" in json.dumps(v), "rewrite with markdown on html refused")

with sync_playwright() as p:
    b = p.chromium.launch()
    ctx = b.new_context(viewport={"width": 1300, "height": 800})
    pg = ctx.new_page()
    pg.goto(BASE + "/auth/login?email=marko@test&next=/")
    errs = []
    pg.on("console", lambda m: errs.append(m.text) if m.type == "error" else None)

    # source + headers
    resp = pg.request.get(BASE + "/a/" + aid + "/source")
    check(resp.status == 200 and "whiteboard in phase two" in resp.text() and resp.headers["content-type"].startswith("text/plain"), "source returns markdown text")
    resp = pg.request.get(BASE + "/a/" + aid + "/source?download=1")
    check('filename="walrus-prd.md"' in resp.headers.get("content-disposition", ""), "source download filename")
    resp = pg.request.get(BASE + "/c/" + aid + "/2", headers={"Sec-Fetch-Dest": "iframe"})
    csp = resp.headers.get("content-security-policy", "")
    check("'nonce-" in csp and "'strict-dynamic'" in csp and "unsafe-inline' blob" not in csp.split("script-src")[1].split(";")[0], "frame CSP: nonce-only scripts")
    resp = pg.request.get(BASE + "/c/" + aid + "/1", headers={"Sec-Fetch-Dest": "iframe"})
    body = resp.text().split('<main class="md" id="doc">')[1].split('</main>')[0]
    check("whiteboard later" in body, "old version renders old text")
    check(all(x not in body for x in ["<script>window.__pwned", "onerror", "dns-prefetch", "http-equiv", "<style>body", "<iframe", "<form"]) and "<details>" in body,
          "sanitizer drops script/link/meta/style/iframe/form, keeps details")
    check("img-src data: blob: https://cdnjs" in csp and "https:;" not in csp and "allow-popups" not in csp, "markdown CSP: no remote images, no popups")
    check(resp.headers.get("permissions-policy", "").startswith("camera=()"), "permissions policy header")

    pg.goto(BASE + "/a/" + aid)
    fr = wait_for(lambda: pg.frame_locator("#frame") and [f for f in pg.frames if "/c/" + aid in f.url])
    f = fr[0]
    check(wait_for(lambda: f.locator("#doc h2#goals").count() == 1), "headings get ids")
    check(f.locator(".table-wrap table td").first.inner_text() == "alpha", "GFM table rendered + wrapped")
    check(f.locator("input[type=checkbox]").count() == 2, "task list checkboxes")
    check(f.locator(".footnotes li").count() == 1, "footnotes")
    check(f.locator("details summary").inner_text() == "More" and f.locator("kbd").count() == 1, "raw HTML formatting passes through")
    check(f.evaluate("window.__pwned") is None, "raw <script> and onerror in markdown do not run")
    check(f.evaluate("[...document.images].filter(i => i.alt == 'remote')[0].naturalWidth") == 0 and
          f.evaluate("[...document.images].filter(i => i.alt == 'inline')[0].naturalWidth") == 1, "remote image blocked, data: image shown")
    check(f.locator("#toc a").count() >= 3 and f.locator("#toc").is_visible(), "contents sidebar on a wide screen")
    with ctx.expect_page(timeout=5000) as newp:
        f.locator("a[href='https://example.com']").click()
    check(newp.value.url.startswith("https://example.com"), "external links open a new tab (via the shell)")
    newp.value.close()
    check(f.locator("a.anchor").count() >= 4, "heading anchors")
    mer = wait_for(lambda: f.locator(".mermaid svg").count() == 1, 20)
    check(bool(mer), "mermaid diagram rendered (CDN)")
    hl = wait_for(lambda: f.locator("pre code.hljs .hljs-keyword").count() >= 1, 15)
    check(bool(hl), "code highlighted (CDN)")
    src = pg.locator("#src").get_attribute("href")
    check(src == "/a/" + aid + "/source", "shell source link")

    # live reload keeps the reader's place
    f.evaluate("window.scrollTo(0, 900)")
    time.sleep(0.6)
    y0 = f.evaluate("window.scrollY")
    tool("edit", id=aid, base_version=2, edits=[{"old_text": "Intro text", "new_text": "Intro text (updated)"}])
    def reloaded():
        fs = [x for x in pg.frames if "/c/" + aid in x.url]
        return fs and pg.locator("#ver").inner_text() == "v3" and "(updated)" in fs[0].locator("#doc p").first.inner_text() and fs[0]
    f3 = wait_for(reloaded, 10)
    check(bool(f3), "live reload to v3")
    if f3:
        time.sleep(0.5)
        y1 = f3.evaluate("window.scrollY")
        check(abs(y1 - y0) < 4 and y0 > 500, "scroll position kept across reload (%s -> %s)" % (y0, y1))

    # phone width: no sidebar
    ph = b.new_context(viewport={"width": 390, "height": 800}, storage_state=ctx.storage_state()).new_page()
    ph.goto(BASE + "/a/" + aid + "/raw")
    rf = ph.frame_locator("#frame")
    check(wait_for(lambda: rf.locator("#doc h1").count() == 1) and not rf.locator("#toc").is_visible(), "raw view on a phone: no sidebar")
    pf = [x for x in ph.frames if "/c/" in x.url][0]
    wide = pf.evaluate("document.documentElement.scrollWidth <= window.innerWidth + 1")
    check(wide, "no horizontal overflow on a phone")
    ph.screenshot(path="/tmp/vt/md_phone.png", full_page=False)

    pg.goto(BASE + "/")
    check(wait_for(lambda: pg.locator(".card .tag.kind").count() == 1), "index shows doc tag")
    pg.goto(BASE + "/a/" + aid)
    time.sleep(2)
    pg.screenshot(path="/tmp/vt/md_shell.png")
    bad = [e for e in errs if "Content Security Policy" not in e and "pwned" not in e]
    check(not bad, "no unexpected console errors: " + "; ".join(bad)[:300])
    b.close()
print("FAILS:", fails)
