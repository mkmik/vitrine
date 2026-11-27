/* vitrine page hardening. Runs before any page script, in html pages only.
   The CSP blocks fetch, images and sockets to other hosts, but no browser lets
   a policy block WebRTC, and child frames run their own code. So:
   - WebRTC is removed from this realm;
   - child frames (iframe, object, embed...) never reach the page: they would
     bring WebRTC back. Page code can run in the middle of an insert (custom
     element callbacks, inserted scripts, the load event a new frame fires
     right away), so frames are taken out of what is inserted before the
     insert, not swept after it;
   - network hints (dns-prefetch, preconnect, prefetch, prerender) are removed;
   - popups are off (sandbox), so links and window.open go through the shell,
     which opens http(s) URLs in a new tab on a real click only.
   Every primitive is captured up front, so page code can't swap them out. */
(function () {
  'use strict';
  var W = window, D = document, PARENT = W.parent;
  var apply = Reflect.apply, getDesc = Object.getOwnPropertyDescriptor, defProp = Object.defineProperty;
  var test = RegExp.prototype.test, push = Array.prototype.push, strReplace = String.prototype.replace;
  var EP = Element.prototype, NP = Node.prototype, DP = Document.prototype, FP = DocumentFragment.prototype;
  var SRP = W.ShadowRoot ? ShadowRoot.prototype : null;
  var qsaE = EP.querySelectorAll, qsaD = DP.querySelectorAll, qsaF = FP.querySelectorAll;
  var removeEl = EP.remove, getAttr = EP.getAttribute, closest = EP.closest;
  var localName = getDesc(EP, 'localName').get, nodeType = getDesc(NP, 'nodeType').get, parentNode = getDesc(NP, 'parentNode').get;
  var lenDesc = getDesc(W, 'length'), winLen = lenDesc && lenDesc.get;
  var framed = PARENT !== W;
  var URL_ = W.URL, stopNow = Event.prototype.stopImmediatePropagation, evTarget = getDesc(Event.prototype, 'target').get;
  function re(r, s) { return apply(test, r, [s]); }
  function frames() { return winLen ? apply(winLen, W, []) : 0; }

  // 1. WebRTC out of this realm (workers don't have it)
  Object.getOwnPropertyNames(W).forEach(function (n) {
    if (re(/^(webkit|moz)?RTC/, n)) { try { delete W[n]; } catch (e) { } }
  });

  // 2. child frames and network hints
  // localName, not tagName: createElementNS(xhtml, 'x:iframe') is an iframe whose tagName is X:IFRAME
  var NAMES = ['iframe', 'frame', 'frameset', 'object', 'embed', 'portal', 'fencedframe'];
  var FRAME = new RegExp('^(' + NAMES.join('|') + ')$');
  // an html start tag that would make a frame, and (document.write only) the
  // start of one cut off at the end of a chunk, finished by the next write
  var FRAMETAG = new RegExp('<(?=(' + NAMES.join('|') + ')([\\s/>]|$))', 'gi');
  var prefixes = [];
  NAMES.forEach(function (n) { for (var i = 1; i < n.length; i++) prefixes.push(n.slice(0, i)); });
  var FRAMETAIL = new RegExp('<(?=(' + prefixes.join('|') + ')?$)', 'i');
  var HINT = /(^|\s)(dns-prefetch|preconnect|prefetch|prerender)(\s|$)/i;
  var SEL = 'iframe,frame,frameset,object,embed,portal,fencedframe,link[rel]';
  var shadows = [];
  function bad(el) {
    var t = apply(localName, el, []);
    if (re(FRAME, t)) return true;
    return t === 'link' && re(HINT, apply(getAttr, el, ['rel']) || '');
  }
  function typeOf(n) { try { return apply(nodeType, n, []); } catch (e) { return 0; } }
  // `<iframe` -> `&lt;iframe`: text where a tag was, the same value inside an attribute
  function defang(s) { return s == null ? s : apply(strReplace, String(s), [FRAMETAG, '&lt;']); }
  // false if n is itself a frame; otherwise strips frames out of it
  function clean(n) {
    var t = typeOf(n);
    if (t === 1 && re(FRAME, apply(localName, n, []))) return false;
    if (t === 1 || t === 11) sweepIn(n);
    return true;
  }
  // the arguments to insert with: html defanged, frames dropped (only the one at
  // `at`, or any when at < 0); null when the one node to insert is a frame
  function prep(args, at, html) {
    var out = [];
    for (var i = 0; i < args.length; i++) {
      var a = args[i];
      if (at < 0 || i === at) {
        if (html) a = defang(a);
        else if (!clean(a)) { if (at < 0) continue; return null; }
      }
      apply(push, out, [a]);
    }
    return out;
  }
  function sweepIn(root) {
    if (!root) return;
    var t = apply(nodeType, root, []), list;
    if (t === 1) { if (bad(root)) { apply(removeEl, root, []); return; } list = apply(qsaE, root, [SEL]); }
    else if (t === 9) list = apply(qsaD, root, [SEL]);
    else if (t === 11) list = apply(qsaF, root, [SEL]);
    else return;
    for (var i = 0; i < list.length; i++) if (bad(list[i])) apply(removeEl, list[i], []);
  }
  function sweepAll() {
    sweepIn(D);
    for (var i = 0; i < shadows.length; i++) sweepIn(shadows[i]);
    // a frame we could not find should not exist; if one does, blank it out
    for (var j = 0; j < frames(); j++) { try { W[j].location.replace('about:blank'); } catch (e) { } }
  }
  function finish(targets) {
    for (var i = 0; i < targets.length; i++) sweepIn(targets[i]);
    if (frames() > 0) sweepAll();
  }
  // what to sweep after a call, decided before it (outerHTML/replaceWith detach `this`)
  function parentOf(self) { return apply(parentNode, self, []) || self; }
  function inserted(self, args, base) {
    var out = [];
    for (var i = 0; i < args.length; i++) {
      var a = args[i];
      var t = typeOf(a);
      if (t) apply(push, out, [t === 11 ? base : a]);
    }
    return out;
  }
  var T = {
    self: function (self) { return [self]; },
    parent: function (self) { return [parentOf(self)]; },
    doc: function () { return [D]; },
    args: function (self, args) { return inserted(self, args, self); },
    sibArgs: function (self, args) { return inserted(self, args, parentOf(self)); },
    first: function (self, args) { return inserted(self, [args[0]], self); },
  };
  // at: which argument is inserted (-1: all of them, null: none); html: it is markup
  function wrapFn(proto, name, pick, at, html) {
    var d = proto && getDesc(proto, name);
    if (!d || typeof d.value !== 'function') return;
    var f = d.value;
    d.value = function () {
      var args = at === null ? arguments : prep(arguments, at, html);
      if (!args) return arguments[at];
      var targets = pick(this, args);
      try { return apply(f, this, args); } finally { finish(targets); }
    };
    defProp(proto, name, d);
  }
  function wrapSet(proto, name, pick, html) {
    var d = proto && getDesc(proto, name);
    if (!d || !d.set) return;
    var s = d.set;
    d.set = function (v) {
      var args = prep([v], 0, html);
      if (!args) return;
      var targets = pick(this, args);
      try { apply(s, this, args); } finally { finish(targets); }
    };
    defProp(proto, name, d);
  }
  var N = false, H = true;
  wrapFn(NP, 'appendChild', T.first, 0, N); wrapFn(NP, 'insertBefore', T.first, 0, N); wrapFn(NP, 'replaceChild', T.first, 0, N);
  [EP, DP, FP].forEach(function (p) {
    ['append', 'prepend', 'replaceChildren'].forEach(function (n) { wrapFn(p, n, T.args, -1, N); });
    wrapFn(p, 'moveBefore', T.first, 0, N);
  });
  [EP, CharacterData.prototype, DocumentType.prototype].forEach(function (p) {
    ['after', 'before', 'replaceWith'].forEach(function (n) { wrapFn(p, n, T.sibArgs, -1, N); });
  });
  wrapFn(EP, 'insertAdjacentElement', T.parent, 1, N);
  wrapFn(EP, 'insertAdjacentHTML', T.parent, 1, H);
  wrapFn(EP, 'setHTML', T.self, 0, H); wrapFn(SRP, 'setHTML', T.self, 0, H);
  wrapSet(EP, 'innerHTML', T.self, H); wrapSet(EP, 'outerHTML', T.parent, H); wrapSet(SRP, 'innerHTML', T.self, H);
  wrapFn(EP, 'setAttribute', T.self, null); wrapFn(EP, 'setAttributeNS', T.self, null); wrapFn(EP, 'toggleAttribute', T.self, null);
  if (W.HTMLLinkElement) wrapSet(HTMLLinkElement.prototype, 'rel', T.self, N);
  wrapFn(Range.prototype, 'insertNode', T.doc, 0, N); wrapFn(Range.prototype, 'surroundContents', T.doc, 0, N);
  wrapFn(DP, 'execCommand', T.doc, 2, H);
  // the less obvious ways to insert a node
  wrapSet(DP, 'body', T.doc, N);
  if (W.HTMLTableElement) ['caption', 'tHead', 'tFoot'].forEach(function (n) { wrapSet(HTMLTableElement.prototype, n, T.self, N); });
  if (W.HTMLSelectElement) wrapFn(HTMLSelectElement.prototype, 'add', T.self, 0, N);
  if (W.HTMLOptionsCollection) wrapFn(HTMLOptionsCollection.prototype, 'add', T.doc, 0, N);
  // declarative shadow DOM would hide frames in closed shadow roots: parse without it
  // (and no frame tags, as for innerHTML)
  function noDSD(s) { return defang(apply(strReplace, String(s), [/shadowroot(mode|delegatesfocus|clonable|serializable)/gi, 'data-x-$1'])); }
  ['write', 'writeln'].forEach(function (n) {
    var f = DP[n];
    DP[n] = function () {
      var a = [];
      for (var i = 0; i < arguments.length; i++) apply(push, a, [apply(strReplace, noDSD(arguments[i]), [FRAMETAIL, '&lt;'])]);
      try { return apply(f, this, a); } finally { sweepAll(); finish([this]); }
    };
  });
  [EP, SRP].forEach(function (p) {
    var f = p && p.setHTMLUnsafe;
    if (f) p.setHTMLUnsafe = function (s) { try { return apply(f, this, [noDSD(s)]); } finally { finish([this]); } };
  });
  if (Document.parseHTMLUnsafe) {
    var parse = Document.parseHTMLUnsafe;
    Document.parseHTMLUnsafe = function (s) { return apply(parse, Document, [noDSD(s)]); };
  }
  // parser-inserted markup and anything missed: a microtask later, which is
  // still before the next script runs and before a frame can load
  var OBS = { childList: true, subtree: true, attributes: true, attributeFilter: ['rel'] };
  var mo = new MutationObserver(sweepAll);
  mo.observe(D, OBS);
  // a frame the parser inserts fires load right away in some engines, before
  // the observer runs: take it out first, ahead of every other listener
  D.addEventListener('load', function (e) {
    var t = apply(evTarget, e, []);
    if (t !== D && typeOf(t) === 1 && re(FRAME, apply(localName, t, []))) { apply(stopNow, e, []); apply(removeEl, t, []); }
  }, true);
  // every shadow root, open or closed, is swept and watched too
  var attach = EP.attachShadow;
  EP.attachShadow = function () {
    var r = apply(attach, this, arguments);
    apply(push, shadows, [r]);
    try { mo.observe(r, OBS); } catch (e) { }
    return r;
  };

  // 3. links and window.open go through the shell
  function openOut(url) {
    var u;
    try { u = new URL_(String(url), D.baseURI); } catch (e) { return; }
    if (u.protocol !== 'http:' && u.protocol !== 'https:' && u.protocol !== 'mailto:') return;
    if (framed) { try { PARENT.postMessage({ __vitrine_open: 1, url: u.href }, '*'); } catch (e) { } }
    else W.location.assign(u.href);
  }
  defProp(W, 'open', { value: function (url) { if (url) openOut(url); return null; }, writable: false, configurable: false });
  W.addEventListener('click', function (e) {
    if (e.defaultPrevented || e.button !== 0 || !e.target || apply(nodeType, e.target, []) !== 1) return;
    var a = apply(closest, e.target, ['a[href]']);
    if (!a) return;
    var href = apply(getAttr, a, ['href']);
    if (!href || href.charAt(0) === '#' || re(/^\s*javascript:/i, href)) return;
    e.preventDefault();
    openOut(a.href);
  });
})();
