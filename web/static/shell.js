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
    caps: A.caps || [], capabilities: {}, peer: null, connected: false, gen: 0, nonce: null, pendingHead: 0 };
  var auto = true;
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
    $('title').textContent = state.title;
    document.title = state.title;
    var lv = LV[state.level] || 0;
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
    var b = $('badge');
    if (state.pendingHead && state.follow) { b.hidden = false; b.textContent = 'New version v' + state.pendingHead + ' — refresh'; }
    else if (!state.follow && !state.pinned && state.head > state.shown && lv >= LV.edit) { b.hidden = false; b.textContent = 'Latest is v' + state.head; }
    else b.hidden = true;
    $('conn').className = 'dot' + (state.connected ? ' on' : '');
    $('conn').title = state.connected ? 'Live' : 'Reconnecting…';
  }
  function load(n) {
    state.shown = n;
    state.pendingHead = 0;
    frame.src = '/c/' + A.id + '/' + n;
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
  function audLabel(a) { return a === 'public' ? 'Anyone with the link' : a === 'users' ? 'Everyone who can sign in' : a.replace(/^email:/, ''); }
  function renderGrants(gs) {
    var box = $('grants'); box.textContent = '';
    box.appendChild(el('div', { class: 'grant' }, [el('span', { text: 'You' }), el('span', { class: 'muted', text: 'owner' })]));
    (gs || []).forEach(function (g) {
      box.appendChild(el('div', { class: 'grant' }, [
        el('span', { text: audLabel(g.audience) }),
        el('span', { class: 'muted', text: g.level + (g.pinned ? ' · v' + g.pinned : ' · live') }),
        el('button', { class: 'small', type: 'button', text: 'Remove', onclick: function () { share(g.audience.replace(/^email:/, ''), 'none', 0); } })
      ]));
    });
  }
  function share(who, level, version) {
    $('shareErr').textContent = '';
    return api('POST', '/api/artifacts/' + A.id + '/share', { who: who, level: level, version: version }).then(function (j) { renderGrants(j.grants); })
      .catch(function (e) { $('shareErr').textContent = e.message; throw e; });
  }
  $('shareBtn').addEventListener('click', function () {
    $('shareUrl').textContent = A.url;
    var pin = $('pin'); pin.textContent = '';
    pin.appendChild(el('option', { value: '0', text: 'live' }));
    for (var v = state.head; v >= 1 && v > state.head - 30; v--) pin.appendChild(el('option', { value: String(v), text: 'pinned v' + v }));
    api('GET', '/api/artifacts/' + A.id).then(function (j) { renderGrants(j.grants); });
    $('share').showModal();
  });
  $('grantBtn').addEventListener('click', function (e) {
    e.preventDefault();
    var who = $('who').value.trim();
    if (!who) return;
    share(who, $('lvl').value, +$('pin').value).then(function () { $('who').value = ''; }, function () { });
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
      activeSubs.set(key, { path: msg.path, q: msg.q, sig: JSON.stringify(msg.q || msg.path), real: false });
      var sig = activeSubs.get(key).sig;
      cacheGet(sig).then(function (v) { if (v) deliverSnap(key, v.doc, v.docs, true); });
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
