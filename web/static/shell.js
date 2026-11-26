/* vitrine shell: chrome around one artifact, live reload, and the postMessage
   bridge that serves the page's claude.use() calls over one WebSocket. */
(function () {
  'use strict';
  var boot = JSON.parse(document.body.dataset.boot);
  var A = boot.artifact;
  var $ = function (id) { return document.getElementById(id); };
  var frame = $('frame');
  var LV = { none: 0, view: 1, interact: 2, edit: 3, owner: 4 };
  var state = { head: A.head, shown: A.shown, follow: A.follow, level: A.level, pinned: A.pinned, title: A.title,
    caps: A.caps || [], capabilities: {}, peer: null, connected: false, gen: 0, nonce: null, pendingHead: 0, updated: A.updated_at };
  var auto = true, mdPos = null;
  try { auto = localStorage.getItem('vitrine.auto') !== '0'; } catch (e) { }

  function api(method, path, body) {
    return fetch(path, { method: method, credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-CSRF': boot.csrf || '' },
      body: body ? JSON.stringify(body) : undefined }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error((j.error && (j.error.message || j.error)) || r.statusText);
        return j;
      });
    });
  }
  function fmtTime(t) { return new Date(t * 1000).toLocaleString(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }); }
  function el(tag, attrs, kids) {
    var e = document.createElement(tag);
    for (var k in attrs || {}) { if (k === 'text') e.textContent = attrs[k]; else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), attrs[k]); else e.setAttribute(k, attrs[k]); }
    (kids || []).forEach(function (c) { if (c) e.appendChild(typeof c === 'string' ? document.createTextNode(c) : c); });
    return e;
  }

  // ---------------- chrome ----------------
  function render() {
    $('titleText').textContent = state.title;
    document.title = state.title;
    var lv = LV[state.level] || 0;
    $('renameBtn').hidden = lv < LV.edit;
    $('dupBtn').hidden = !boot.can_create;
    $('thumbBtn').hidden = lv < LV.edit;
    $('title').disabled = $('renameBtn').hidden && $('dupBtn').hidden;
    $('edited').textContent = state.updated ? 'Edited ' + fmtTime(state.updated) : '';
    var label = 'v' + state.shown;
    if (state.pinned) label += ' · pinned';
    else if (state.shown !== state.head) label += ' of ' + state.head;
    $('ver').textContent = label;
    $('historyBtn').hidden = lv < LV.edit;
    $('shareBtn').hidden = lv < LV.owner;
    $('copyBtn').hidden = lv >= LV.owner;
    $('autoWrap').hidden = !state.follow;
    $('auto').checked = auto;
    $('raw').href = '/a/' + A.id + '/raw' + (state.follow ? '' : '/v/' + state.shown);
    $('src').href = '/a/' + A.id + '/source' + (state.follow ? '' : '/v/' + state.shown);
    $('src').title = A.kind === 'markdown' ? 'Markdown source' : 'Page source';
    var b = $('badge');
    if (state.pendingHead && state.follow) { b.hidden = false; b.textContent = 'New version v' + state.pendingHead + ' — refresh'; }
    else if (!state.follow && !state.pinned && state.head > state.shown && lv >= LV.edit) { b.hidden = false; b.textContent = 'Latest is v' + state.head; }
    else b.hidden = true;
    $('conn').className = 'dot' + (state.connected ? ' on' : '');
    $('conn').title = state.connected ? 'Live' : 'Reconnecting…';
  }
  // A page can't navigate its frame elsewhere: this page's CSP allows frames
  // from /c/ only, and browsers check that for the frame's own navigations.
  // links and window.open from the page: http(s)/mailto only, on a real click only
  function openOut(url) {
    var u;
    try { u = new URL(url); } catch (e) { return; }
    if (!/^(https?|mailto):$/.test(u.protocol)) return;
    if (navigator.userActivation && !navigator.userActivation.isActive) return;
    window.open(u.href, '_blank', 'noopener,noreferrer');
  }
  function load(n) {
    state.shown = n;
    state.pendingHead = 0;
    // documents reopen where the reader was
    var at = '';
    if (A.kind === 'markdown' && mdPos && mdPos.y > 0) {
      at = '?vy=' + (mdPos.y | 0) + (mdPos.a ? '&va=' + encodeURIComponent(mdPos.a) + '&vi=' + (mdPos.i | 0) + '&vo=' + (mdPos.o | 0) : '');
    }
    if (A.kind !== 'markdown') mdPos = null; // a page opens at the top
    frame.src = '/c/' + A.id + '/' + n + at;
    render();
  }
  function goVersion(n, follow) {
    state.follow = follow;
    history.replaceState(null, '', follow ? '/a/' + A.id : '/a/' + A.id + '/v/' + n);
    load(n);
  }
  $('auto').addEventListener('change', function (e) {
    auto = e.target.checked;
    try { localStorage.setItem('vitrine.auto', auto ? '1' : '0'); } catch (x) { }
    if (auto && state.pendingHead) load(state.pendingHead);
    render();
  });
  $('reload').addEventListener('click', function () { load(state.follow ? state.head : state.shown); });
  $('badge').addEventListener('click', function () { if (state.pendingHead) load(state.pendingHead); else goVersion(state.head, true); });
  $('copyBtn').addEventListener('click', function () {
    if (navigator.clipboard) navigator.clipboard.writeText(A.url).then(function () { $('copyBtn').textContent = 'Copied'; setTimeout(function () { $('copyBtn').textContent = 'Copy link'; }, 1500); });
  });

  // title menu: rename, duplicate
  var menu = $('titleMenu'), titleEdit = $('titleEdit');
  menu.addEventListener('beforetoggle', function (e) {
    if (e.newState !== 'open') return;
    var r = $('title').getBoundingClientRect();
    menu.style.top = (r.bottom + 6) + 'px'; menu.style.left = r.left + 'px';
  });
  // clicks inside the artifact frame never reach this document; close on focus loss instead
  window.addEventListener('blur', function () { if (menu.matches(':popover-open')) menu.hidePopover(); });
  $('renameBtn').addEventListener('click', function () {
    menu.hidePopover();
    titleEdit.value = state.title;
    $('title').hidden = true; titleEdit.hidden = false;
    titleEdit.focus(); titleEdit.select();
  });
  function endRename(save) {
    if (titleEdit.hidden) return;
    var v = titleEdit.value.trim(), old = state.title;
    titleEdit.hidden = true; $('title').hidden = false;
    if (!save || !v || v === old) return;
    state.title = v; render();
    api('POST', '/api/artifacts/' + A.id + '/meta', { title: v }).then(function () { state.updated = Date.now() / 1000; render(); },
      function (e) { state.title = old; render(); alert(e.message); });
  }
  titleEdit.addEventListener('keydown', function (e) {
    if (e.key === 'Enter') endRename(true); else if (e.key === 'Escape') endRename(false);
  });
  titleEdit.addEventListener('blur', function () { endRename(true); });
  $('dupBtn').addEventListener('click', function () {
    menu.hidePopover();
    api('POST', '/api/artifacts/' + A.id + '/duplicate', { version: state.follow ? 0 : state.shown })
      .then(function (j) { location.href = '/a/' + j.id; }, function (e) { alert(e.message); });
  });

  // thumbnail: the page at a scroll offset ("current view"), or an uploaded image
  var thumb = boot.thumb || { y: 0, img: 0 }, pick = null;
  function showThumb(p) {
    var box = $('thumbPrev'), paper = el('div', { class: 'paper' });
    box.textContent = ''; box.appendChild(paper);
    if (p.file || p.img) paper.appendChild(el('img', { alt: '', src: p.file ? URL.createObjectURL(p.file) : '/a/' + A.id + '/thumb?v=' + p.img }));
    else paper.appendChild(el('iframe', { src: '/c/' + A.id + '/' + state.shown + '?raw=1' + (p.y ? '&vy=' + p.y : ''), tabindex: '-1', 'aria-hidden': 'true',
      sandbox: 'allow-scripts', referrerpolicy: 'no-referrer' }));
    box.style.setProperty('--s', paper.clientWidth / 1024);
  }
  $('thumbBtn').addEventListener('click', function () {
    menu.hidePopover();
    pick = null; $('thumbErr').textContent = '';
    $('thumbDlg').showModal();
    showThumb(thumb);
  });
  $('thumbView').addEventListener('click', function () { pick = { y: mdPos ? mdPos.y : 0 }; showThumb(pick); });
  $('thumbUp').addEventListener('click', function () { $('thumbFile').click(); });
  $('thumbFile').addEventListener('change', function () {
    var f = this.files[0]; this.value = '';
    if (!f) return;
    if (f.size > 2 << 20) { $('thumbErr').textContent = 'Image too large (2 MB at most).'; return; }
    $('thumbErr').textContent = '';
    pick = { file: f }; showThumb(pick);
  });
  $('thumbSave').addEventListener('click', function () {
    if (!pick) return $('thumbDlg').close();
    var body = pick.file || JSON.stringify({ y: pick.y });
    fetch('/api/artifacts/' + A.id + '/thumb', { method: 'POST', credentials: 'same-origin', body: body,
      headers: { 'Content-Type': pick.file ? pick.file.type || 'application/octet-stream' : 'application/json', 'X-CSRF': boot.csrf || '' } })
      .then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { if (!r.ok) throw new Error(j.error || r.statusText); return j; }); })
      .then(function (j) { thumb = j; $('thumbDlg').close(); }, function (e) { $('thumbErr').textContent = e.message; });
  });

  // history panel
  function openHistory() {
    $('history').hidden = false;
    var ol = $('versions'); ol.textContent = 'Loading…';
    api('GET', '/api/artifacts/' + A.id + '/history').then(function (j) {
      ol.textContent = '';
      j.versions.forEach(function (v) {
        ol.appendChild(el('li', { class: v.version === state.shown ? 'cur' : '' }, [
          el('button', { class: 'link', text: 'v' + v.version + (v.version === j.head ? ' (latest)' : ''), onclick: function () { goVersion(v.version, v.version === j.head); openHistory(); } }),
          el('span', { class: 'muted small', text: fmtTime(v.created_at) + ' · ' + (v.author || '') + (v.note ? ' · ' + v.note : '') + ' · ' + Math.ceil(v.size / 1024) + ' KB' }),
          v.version !== j.head ? el('button', { class: 'small', text: 'Restore', onclick: function () {
            if (!confirm('Make v' + v.version + ' the new latest version?')) return;
            api('POST', '/api/artifacts/' + A.id + '/restore', { version: v.version }).then(function (r) { goVersion(r.head, true); setTimeout(openHistory, 300); }).catch(function (e) { alert(e.message); });
          } }) : null
        ]));
      });
    }).catch(function (e) { ol.textContent = e.message; });
  }
  $('historyBtn').addEventListener('click', function () { if ($('history').hidden) openHistory(); else $('history').hidden = true; });
  document.querySelector('#history [data-close]').addEventListener('click', function () { $('history').hidden = true; });

  // share dialog
  var LVNAME = { view: 'Viewer', interact: 'Interactor', edit: 'Editor' };
  var ICON = {
    lock: 'M7 11V8a5 5 0 0 1 10 0v3M5 11h14v10H5z',
    org: 'M4 21V4h10v17M14 9h6v12M2 21h20M7 8h2M7 12h2M7 16h2',
    globe: 'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM3 12h18M12 3c3 3 3 15 0 18M12 3c-3 3-3 15 0 18'
  };
  function icon(name) {
    var ns = 'http://www.w3.org/2000/svg', s = document.createElementNS(ns, 'svg'), p = document.createElementNS(ns, 'path');
    s.setAttribute('viewBox', '0 0 24 24'); s.setAttribute('width', '18'); s.setAttribute('height', '18');
    p.setAttribute('d', ICON[name]); p.setAttribute('fill', 'none'); p.setAttribute('stroke', 'currentColor');
    p.setAttribute('stroke-width', '1.8'); p.setAttribute('stroke-linecap', 'round'); p.setAttribute('stroke-linejoin', 'round');
    s.appendChild(p);
    return el('span', { class: 'avatar' }, [s]);
  }
  function initials(s) { return (s.match(/\b\w/g) || ['?']).slice(0, 2).join('').toUpperCase(); }
  // sel builds a <select> from [value, label] pairs with value cur picked.
  function sel(opts, cur, onchange) {
    var s = el('select', { class: 'quiet', onchange: function () { onchange(s.value); } },
      opts.map(function (o) { return el('option', { value: o[0], text: o[1] }); }));
    s.value = String(cur);
    return s;
  }
  function verOpts(cur) {
    var o = [['0', 'Live']];
    for (var v = state.head; v >= 1 && v > state.head - 30; v--) o.push([String(v), 'v' + v]);
    if (cur > 0 && cur <= state.head - 30) o.push([String(cur), 'v' + cur]);
    return o;
  }
  function row(avatar, name, sub, controls) {
    return el('div', { class: 'grant' }, [avatar, el('div', { class: 'who' }, [name, sub && el('div', { class: 'muted small', text: sub })])].concat(controls));
  }
  function renderGrants(gs) {
    gs = gs || [];
    var me = boot.user || {}, people = $('people'), gen = $('general');
    people.textContent = ''; gen.textContent = '';
    people.appendChild(row(el('span', { class: 'avatar me', text: initials(me.name || me.email || '?') }),
      el('div', {}, [me.name || me.email || 'You', el('span', { class: 'muted', text: ' (you)' })]), me.name ? me.email : '',
      [el('span', { class: 'muted small', text: 'Owner' })]));
    gs.forEach(function (g) {
      if (g.audience.indexOf('email:') !== 0) return;
      var email = g.audience.slice(6);
      people.appendChild(row(el('span', { class: 'avatar', text: initials(email.split('@')[0]) }), el('div', { text: email }), '', [
        g.level === 'edit' ? null : sel(verOpts(g.pinned), g.pinned || 0, function (v) { share(email, g.level, +v); }),
        sel([['view', 'Viewer'], ['interact', 'Interactor'], ['edit', 'Editor'], ['none', 'Remove access']], g.level,
          function (l) { share(email, l, l === 'edit' ? 0 : g.pinned || 0); })
      ]));
    });
    // General access is one choice; public wins if an older setup left both grants.
    var by = {}; gs.forEach(function (g) { by[g.audience] = g; });
    var cur = by.public || by.users || null, aud = cur ? cur.audience : '';
    var org = boot.org ? 'Anyone at ' + boot.org : 'Anyone who can sign in';
    var hint = { '': 'Only the people above can open it.', users: 'Anyone who signs in here can open it.', public: 'Anyone with the link, no sign-in needed.' };
    var setAud = function (a) {
      var drop = function (x) { return by[x] && x !== a ? share(x, 'none', 0) : null; };
      var lvl = cur ? cur.level : 'view';
      if (a === 'public' && lvl === 'edit') lvl = 'interact';
      (a ? share(a, lvl, cur && lvl !== 'edit' ? cur.pinned || 0 : 0) : Promise.resolve())
        .then(function () { return drop('users'); }).then(function () { return drop('public'); }).catch(function () { });
    };
    var lvls = [['view', 'Viewer'], ['interact', 'Interactor']].concat(aud === 'users' ? [['edit', 'Editor']] : []);
    gen.appendChild(row(icon(aud === 'public' ? 'globe' : aud ? 'org' : 'lock'),
      sel([['', 'Restricted'], ['users', org], ['public', 'Anyone with the link']], aud, setAud), hint[aud],
      cur ? [
        cur.level === 'edit' ? null : sel(verOpts(cur.pinned), cur.pinned || 0, function (v) { share(aud, cur.level, +v); }),
        sel(lvls, cur.level, function (l) { share(aud, l, l === 'edit' ? 0 : cur.pinned || 0); })
      ] : []));
  }
  function share(who, level, version) {
    $('shareErr').textContent = '';
    return api('POST', '/api/artifacts/' + A.id + '/share', { who: who, level: level, version: version }).then(function (j) { renderGrants(j.grants); })
      .catch(function (e) { $('shareErr').textContent = e.message; throw e; });
  }
  $('shareBtn').addEventListener('click', function () {
    api('GET', '/api/artifacts/' + A.id).then(function (j) { renderGrants(j.grants); });
    $('share').showModal();
  });
  $('shareCopy').addEventListener('click', function () {
    var b = this;
    navigator.clipboard.writeText(A.url).then(function () { b.textContent = 'Copied'; setTimeout(function () { b.textContent = 'Copy link'; }, 1500); });
  });
  $('grantBtn').addEventListener('click', function (e) {
    e.preventDefault();
    var who = $('who').value.trim();
    if (!who) return;
    share(who, $('lvl').value, 0).then(function () { $('who').value = ''; }, function () { });
  });

  // ---------------- live connection ----------------
  var ws = null, wsSeq = 0, wsWait = new Map(), queue = [], backoff = 500;
  function wsSend(m) {
    return new Promise(function (res, rej) {
      m.id = ++wsSeq;
      wsWait.set(m.id, { res: res, rej: rej });
      var s = JSON.stringify(m);
      if (ws && ws.readyState === 1 && state.connected) ws.send(s); else queue.push(s);
    });
  }
  function connect() {
    var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + '/ws/' + A.id);
    ws.onopen = function () { backoff = 500; };
    ws.onmessage = function (ev) {
      var m; try { m = JSON.parse(ev.data); } catch (e) { return; }
      if (m.re) {
        var w = wsWait.get(m.re); if (!w) return; wsWait.delete(m.re);
        if (m.error) w.rej(m.error); else w.res(m.result);
        return;
      }
      switch (m.ev) {
        case 'hello':
          state.connected = true; state.peer = m.peer; state.level = m.level; state.caps = m.caps || []; state.capabilities = m.capabilities || {};
          queue.splice(0).forEach(function (s) { ws.send(s); });
          if (m.head > state.head) onHead(m.head);
          render();
          pollSubs.clear(); clearTimeout(pollT); pollT = 0;
          if (helloInfoSent) toFrame({ t: 'ev', ev: 'conn', connected: true, peer: m.peer });
          else if (pendingHello) answerHello();
          break;
        case 'head':
          if (m.head > state.head) state.updated = Date.now() / 1000;
          state.title = m.title; state.caps = m.caps || state.caps; state.capabilities = m.capabilities || state.capabilities;
          onHead(m.head);
          break;
        case 'access':
          location.reload();
          break;
        case 'deleted':
          frame.hidden = true; $('gone').hidden = false; $('gone').textContent = 'This artifact was deleted.';
          break;
        case 'db.snap':
          deliverSnap(m.sub, m.doc, m.docs, false);
          break;
        case 'room.peers': case 'room.msg':
          toFrame({ t: 'ev', ev: m.ev, room: m.room, peers: m.peers, topic: m.topic, data: m.data, from: m.from });
          break;
      }
    };
    ws.onclose = function () {
      var was = state.connected;
      state.connected = false; render();
      wsWait.forEach(function (w) { w.rej({ code: 'unavailable', message: 'connection lost' }); }); wsWait.clear();
      queue = [];
      if (was) toFrame({ t: 'ev', ev: 'conn', connected: false });
      activeSubs.forEach(function (x, k) { pollSubs.add(k); });
      if (pollSubs.size && !pollT) schedulePoll(POLL_MS);
      setTimeout(connect, backoff); backoff = Math.min(backoff * 2, 15000);
    };
  }
  function onHead(n) {
    if (n > state.head) state.head = n;
    if (state.follow && state.head > state.shown) {
      if (auto) load(state.head); else state.pendingHead = state.head;
    }
    render();
  }

  // ---------------- bridge ----------------
  var helloInfoSent = false, pendingHello = false;
  function toFrame(m) {
    if (!frame.contentWindow) return;
    m.__vitrine = 1;
    try { frame.contentWindow.postMessage(m, '*'); } catch (e) { }
  }
  function helloInfo() {
    return { caps: state.caps, capabilities: state.capabilities, level: state.level, peer: state.peer, connected: state.connected,
      user: boot.user ? { id: boot.user.id, name: boot.user.name, email: boot.user.email } : null, sample: !!(boot.user && boot.sample) };
  }
  function answerHello() { pendingHello = false; helloInfoSent = true; toFrame({ t: 'hello', info: helloInfo() }); }
  var OPS = { 'db.get': 1, 'db.query': 1, 'db.write': 1, 'db.acquire': 1, 'db.sub': 1, 'db.unsub': 1, 'room.join': 1, 'room.leave': 1, 'room.presence': 1, 'room.emit': 1, 'user.profiles': 1, 'user.search': 1 };
  var FIELDS = ['path', 'q', 'w', 'holder', 'ttlMs', 'data', 'room', 'topic', 'patch', 'ids', 'query'];
  var samplesInFlight = new Map(), sampleCache = new Map();

  // ---------------- db: saved snapshots and the HTTP fallback ----------------
  // activeSubs: the page's listeners (key "gen:sub"); pollSubs: those refreshed
  // over HTTP while the socket is down. Confirmed snapshots are saved in this
  // origin's IndexedDB (the sandboxed page has no durable storage) and served
  // first on the next visit, marked fromCache.
  var activeSubs = new Map(), pollSubs = new Set(), pollT = 0, POLL_MS = 30000;
  try { POLL_MS = +localStorage.getItem('vitrine.pollMs') || 30000; } catch (e) { }
  var idbP = null;
  function idb() {
    if (!boot.user || !window.indexedDB) return Promise.reject(new Error('no idb'));
    if (!idbP) idbP = new Promise(function (res, rej) {
      var r = indexedDB.open('vitrine', 1);
      r.onupgradeneeded = function () { r.result.createObjectStore('snaps', { keyPath: 'k' }); };
      r.onsuccess = function () { res(r.result); };
      r.onerror = function () { rej(r.error); };
    });
    return idbP;
  }
  function cacheKey(sig) { return boot.user.id + '|' + A.id + '|' + sig; }
  function cacheGet(sig) {
    return idb().then(function (db) {
      return new Promise(function (res) {
        var q = db.transaction('snaps').objectStore('snaps').get(cacheKey(sig));
        q.onsuccess = function () { var v = q.result; res(v && Date.now() - v.at < 30 * 864e5 ? v : null); };
        q.onerror = function () { res(null); };
      });
    }).catch(function () { return null; });
  }
  function cachePut(sig, doc, docs) {
    var rec = { k: cacheKey(sig), at: Date.now(), doc: doc || null, docs: docs || null };
    try { if (JSON.stringify(rec).length > 1048576) return; } catch (e) { return; }
    idb().then(function (db) { db.transaction('snaps', 'readwrite').objectStore('snaps').put(rec); }).catch(function () { });
  }
  function deliverSnap(key, doc, docs, fromCache) {
    var pend = activeSubs.get(key);
    if (!fromCache && pend && pend.cached) {
      return pend.cached.then(function () { if (pend.cached) { pend.cached = null; } deliverSnap(key, doc, docs, false); });
    }
    var i = key.indexOf(':');
    if (+key.slice(0, i) !== state.gen) return;
    var s = activeSubs.get(key);
    if (s) {
      if (fromCache && s.real) return; // the store already answered
      if (!fromCache) { s.real = true; cachePut(s.sig, doc, docs); }
    }
    toFrame({ t: 'ev', ev: 'db.snap', sub: key.slice(i + 1), doc: doc, docs: docs, fromCache: fromCache });
  }
  function dbHTTP(msg) {
    return fetch('/api/db/' + A.id, { method: 'POST', credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-CSRF': boot.csrf || '' },
      body: JSON.stringify(msg) }).then(function (r) {
      return r.json().then(function (j) { if (j.error) throw j.error; return j.result; }, function () { throw { code: 'unavailable', message: 'HTTP ' + r.status }; });
    }, function () { throw { code: 'unavailable', message: 'the store cannot be reached' }; });
  }
  function pollOne(key) {
    var s = activeSubs.get(key);
    if (!s) return Promise.resolve();
    return dbHTTP(s.q ? { op: 'db.query', q: s.q } : { op: 'db.get', path: s.path }).then(function (r) {
      if (!activeSubs.has(key) || state.connected) return;
      if (s.q) deliverSnap(key, null, (r && r.docs) || [], false); else deliverSnap(key, r, null, false);
    });
  }
  function schedulePoll(ms) { clearTimeout(pollT); pollT = setTimeout(pollTick, ms); }
  function pollTick() {
    pollT = 0;
    if (state.connected || !pollSubs.size) return;
    if (document.visibilityState === 'visible') pollSubs.forEach(function (k) { pollOne(k).catch(function () { }); });
    schedulePoll(POLL_MS);
  }
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible' && !state.connected && pollSubs.size) schedulePoll(0);
  });
  function routeOp(msg) {
    var key = msg.sub;
    if (msg.op === 'db.sub') {
      var sub = { path: msg.path, q: msg.q, sig: JSON.stringify(msg.q || msg.path), real: false };
      activeSubs.set(key, sub);
      // the saved copy (if any) always goes first: live snapshots wait for this lookup
      sub.cached = cacheGet(sub.sig).then(function (v) { if (v) deliverSnap(key, v.doc, v.docs, true); });
    } else if (msg.op === 'db.unsub') {
      activeSubs.delete(key); pollSubs.delete(key);
    }
    if (state.connected || msg.op.indexOf('db.') !== 0) return wsSend(msg);
    // no live socket: db goes over plain HTTP
    if (msg.op === 'db.unsub') return Promise.resolve(true);
    if (msg.op === 'db.sub') {
      pollSubs.add(key);
      if (!pollT) schedulePoll(POLL_MS);
      return pollOne(key).then(function () { return true; }, function (e) {
        if (e && e.code === 'unavailable') return true; // keep retrying quietly, like the live path
        activeSubs.delete(key); pollSubs.delete(key);
        throw e;
      });
    }
    return dbHTTP(msg);
  }

  window.addEventListener('message', function (ev) {
    if (ev.source !== frame.contentWindow) return;
    var m = ev.data;
    if (m && m.__vitrine_md === 1) { if (typeof m.y === 'number') mdPos = m; return; }
    if (m && m.__vitrine_open === 1 && typeof m.url === 'string') { openOut(m.url); return; }
    if (!m || m.__vitrine !== 1 || typeof m.nonce !== 'string') return;
    if (m.t === 'hello') {
      if (state.nonce === m.nonce) { if (helloInfoSent) answerHello(); return; }
      var first = state.nonce === null;
      state.nonce = m.nonce;
      state.gen++;
      samplesInFlight.forEach(function (c) { c.abort(); }); samplesInFlight.clear();
      activeSubs.clear(); pollSubs.clear();
      helloInfoSent = false;
      // don't hold the page hostage to the socket: after 3 s it starts over HTTP
      var reply = function () {
        if (state.connected) return answerHello();
        pendingHello = true;
        setTimeout(function () { if (pendingHello) answerHello(); }, 3000);
      };
      if (first) reply();
      else wsSend({ op: 'frame.reset' }).then(function (r) { if (r && r.peer) state.peer = r.peer; reply(); }, reply);
      return;
    }
    if (m.nonce !== state.nonce) return;
    var gen = state.gen;
    if (m.t === 'op') {
      if (!OPS[m.op]) return toFrame({ t: 're', id: m.id, error: { code: 'invalid_argument', message: 'unknown op' } });
      var msg = { op: m.op };
      FIELDS.forEach(function (k) { if (m[k] !== undefined) msg[k] = m[k]; });
      if (m.sub) msg.sub = gen + ':' + m.sub;
      routeOp(msg).then(function (r) { if (gen === state.gen) toFrame({ t: 're', id: m.id, result: r }); },
        function (e) { if (gen === state.gen) toFrame({ t: 're', id: m.id, error: e }); });
    } else if (m.t === 'sample') {
      runSample(m.id, m.req || {}, gen);
    } else if (m.t === 'sample.abort') {
      var c = samplesInFlight.get(gen + ':' + m.id); if (c) c.abort();
    }
  });

  var consentKey = 'vitrine.consent.' + A.id, consent = null, consentWait = null;
  try { consent = localStorage.getItem(consentKey); } catch (e) { }
  function askConsent() {
    if (consent === 'allow') return Promise.resolve(true);
    if (consent === 'deny') return Promise.resolve(false);
    if (consentWait) return consentWait;
    consentWait = new Promise(function (res) {
      var d = $('consent');
      d.returnValue = '';
      d.onclose = function () {
        consent = d.returnValue === 'allow' ? 'allow' : 'deny';
        try { localStorage.setItem(consentKey, consent); } catch (e) { }
        consentWait = null;
        res(consent === 'allow');
      };
      d.showModal();
    });
    return consentWait;
  }
  function runSample(id, req, gen) {
    var send = function (m) { if (gen === state.gen) { m.id = id; toFrame(m); } };
    var cacheOpt = req.cache, gc = 300000;
    if (cacheOpt && typeof cacheOpt === 'object') gc = Math.min(cacheOpt.gcTime || 300000, 86400000);
    var key = JSON.stringify([req.input, req.modelTier, req.model, req.json]);
    var hit = sampleCache.get(key);
    if (cacheOpt !== false && !(cacheOpt && cacheOpt.refresh) && hit && Date.now() - hit.at < Math.min(gc, hit.gc)) {
      setTimeout(function () { send({ t: 'sample.delta', delta: hit.text }); send({ t: 'sample.done', result: hit.result }); }, 0);
      return;
    }
    askConsent().then(function (ok) {
      if (!ok) return send({ t: 'sample.err', error: { code: 'not_granted', message: 'the viewer did not allow this page to use AI' } });
      var ctl = new AbortController(), text = '';
      samplesInFlight.set(gen + ':' + id, ctl);
      fetch('/api/sample/' + A.id, { method: 'POST', signal: ctl.signal, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF': boot.csrf || '' },
        body: JSON.stringify({ input: req.input, modelTier: req.modelTier, model: req.model, json: req.json }) }).then(function (r) {
        if (!r.ok) return r.json().catch(function () { return {}; }).then(function (j) { send({ t: 'sample.err', error: (j.error && j.error.code) ? j.error : { code: 'upstream_error', message: r.statusText } }); });
        var reader = r.body.getReader(), dec = new TextDecoder(), buf = '';
        function pump() {
          return reader.read().then(function (x) {
            if (x.done) return;
            buf += dec.decode(x.value, { stream: true });
            var parts = buf.split('\n\n'); buf = parts.pop();
            parts.forEach(function (p) {
              var line = p.split('\n').filter(function (l) { return l.indexOf('data:') === 0; }).map(function (l) { return l.slice(5).trim(); }).join('');
              if (!line) return;
              var e; try { e = JSON.parse(line); } catch (er) { return; }
              if (e.delta) { text += e.delta; send({ t: 'sample.delta', delta: e.delta }); }
              else if (e.error) send({ t: 'sample.err', error: e.error });
              else if (e.done) {
                var result = { truncated: e.truncated, modelTierApplied: e.modelTierApplied, modelApplied: e.modelApplied };
                if (cacheOpt !== false) sampleCache.set(key, { text: text, result: result, at: Date.now(), gc: gc });
                send({ t: 'sample.done', result: result });
              }
            });
            return pump();
          });
        }
        return pump();
      }).catch(function (e) {
        if (e && e.name === 'AbortError') return;
        send({ t: 'sample.err', error: { code: 'upstream_error', message: String((e && e.message) || e) } });
      }).finally(function () { samplesInFlight.delete(gen + ':' + id); });
    });
  }

  idb().catch(function () { }); // open early so saved snapshots beat the network
  render();
  load(state.shown);
  connect();
})();
