/* vitrine page hardening. Runs before any page script, in html pages only.
   The CSP blocks fetch, images and sockets to other hosts, but no browser lets
   a policy block WebRTC, and child frames run their own code. So:
   - WebRTC is removed from this realm;
   - child frames (iframe, object, embed...) are removed as soon as they are
     inserted, before they can load: they would bring WebRTC back;
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
  var tagName = getDesc(EP, 'tagName').get, nodeType = getDesc(NP, 'nodeType').get, parentNode = getDesc(NP, 'parentNode').get;
  var lenDesc = getDesc(W, 'length'), winLen = lenDesc && lenDesc.get;
  var framed = PARENT !== W;
  var URL_ = W.URL, Node_ = W.Node;
  function re(r, s) { return apply(test, r, [s]); }
  function frames() { return winLen ? apply(winLen, W, []) : 0; }

  // 1. WebRTC out of this realm (workers don't have it)
  Object.getOwnPropertyNames(W).forEach(function (n) {
    if (re(/^(webkit|moz)?RTC/, n)) { try { delete W[n]; } catch (e) { } }
  });

  // 2. child frames and network hints
  var FRAME = /^(IFRAME|FRAME|FRAMESET|OBJECT|EMBED|PORTAL|FENCEDFRAME)$/;
  var HINT = /(^|\s)(dns-prefetch|preconnect|prefetch|prerender)(\s|$)/i;
  var SEL = 'iframe,frame,frameset,object,embed,portal,fencedframe,link[rel]';
  var shadows = [];
  function bad(el) {
    var t = apply(tagName, el, []);
    if (re(FRAME, t)) return true;
    return t === 'LINK' && re(HINT, apply(getAttr, el, ['rel']) || '');
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
      if (a instanceof Node_) apply(push, out, [apply(nodeType, a, []) === 11 ? base : a]);
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
  function wrapFn(proto, name, pick) {
    var d = proto && getDesc(proto, name);
    if (!d || typeof d.value !== 'function') return;
    var f = d.value;
    d.value = function () {
      var targets = pick(this, arguments);
      try { return apply(f, this, arguments); } finally { finish(targets); }
    };
    defProp(proto, name, d);
  }
  function wrapSet(proto, name, pick) {
    var d = proto && getDesc(proto, name);
    if (!d || !d.set) return;
    var s = d.set;
    d.set = function (v) {
      var targets = pick(this, [v]);
      try { apply(s, this, [v]); } finally { finish(targets); }
    };
    defProp(proto, name, d);
  }
  wrapFn(NP, 'appendChild', T.first); wrapFn(NP, 'insertBefore', T.first); wrapFn(NP, 'replaceChild', T.first);
  [EP, DP, FP].forEach(function (p) {
    ['append', 'prepend', 'replaceChildren'].forEach(function (n) { wrapFn(p, n, T.args); });
    wrapFn(p, 'moveBefore', T.first);
  });
  [EP, CharacterData.prototype, DocumentType.prototype].forEach(function (p) {
    ['after', 'before', 'replaceWith'].forEach(function (n) { wrapFn(p, n, T.sibArgs); });
  });
  wrapFn(EP, 'insertAdjacentElement', T.parent);
  wrapFn(EP, 'insertAdjacentHTML', T.parent);
  wrapFn(EP, 'setHTML', T.self); wrapFn(SRP, 'setHTML', T.self);
  wrapSet(EP, 'innerHTML', T.self); wrapSet(EP, 'outerHTML', T.parent); wrapSet(SRP, 'innerHTML', T.self);
  wrapFn(EP, 'setAttribute', T.self); wrapFn(EP, 'setAttributeNS', T.self); wrapFn(EP, 'toggleAttribute', T.self);
  if (W.HTMLLinkElement) wrapSet(HTMLLinkElement.prototype, 'rel', T.self);
  wrapFn(Range.prototype, 'insertNode', T.doc); wrapFn(Range.prototype, 'surroundContents', T.doc);
  wrapFn(DP, 'execCommand', T.doc);
  // declarative shadow DOM would hide frames in closed shadow roots: parse without it
  function noDSD(s) { return apply(strReplace, String(s), [/shadowroot(mode|delegatesfocus|clonable|serializable)/gi, 'data-x-$1']); }
  ['write', 'writeln'].forEach(function (n) {
    var f = DP[n];
    DP[n] = function () {
      var a = [];
      for (var i = 0; i < arguments.length; i++) apply(push, a, [noDSD(arguments[i])]);
      try { return apply(f, this, a); } finally { sweepAll(); }
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
