/* vitrine markdown renderer: progressive touches on the server-rendered document. */
(function () {
  'use strict';
  var doc = document.getElementById('doc');
  var CDN = 'https://cdn.jsdelivr.net/npm/';
  var dark = window.matchMedia && matchMedia('(prefers-color-scheme: dark)').matches;

  // links leave the sandbox in a new tab; in-page anchors stay
  doc.querySelectorAll('a[href]').forEach(function (a) {
    var h = a.getAttribute('href');
    if (h && h.charAt(0) !== '#') { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
  });

  // wide tables scroll on their own instead of widening the page
  doc.querySelectorAll('table').forEach(function (t) {
    if (t.parentNode.classList && t.parentNode.classList.contains('table-wrap')) return;
    var w = document.createElement('div'); w.className = 'table-wrap';
    t.parentNode.insertBefore(w, t); w.appendChild(t);
  });

  // heading anchors + contents
  var heads = [].slice.call(doc.querySelectorAll('h1[id], h2[id], h3[id], h4[id]'));
  heads.forEach(function (h) {
    var a = document.createElement('a'); a.className = 'anchor'; a.href = '#' + h.id; a.textContent = '#';
    a.setAttribute('aria-label', 'Link to this section');
    h.insertBefore(a, h.firstChild);
  });
  var tocHeads = heads.filter(function (h) { return h.tagName === 'H2' || h.tagName === 'H3'; });
  if (tocHeads.length >= 3) {
    var toc = document.getElementById('toc');
    var b = document.createElement('b'); b.textContent = 'Contents'; toc.appendChild(b);
    var links = new Map();
    tocHeads.forEach(function (h) {
      var a = document.createElement('a'); a.href = '#' + h.id; a.className = h.tagName === 'H3' ? 'l3' : 'l2';
      a.textContent = h.textContent.replace(/^#/, '').trim();
      toc.appendChild(a); links.set(h, a);
    });
    toc.hidden = false; document.body.classList.add('has-toc');
    var cur = null;
    var mark = function () {
      var y = window.scrollY + 80, on = tocHeads[0];
      for (var i = 0; i < tocHeads.length; i++) { if (tocHeads[i].offsetTop <= y) on = tocHeads[i]; else break; }
      if (on !== cur) { if (cur) links.get(cur).classList.remove('on'); cur = on; links.get(on).classList.add('on'); }
    };
    window.addEventListener('scroll', mark, { passive: true }); mark();
  }

  // keep the reader's place across live reloads. The shell hands back
  // ?va=<heading id>&vi=<blocks after it>&vo=<px into that block>&vy=<y>:
  // the block at the top of the viewport, located from the nearest heading
  // so edits elsewhere don't move it. The browser's scroll anchoring then
  // absorbs diagrams that grow after load.
  var top = function (el) { return el.getBoundingClientRect().top + window.scrollY; };
  var isHead = function (el) { return /^H[1-6]$/.test(el.tagName) && el.id; };
  var q = new URLSearchParams(location.search);
  if (q.has('vy') && !location.hash) {
    var target = function () {
      var el = q.get('va') && document.getElementById(q.get('va'));
      if (!el || el.parentNode !== doc) return +q.get('vy');
      for (var n = +q.get('vi') || 0; n > 0 && el.nextElementSibling; n--) el = el.nextElementSibling;
      return top(el) + (+q.get('vo') || 0);
    };
    var go = function () { window.scrollTo(0, Math.max(0, Math.round(target()))); };
    go(); window.addEventListener('load', go);
  }
  var postT = 0;
  window.addEventListener('scroll', function () {
    if (postT) return;
    postT = setTimeout(function () {
      postT = 0;
      var y = window.scrollY, b = doc.firstElementChild;
      while (b && b.nextElementSibling && top(b) + b.offsetHeight <= y) b = b.nextElementSibling;
      var msg = { __vitrine_md: 1, y: Math.round(y), a: '', i: 0, o: 0 };
      if (b) {
        var h = b, i = 0;
        while (h && !isHead(h)) { h = h.previousElementSibling; i++; }
        if (h) { msg.a = h.id; msg.i = i; msg.o = Math.round(y - top(b)); }
      }
      try { parent.postMessage(msg, '*'); } catch (e) { }
    }, 150);
  }, { passive: true });

  // diagrams
  var mer = [].slice.call(doc.querySelectorAll('pre > code.language-mermaid'));
  if (mer.length) {
    var nodes = mer.map(function (c) {
      var d = document.createElement('div'); d.className = 'mermaid'; d.textContent = c.textContent;
      d._pre = c.parentNode; c.parentNode.replaceWith(d); return d;
    });
    import(CDN + 'mermaid@11/dist/mermaid.esm.min.mjs').then(function (mod) {
      var mermaid = mod.default;
      mermaid.initialize({ startOnLoad: false, securityLevel: 'strict', theme: dark ? 'dark' : 'default' });
      return mermaid.run({ nodes: nodes, suppressErrors: true });
    }).catch(function () {
      nodes.forEach(function (d) { if (!d.querySelector('svg')) d.replaceWith(d._pre); });
    });
  }

  // code highlighting
  var code = [].slice.call(doc.querySelectorAll('pre > code[class*="language-"]'));
  if (code.length) {
    import(CDN + '@highlightjs/cdn-assets@11/es/highlight.min.js').then(function (mod) {
      var hljs = mod.default;
      hljs.configure({ ignoreUnescapedHTML: true });
      code.forEach(function (c) {
        var lang = (/language-([\w+#-]+)/.exec(c.className) || [])[1];
        if (lang && hljs.getLanguage(lang)) hljs.highlightElement(c);
      });
    }).catch(function () { });
  }
})();
