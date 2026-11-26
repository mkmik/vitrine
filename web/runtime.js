/* vitrine page runtime: window.claude.use(name) with db, room, sample and user,
   API-compatible with Claude artifacts. Talks only to the parent shell over
   postMessage; holds no credentials. */
(function () {
  'use strict';
  var CFG = window.__vitrine_cfg || { mode: 'raw' };
  try { delete window.__vitrine_cfg; } catch (e) { window.__vitrine_cfg = undefined; }
  var parentWin = window.parent;
  var framed = CFG.mode === 'frame' && parentWin && parentWin !== window;
  var nonce = (window.crypto && crypto.randomUUID) ? crypto.randomUUID() : String(Math.random()).slice(2) + Date.now();
  var seq = 0;
  var pending = new Map();      // op id -> {res, rej}
  var samples = new Map();      // sample id -> handler
  var info = null;              // hello info from the shell
  var helloWaiters = [];
  var connected = false;
  var connListeners = new Set();
  var LV = { none: 0, view: 1, interact: 2, edit: 3, admin: 3, owner: 4, self: 5 };

  function err(code, message, extra) {
    var e = { code: code, message: message || code };
    if (extra) for (var k in extra) e[k] = extra[k];
    return e;
  }
  function post(m) {
    m.__vitrine = 1; m.nonce = nonce;
    try { parentWin.postMessage(m, '*'); } catch (e) { }
  }
  function op(name, payload) {
    return new Promise(function (res, rej) {
      if (!framed) return rej(err('not_granted', 'no viewer'));
      var id = ++seq;
      pending.set(id, { res: res, rej: rej });
      var m = { t: 'op', id: id, op: name };
      for (var k in payload) m[k] = payload[k];
      post(m);
    });
  }
  function deepFreeze(o) {
    if (o && typeof o === 'object' && !Object.isFrozen(o)) {
      Object.freeze(o);
      for (var k in o) deepFreeze(o[k]);
    }
    return o;
  }
  function clone(v) { return v === undefined ? undefined : JSON.parse(JSON.stringify(v)); }
  function isPlain(o) { return o !== null && typeof o === 'object' && Object.getPrototypeOf(o) === Object.prototype; }
  function later(fn) { Promise.resolve().then(fn); }
  function report(e) { try { (window.reportError || console.error)(e); } catch (x) { console.error(e); } }

  // ---------------- messages from the shell ----------------
  var onRoomEvent = null, onDbSnap = null;
  window.addEventListener('message', function (ev) {
    if (ev.source !== parentWin || !framed) return;
    var m = ev.data;
    if (!m || m.__vitrine !== 1) return;
    switch (m.t) {
      case 'hello':
        info = m.info;
        connected = !!m.info.connected;
        helloWaiters.splice(0).forEach(function (f) { f(info); });
        break;
      case 're':
        var p = pending.get(m.id);
        if (!p) return;
        pending.delete(m.id);
        if (m.error) p.rej(err(m.error.code || 'unavailable', m.error.message)); else p.res(m.result);
        break;
      case 'ev':
        if (m.ev === 'conn') {
          var was = connected;
          connected = !!m.connected;
          if (m.peer && info) info.peer = m.peer;
          if (was !== connected) connListeners.forEach(function (f) { try { f(connected, !was && connected); } catch (e) { report(e); } });
        } else if (m.ev === 'db.snap') { if (onDbSnap) onDbSnap(m); }
        else if (m.ev === 'room.peers' || m.ev === 'room.msg') { if (onRoomEvent) onRoomEvent(m); }
        break;
      case 'sample.delta': case 'sample.done': case 'sample.err':
        var h = samples.get(m.id);
        if (h) h(m);
        break;
    }
  });

  function waitHello(ms) {
    if (info) return Promise.resolve(info);
    if (!framed) return Promise.resolve(null);
    return new Promise(function (res) {
      var t = setTimeout(function () { res(null); }, ms);
      helloWaiters.push(function (i) { clearTimeout(t); res(i); });
    });
  }
  if (framed) {
    post({ t: 'hello' });
    var tries = 0, iv = setInterval(function () { if (info || ++tries > 20) return clearInterval(iv); post({ t: 'hello' }); }, 500);
    // the viewer remembers where the page is scrolled (for "use current view" thumbnails)
    var scrollT = 0;
    window.addEventListener('scroll', function () {
      if (scrollT) return;
      scrollT = setTimeout(function () { scrollT = 0; try { parentWin.postMessage({ __vitrine_md: 1, y: Math.round(window.scrollY) }, '*'); } catch (e) { } }, 150);
    }, { passive: true });
  }
  if (CFG.y > 0) { // opened scrolled down: a thumbnail; again on load, after late layout
    var goY = function () { window.scrollTo(0, CFG.y); };
    document.addEventListener('DOMContentLoaded', goY); window.addEventListener('load', goY); setTimeout(goY, 800);
  }

  function capDecl(name) { return info && info.capabilities ? info.capabilities[name] : undefined; }
  function hasCap(name) { return !!(info && info.caps && info.caps.indexOf(name) >= 0); }
  function level() { return info ? (LV[info.level] || 0) : 0; }

  // ================= db =================
  var SEG = /^[A-Za-z0-9_\-.~:@+]+$/;
  function checkPath(p, wantDoc, what) {
    if (typeof p !== 'string') throw new TypeError(what + ': path must be a string, got ' + (p && typeof p.then === 'function' ? 'a Promise (await it first)' : typeof p));
    if (p.length > 1000) throw new TypeError(what + ': path over 1000 bytes');
    var segs = p.split('/');
    if (segs.length > 16) throw new TypeError(what + ': path over 16 segments');
    for (var i = 0; i < segs.length; i++) {
      var s = segs[i];
      if (!s) throw new TypeError(what + ': empty segment in "' + p + '"');
      if (s === '.' || s === '..') throw new TypeError(what + ': segment "' + s + '" not allowed');
      if (s.length > 200) throw new TypeError(what + ': segment over 200 bytes');
      if (!SEG.test(s)) throw new TypeError(what + ': segment "' + s + '" has characters outside letters, digits and _ - . ~ : @ +');
    }
    if (wantDoc && segs.length % 2) throw new TypeError(what + ': a document path needs an even number of segments; "' + p + '" has ' + segs.length);
    if (!wantDoc && segs.length % 2 === 0) throw new TypeError(what + ': a collection path needs an odd number of segments; "' + p + '" has ' + segs.length);
    return p;
  }
  function autoId() {
    var c = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789', s = '';
    var b = new Uint8Array(20); crypto.getRandomValues(b);
    for (var i = 0; i < 20; i++) s += c[b[i] % 62];
    return s;
  }
  function typeRank(v) {
    if (v === null || v === undefined) return 0;
    if (typeof v === 'boolean') return 1;
    if (typeof v === 'number') return 2;
    if (typeof v === 'string') return 3;
    if (Array.isArray(v)) return 4;
    return 5;
  }
  function cmp(a, b) {
    var ra = typeRank(a), rb = typeRank(b);
    if (ra !== rb) return ra - rb;
    if (ra === 0) return 0;
    if (ra <= 3) return a === b ? 0 : (a < b ? -1 : 1);
    var ja = JSON.stringify(a), jb = JSON.stringify(b);
    return ja === jb ? 0 : (ja < jb ? -1 : 1);
  }
  function matches(data, where) {
    for (var i = 0; i < where.length; i++) {
      var f = where[i], has = Object.prototype.hasOwnProperty.call(data, f.field), v = data[f.field];
      switch (f.op) {
        case '==': if (!has || cmp(v, f.value) !== 0) return false; break;
        case '!=': if (!has || cmp(v, f.value) === 0) return false; break;
        case '<': case '<=': case '>': case '>=':
          if (!has || typeRank(v) !== typeRank(f.value)) return false;
          var c = cmp(v, f.value);
          if ((f.op === '<' && c >= 0) || (f.op === '<=' && c > 0) || (f.op === '>' && c <= 0) || (f.op === '>=' && c < 0)) return false;
          break;
        case 'in': case 'not-in':
          var found = has && f.value.some(function (x) { return cmp(v, x) === 0; });
          if (!has || (f.op === 'in') !== found) return false; break;
        case 'array-contains':
          if (!Array.isArray(v) || !v.some(function (x) { return cmp(x, f.value) === 0; })) return false; break;
      }
    }
    return true;
  }
  function deepMerge(dst, src) {
    var out = {}, k;
    for (k in dst) out[k] = dst[k];
    for (k in src) {
      if (isPlain(src[k]) && isPlain(out[k])) out[k] = deepMerge(out[k], src[k]);
      else out[k] = src[k];
    }
    return out;
  }
  function lastSeg(path) { return path.slice(path.lastIndexOf('/') + 1); }
  function parentOf(path) { return path.slice(0, path.lastIndexOf('/')); }

  var dbSubs = new Map();    // sub id -> sub
  var overlays = new Map();  // path -> local unconfirmed write {seq, exists, data, confirmedVersion}
  var knownDocs = new Map(); // path -> last server doc
  var subSeq = 0, writeSeq = 0;

  function snapOf(path, exists, data, version, pendingW, cache, fromCache) {
    var key = path + '|' + exists + '|' + version + '|' + pendingW + '|' + !!fromCache + '|' + (pendingW ? JSON.stringify(data) : '');
    if (cache && cache.has(key)) return cache.get(key);
    var frozen = exists ? deepFreeze(clone(data)) : undefined;
    var s = Object.freeze({
      id: lastSeg(path), exists: !!exists, version: version || 0,
      data: function () { return frozen; },
      metadata: Object.freeze({ fromCache: !!fromCache, hasPendingWrites: !!pendingW })
    });
    if (cache) { if (cache.size > 2000) cache.clear(); cache.set(key, s); }
    return s;
  }
  // the server doc with any local unconfirmed write on top
  function viewDoc(d) {
    var o = overlays.get(d.path);
    if (o && o.confirmedVersion && d.version >= o.confirmedVersion) { overlays.delete(d.path); o = null; }
    if (!o) return { path: d.path, exists: d.exists, data: d.data, version: d.version, pending: false };
    return { path: d.path, exists: o.exists, data: o.data, version: d.version, pending: true };
  }
  // fromCache: true = not confirmed by the store (a copy saved on an earlier
  // visit, or our own writes before the first read lands)
  function emitSub(sub) {
    if (sub.dead) return;
    var server = sub.server, fc = !!sub.fromCache;
    if (!server) {
      var mine = false;
      overlays.forEach(function (o, p) { if (sub.kind === 'doc' ? p === sub.path : parentOf(p) === sub.q.coll) mine = true; });
      if (!mine) return;
      server = sub.kind === 'doc' ? { path: sub.path, exists: false, version: 0 } : [];
      fc = true;
    }
    if (sub.kind === 'doc') {
      var v = viewDoc(server);
      var s = snapOf(v.path, v.exists, v.data, v.version, v.pending, sub.cache, fc);
      if (s === sub.lastSnap) return;
      sub.lastSnap = s;
      try { sub.next(s); } catch (e) { report(e); }
      return;
    }
    var byPath = new Map();
    server.forEach(function (d) { byPath.set(d.path, d); });
    overlays.forEach(function (o, path) {
      if (parentOf(path) === sub.q.coll && !byPath.has(path)) byPath.set(path, { path: path, exists: false, version: 0 });
    });
    var list = [], anyPending = false;
    byPath.forEach(function (d) {
      var v = viewDoc(d);
      if (!v.exists || !matches(v.data || {}, sub.q.where)) return;
      if (v.pending) anyPending = true;
      list.push(v);
    });
    if (sub.q.orderBy) {
      var f = sub.q.orderBy, desc = sub.q.dir === 'desc';
      list.sort(function (a, b) {
        var ha = a.data && Object.prototype.hasOwnProperty.call(a.data, f), hb = b.data && Object.prototype.hasOwnProperty.call(b.data, f);
        if (!ha || !hb) return ha === hb ? 0 : (ha ? -1 : 1);
        var c = cmp(a.data[f], b.data[f]); return desc ? -c : c;
      });
    } else list.sort(function (a, b) { var ia = lastSeg(a.path), ib = lastSeg(b.path); return ia < ib ? -1 : ia > ib ? 1 : 0; });
    if (sub.q.limit) list = list.slice(0, sub.q.limit);
    var docs = list.map(function (v) { return snapOf(v.path, true, v.data, v.version, v.pending, sub.cache); });
    var prev = sub.lastDocs || [];
    if (sub.lastDocs && sub.lastFC === fc && prev.length === docs.length && prev.every(function (d, i) { return d === docs[i]; })) return;
    var prevIdx = new Map(); prev.forEach(function (d, i) { prevIdx.set(d.id, i); });
    var nextIdx = new Map(); docs.forEach(function (d, i) { nextIdx.set(d.id, i); });
    var changes = [];
    prev.forEach(function (d, i) { if (!nextIdx.has(d.id)) changes.push(Object.freeze({ type: 'removed', doc: d, oldIndex: i, newIndex: -1 })); });
    docs.forEach(function (d, i) {
      if (!prevIdx.has(d.id)) changes.push(Object.freeze({ type: 'added', doc: d, oldIndex: -1, newIndex: i }));
      else if (prev[prevIdx.get(d.id)] !== d) changes.push(Object.freeze({ type: 'modified', doc: d, oldIndex: prevIdx.get(d.id), newIndex: i }));
    });
    sub.lastDocs = docs;
    sub.lastFC = fc;
    var frozenDocs = Object.freeze(docs.slice()), frozenChanges = Object.freeze(changes);
    var qs = Object.freeze({ docs: frozenDocs, size: docs.length, empty: docs.length === 0,
      docChanges: function () { return frozenChanges; },
      metadata: Object.freeze({ fromCache: fc, hasPendingWrites: anyPending }) });
    try { sub.next(qs); } catch (e) { report(e); }
  }
  onDbSnap = function (m) {
    var sub = dbSubs.get(m.sub);
    if (!sub) return;
    if (m.fromCache && sub.server && !sub.fromCache) return; // never replace a confirmed snapshot with a saved copy
    sub.fromCache = !!m.fromCache;
    if (m.doc) { knownDocs.set(m.doc.path, m.doc); sub.server = m.doc; }
    else { (m.docs || []).forEach(function (d) { knownDocs.set(d.path, d); }); sub.server = m.docs || []; }
    emitSub(sub);
  };
  function affected(path) {
    var coll = parentOf(path);
    dbSubs.forEach(function (sub) {
      if ((sub.kind === 'doc' && sub.path === path) || (sub.kind === 'query' && sub.q.coll === coll)) emitSub(sub);
    });
  }
  function currentData(path) {
    var o = overlays.get(path);
    if (o) return o.exists ? o.data : undefined;
    var d = knownDocs.get(path);
    return d && d.exists ? d.data : undefined;
  }
  function doWrite(w) {
    var s = ++writeSeq, path = w.path;
    var prevOverlay = overlays.get(path);
    if (w.op === 'set') overlays.set(path, { seq: s, exists: true, data: clone(w.data) });
    else if (w.op === 'update') { var cur = currentData(path); if (cur) overlays.set(path, { seq: s, exists: true, data: deepMerge(cur, clone(w.data)) }); }
    else if (w.op === 'delete') overlays.set(path, { seq: s, exists: false, data: undefined });
    affected(path);
    return op('db.write', { w: w }).then(function (res) {
      var o = overlays.get(path);
      if (o && o.seq === s) {
        if (w.op === 'delete') { overlays.delete(path); knownDocs.set(path, { path: path, exists: false, version: 0 }); }
        else {
          o.confirmedVersion = (res && res.version) || 1;
          setTimeout(function () { var o2 = overlays.get(path); if (o2 && o2.seq === s) { overlays.delete(path); affected(path); } }, 3000);
        }
      }
      affected(path);
    }, function (e) {
      var o = overlays.get(path);
      if (o && o.seq === s) { if (prevOverlay) overlays.set(path, prevOverlay); else overlays.delete(path); affected(path); }
      throw e;
    });
  }
  function subscribe(kind, path, q, next, error) {
    if (typeof next !== 'function') throw new TypeError('onSnapshot needs a callback');
    var id = 's' + (++subSeq);
    var sub = { id: id, kind: kind, path: path, q: q, next: next, error: error, cache: new Map(), dead: false };
    dbSubs.set(id, sub);
    function start() {
      var payload = { sub: id };
      if (q) payload.q = q; else payload.path = path;
      op('db.sub', payload).catch(function (e) {
        if (sub.dead || e.code === 'unavailable') return; // reconnect restarts it
        sub.dead = true; dbSubs.delete(id);
        if (error) { try { error(e); } catch (x) { report(x); } } else report(new Error('db listener: ' + e.code + ': ' + e.message));
      });
    }
    sub.restart = start;
    later(start);
    return function () {
      if (sub.dead) return;
      sub.dead = true; dbSubs.delete(id);
      op('db.unsub', { sub: id }).catch(function () { });
    };
  }
  connListeners.add(function (c, reconnected) {
    if (reconnected) dbSubs.forEach(function (sub) { sub.restart(); });
  });

  function makeQuery(coll, where, orderBy, dir, limit) {
    var q = { coll: coll, where: where, orderBy: orderBy || '', dir: dir || '', limit: limit || 0 };
    return {
      where: function (field, opn, value) {
        if (where.length >= 10) throw new TypeError('at most 10 filters');
        if (['==', '!=', '<', '<=', '>', '>=', 'in', 'not-in', 'array-contains'].indexOf(opn) < 0) throw new TypeError('unknown operator ' + opn);
        if ((opn === 'in' || opn === 'not-in') && (!Array.isArray(value) || value.length > 30)) throw new TypeError(opn + ' needs an array of at most 30 values');
        return makeQuery(coll, where.concat([{ field: String(field), op: opn, value: clone(value) }]), orderBy, dir, limit);
      },
      orderBy: function (field, d) { if (orderBy) throw new TypeError('at most one orderBy'); return makeQuery(coll, where, String(field), d === 'desc' ? 'desc' : 'asc', limit); },
      limit: function (n) { n = Math.floor(n); if (!(n >= 1 && n <= 1000)) throw new TypeError('limit must be 1-1000'); return makeQuery(coll, where, orderBy, dir, n); },
      get: function () {
        return op('db.query', { q: q }).then(function (r) {
          var out, fake = { kind: 'query', q: q, server: r.docs || [], cache: new Map(), dead: false, next: function (s) { out = s; } };
          emitSub(fake);
          return out;
        });
      },
      onSnapshot: function (next, error) { return subscribe('query', null, q, next, error); }
    };
  }
  function collRef(path) {
    checkPath(path, false, 'collection()');
    var q = makeQuery(path, [], '', '', 0);
    return Object.freeze({
      path: path, id: lastSeg(path),
      where: q.where, orderBy: q.orderBy, limit: q.limit, get: q.get, onSnapshot: q.onSnapshot,
      doc: function (id) { return docRef(path + '/' + (id === undefined ? autoId() : id)); },
      add: function (data) { var r = docRef(path + '/' + autoId()); return r.set(data).then(function () { return r; }); }
    });
  }
  function checkBody(data, what) {
    if (!isPlain(data)) return Promise.reject(err('invalid_argument', what + ': the document body must be a plain object'));
    try { JSON.stringify(data); } catch (e) { return Promise.reject(err('invalid_argument', 'not JSON: ' + e.message)); }
    return null;
  }
  function docRef(path) {
    checkPath(path, true, 'doc()');
    return Object.freeze({
      id: lastSeg(path), path: path,
      get: function () { return op('db.get', { path: path }).then(function (d) { knownDocs.set(path, d); var v = viewDoc(d); return snapOf(path, v.exists, v.data, v.version, v.pending, null); }); },
      set: function (data, opts) {
        var bad = checkBody(data, 'set'); if (bad) return bad;
        var w = { op: 'set', path: path, data: clone(data) };
        if (opts && typeof opts.ifVersion === 'number') w.ifVersion = opts.ifVersion;
        return doWrite(w).then(function () { });
      },
      update: function (data) { var bad = checkBody(data, 'update'); if (bad) return bad; return doWrite({ op: 'update', path: path, data: clone(data) }).then(function () { }); },
      delete: function () { return doWrite({ op: 'delete', path: path }).then(function () { }); },
      acquire: function (o) {
        if (!o || typeof o.holder !== 'string' || !o.holder) return Promise.reject(err('invalid_argument', 'acquire needs {holder}'));
        return op('db.acquire', { path: path, holder: o.holder, ttlMs: o.ttlMs || 0, data: o.data || null });
      },
      onSnapshot: function (next, error) { return subscribe('doc', path, null, next, error); },
      collection: function (sub) { return collRef(path + '/' + sub); }
    });
  }
  var dbNS = null;
  function makeDB() {
    if (!dbNS) dbNS = Object.freeze({ doc: function (p) { return docRef(p); }, collection: function (p) { return collRef(p); } });
    return dbNS;
  }

  // ================= room =================
  var TOPIC = /^[a-z][a-z0-9_.-]{0,47}$/, ROOMNAME = /^[a-z0-9][a-z0-9_.-]{0,47}$/;
  var rooms = new Map(); // name ('' = lobby) -> state
  function jsonSize(v) { try { return new TextEncoder().encode(JSON.stringify(v)).length; } catch (e) { return Infinity; } }
  function roomState(name) {
    var st = rooms.get(name);
    if (st) return st;
    st = { name: name, joined: false, left: false, peers: Object.freeze([]), byPeer: new Map(), topicL: new Map(), peersL: new Set(),
      myPresence: {}, patch: null, flushT: 0, delivered: null, raf: 0, serverPeers: null };
    rooms.set(name, st);
    return st;
  }
  function buildPeers(st) {
    var out = [], seen = new Map();
    (st.serverPeers || []).forEach(function (p) {
      var pres = p.sameTab ? st.myPresence : (p.presence || {});
      var pj = JSON.stringify(pres);
      var key = JSON.stringify([p.by, p.isMe, p.sameTab, p.kind, p.guest]) + pj;
      var old = st.byPeer.get(p.peer), obj;
      if (old && old.key === key) obj = old.obj;
      else obj = Object.freeze({ peer: p.peer, by: p.by == null ? null : p.by, isMe: !!p.isMe, sameTab: !!p.sameTab, kind: p.kind || 'viewer', guest: !!p.guest,
        presence: deepFreeze(clone(pres)), updatedAt: (old && old.pj === pj) ? old.obj.updatedAt : Date.now() });
      seen.set(p.peer, { key: key, pj: pj, obj: obj });
      out.push(obj);
    });
    st.byPeer = seen;
    st.peers = Object.freeze(out);
    scheduleDeliver(st);
  }
  function scheduleDeliver(st) {
    if (st.raf) return;
    var run = function () {
      st.raf = 0;
      var prev = st.delivered, now = st.peers;
      if (prev === now) return;
      var prevMap = new Map(); (prev || []).forEach(function (p) { prevMap.set(p.peer, p); });
      var nowMap = new Map(); now.forEach(function (p) { nowMap.set(p.peer, p); });
      var joined = [], left = [], updated = [];
      now.forEach(function (p) { var o = prevMap.get(p.peer); if (!o) joined.push(p); else if (o !== p) updated.push(p); });
      (prev || []).forEach(function (p) { if (!nowMap.has(p.peer)) left.push(p); });
      st.delivered = now;
      if (!joined.length && !left.length && !updated.length) return;
      var ch = Object.freeze({ peers: now, joined: Object.freeze(joined), left: Object.freeze(left), updated: Object.freeze(updated) });
      st.peersL.forEach(function (l) { try { l.fn(ch); } catch (e) { report(e); } });
    };
    st.raf = (typeof requestAnimationFrame === 'function' && document.visibilityState !== 'hidden') ? requestAnimationFrame(run) : setTimeout(run, 16);
  }
  onRoomEvent = function (m) {
    var st = rooms.get(m.room || '');
    if (!st || st.left) return;
    if (m.ev === 'room.peers') { st.serverPeers = m.peers; buildPeers(st); return; }
    var from = m.from || {};
    var msg = Object.freeze({ topic: m.topic, data: m.data === undefined ? undefined : deepFreeze(m.data), peer: from.peer, by: from.by == null ? null : from.by,
      isMe: !!from.isMe, sameTab: !!from.sameTab, kind: from.kind || 'viewer', guest: !!from.guest });
    var ls = st.topicL.get(m.topic);
    if (ls) ls.forEach(function (l) { try { l.fn(msg); } catch (e) { report(e); } });
  };
  function flushPresence(st) {
    st.flushT = 0;
    if (!st.patch || !st.joined) return;
    var p = st.patch; st.patch = null;
    op('room.presence', { room: st.name, patch: p }).catch(function () { });
  }
  function join(st) {
    st.joined = false;
    return op('room.join', { room: st.name }).then(function () {
      st.joined = true;
      if (Object.keys(st.myPresence).length) op('room.presence', { room: st.name, patch: st.myPresence }).catch(function () { });
    });
  }
  connListeners.add(function (c, reconnected) {
    if (!c) rooms.forEach(function (st) { st.joined = false; });
    if (reconnected) rooms.forEach(function (st) { if (!st.left) join(st).catch(function () { }); });
  });
  function roomAPI(st, isLobby) {
    var api = {
      emit: function (topic, data) {
        if (typeof topic !== 'string' || !TOPIC.test(topic)) return Promise.reject(err('invalid_argument', 'topic must match ^[a-z][a-z0-9_.-]{0,47}$'));
        if (data !== undefined && jsonSize(data) > 4096) return Promise.reject(err('invalid_argument', 'data over 4 KiB of JSON'));
        if (st.left) return Promise.reject(err('invalid_argument', 'you left this room'));
        if (!connected || !st.joined) return Promise.resolve();
        return op('room.emit', { room: st.name, topic: topic, data: data === undefined ? null : clone(data) }).then(function () { });
      },
      on: function (topic, fn, onError) {
        if (typeof fn !== 'function') throw new TypeError('handler must be a function');
        if (typeof topic !== 'string' || !TOPIC.test(topic)) { later(function () { if (onError) onError(err('invalid_argument', 'bad topic')); }); return function () { }; }
        var l = { fn: fn };
        if (!st.topicL.has(topic)) st.topicL.set(topic, new Set());
        st.topicL.get(topic).add(l);
        return function () { var s = st.topicL.get(topic); if (s) s.delete(l); };
      },
      presence: function (patch) {
        if (!isPlain(patch)) return Promise.reject(err('invalid_argument', 'presence takes a plain object'));
        var next = {}, k;
        for (k in st.myPresence) next[k] = st.myPresence[k];
        for (k in patch) { if (patch[k] === null) delete next[k]; else next[k] = clone(patch[k]); }
        if (jsonSize(next) > 4096) return Promise.reject(err('invalid_argument', 'presence over 4 KiB of JSON'));
        st.myPresence = next;
        st.patch = st.patch || {};
        for (k in patch) st.patch[k] = patch[k] === null ? null : clone(patch[k]);
        if (st.serverPeers) buildPeers(st);
        if (!st.flushT) st.flushT = setTimeout(function () { flushPresence(st); }, 33);
        return Promise.resolve();
      },
      peers: function () { return st.peers; },
      onPeers: function (fn) {
        if (typeof fn !== 'function') throw new TypeError('handler must be a function');
        var l = { fn: fn };
        st.peersL.add(l);
        later(function () {
          if (!st.peersL.has(l)) return;
          try { fn(Object.freeze({ peers: st.peers, joined: st.peers, left: Object.freeze([]), updated: Object.freeze([]) })); } catch (e) { report(e); }
        });
        return function () { st.peersL.delete(l); };
      },
      connected: function () { return connected && st.joined; },
      onConnection: function (fn) {
        if (typeof fn !== 'function') throw new TypeError('handler must be a function');
        var l = function (c) { try { fn(c); } catch (e) { report(e); } };
        connListeners.add(l);
        later(function () { if (connListeners.has(l)) l(connected); });
        return function () { connListeners.delete(l); };
      }
    };
    if (isLobby) {
      api.join = function (name) {
        if (typeof name !== 'string' || !ROOMNAME.test(name)) return Promise.reject(err('invalid_argument', 'room name must match ^[a-z0-9][a-z0-9_.-]{0,47}$'));
        var ns = rooms.get(name);
        if (ns && !ns.left && ns.api) return Promise.resolve(ns.api);
        if (ns) rooms.delete(name);
        var nst = roomState(name);
        return join(nst).then(function () { nst.api = Object.freeze(roomAPI(nst, false)); return nst.api; }, function (e) { rooms.delete(name); throw e; });
      };
      api.sendToClaudeSession = function () { return Promise.reject(err('claude_unavailable', 'no Claude session beside this page')); };
      api.canSendToClaudeSession = function () { return Promise.resolve('off'); };
    } else {
      api.name = st.name;
      api.leave = function () {
        if (st.left) return Promise.resolve();
        st.left = true; st.topicL.clear(); st.peersL.clear();
        rooms.delete(st.name);
        return op('room.leave', { room: st.name }).then(function () { }, function () { });
      };
    }
    return api;
  }
  var lobby = null;
  function makeRoom() {
    if (lobby) return lobby;
    var st = roomState('');
    lobby = Object.freeze(roomAPI(st, true));
    join(st).catch(function () { });
    return lobby;
  }

  // ================= user =================
  var PALETTE = ['#e5484d', '#d6409f', '#8e4ec6', '#6e56cf', '#3e63dd', '#0090ff', '#00a2c7', '#12a594', '#30a46c', '#46a758', '#e2a336', '#f76b15', '#a18072'];
  function colorOf(id) { var h = 0; id = String(id || ''); for (var i = 0; i < id.length; i++) h = (h * 31 + id.charCodeAt(i)) | 0; return PALETTE[Math.abs(h) % PALETTE.length]; }
  function avatarOf(name, color) {
    var ini = (name || '').trim().split(/[\s._@-]+/).filter(Boolean).slice(0, 2).map(function (s) { return s[0].toUpperCase(); }).join('') || '?';
    var svg = '<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" rx="32" fill="' + color + '"/><text x="32" y="41" font-family="system-ui,sans-serif" font-size="26" font-weight="600" fill="#fff" text-anchor="middle">' +
      ini.replace(/[<>&"']/g, '') + '</text></svg>';
    return 'data:image/svg+xml;base64,' + btoa(unescape(encodeURIComponent(svg)));
  }
  var profileCache = new Map();
  function makeUser() {
    var d = capDecl('user');
    var declared = hasCap('user');
    var scopes = (d && d.scopes) || [];
    var prof = scopes.indexOf('profile') >= 0, mail = scopes.indexOf('email') >= 0;
    var u = info && info.user;
    var lv = level();
    function rootWriteLevel() {
      var dbd = capDecl('db'), w = LV.interact;
      if (dbd && dbd.rules) dbd.rules.forEach(function (r) { if (r.path === '' && r.write) w = LV[r.write] || w; });
      return w;
    }
    function me() {
      var color = colorOf(u ? u.id : 'anon');
      var name = prof && u ? u.name : '';
      return Object.freeze({ id: declared && u ? u.id : null, name: name, avatarUrl: avatarOf(name || (u && u.name) || '', color), color: color,
        email: mail && u ? u.email : null, isOwner: lv >= LV.owner, canEdit: lv >= LV.edit });
    }
    function profileOf(p) {
      var color = colorOf(p.id);
      return Object.freeze({ id: p.id, name: p.name || '', avatarUrl: avatarOf(p.name || '', color), color: color,
        email: mail ? (p.email || null) : null, isMe: !!(u && declared && p.id === u.id), guest: false });
    }
    return Object.freeze({
      isOwner: function () { return Promise.resolve(lv >= LV.owner); },
      canEdit: function () { return Promise.resolve(lv >= LV.edit); },
      can: function (name) {
        if (!u) return Promise.resolve(false);
        if (name === 'data.write') return Promise.resolve(hasCap('db') && lv >= rootWriteLevel());
        if (name === 'files.write') return Promise.resolve(lv >= LV.edit);
        return Promise.resolve(false);
      },
      me: function () { return Promise.resolve(me()); },
      id: function () { return Promise.resolve(me().id); },
      name: function () { return Promise.resolve(me().name); },
      email: function () { return Promise.resolve(me().email); },
      avatarUrl: function () { return Promise.resolve(u ? me().avatarUrl : null); },
      profiles: function (ids) {
        if (typeof ids === 'string') ids = [ids];
        ids = Array.from(new Set((ids || []).map(String)));
        var missing = ids.filter(function (id) { return !profileCache.has(id); });
        var p = (missing.length && u && declared) ? op('user.profiles', { ids: missing }).then(function (r) {
          for (var k in r) profileCache.set(k, profileOf(r[k]));
        }, function () { }) : Promise.resolve();
        return p.then(function () {
          var out = {};
          ids.forEach(function (id) { out[id] = profileCache.get(id) || profileOf({ id: id, name: '' }); });
          return out;
        });
      },
      search: function (q) {
        if (!u || !prof) return Promise.resolve([]);
        return op('user.search', { query: String(q || '') }).then(function (r) {
          return (r || []).map(function (p) { var pr = profileOf(p); profileCache.set(p.id, pr); return pr; });
        }, function () { return []; });
      }
    });
  }

  // ================= sample =================
  var sampleSeq = 0;
  function parseJSONReply(text) {
    try { return { ok: true, v: JSON.parse(text) }; } catch (e) { }
    var m = /```(?:json)?\s*([\s\S]*?)```/.exec(text);
    if (m) { try { return { ok: true, v: JSON.parse(m[1]) }; } catch (e) { } }
    var a = text.search(/[\[{]/), b = Math.max(text.lastIndexOf('}'), text.lastIndexOf(']'));
    if (a >= 0 && b > a) { try { return { ok: true, v: JSON.parse(text.slice(a, b + 1)) }; } catch (e) { } }
    return { ok: false };
  }
  function runSample(input, opts, asJSON) {
    return new Promise(function (resolve, reject) {
      function bad(code, msg) { later(function () { reject(err(code, msg)); }); }
      if (opts === undefined) opts = {};
      if (!isPlain(opts)) return bad('invalid_request', 'options must be a plain object');
      if (typeof input === 'string') { if (!input.trim()) return bad('invalid_request', 'input is empty'); }
      else if (Array.isArray(input)) {
        if (!input.length || !input[0] || input[0].role !== 'user' || input[input.length - 1].role !== 'user') return bad('invalid_request', 'turns must start and end with a user turn');
        for (var i = 0; i < input.length; i++) if (!input[i] || (input[i].role !== 'user' && input[i].role !== 'assistant') || typeof input[i].content !== 'string' || !input[i].content) return bad('invalid_request', 'each turn needs role user|assistant and non-empty content');
        input = input.map(function (t) { return { role: t.role, content: t.content }; });
      } else return bad('invalid_request', 'input must be a string or a list of turns (the prompt goes first)');
      if (opts.tools) return bad('tools_unavailable', 'page tools are not supported on this server');
      if (opts.images) return bad('images_unavailable', 'images are not supported on this server');
      if (opts.onText !== undefined && typeof opts.onText !== 'function') return bad('invalid_request', 'onText must be a function');
      if (opts.signal !== undefined && !(opts.signal instanceof AbortSignal)) return bad('invalid_request', 'signal must be an AbortSignal (pass ctl.signal)');
      if (opts.modelTier && ['default', 'complex', 'quick', 'inherit'].indexOf(opts.modelTier) < 0) return bad('invalid_request', 'unknown modelTier');
      var cache = opts.cache;
      if (cache !== undefined && cache !== true && cache !== false && !isPlain(cache)) return bad('invalid_request', 'cache must be true, false or {gcTime, refresh}');
      if (jsonSize(input) > 262144 + 64) return bad('prompt_too_large', 'input over 256 KiB');
      var signal = opts.signal;
      if (signal && signal.aborted) return bad('cancelled', 'aborted');
      var id = ++sampleSeq, text = '', settled = false;
      function finish() { settled = true; samples.delete(id); if (signal) signal.removeEventListener('abort', onAbort); }
      function onAbort() { if (settled) return; post({ t: 'sample.abort', id: id }); finish(); reject(err('cancelled', 'aborted', text ? { text: text } : null)); }
      if (signal) signal.addEventListener('abort', onAbort);
      samples.set(id, function (m) {
        if (settled) return;
        if (m.t === 'sample.delta') {
          if (!m.delta) return;
          text += m.delta;
          if (opts.onText) { try { opts.onText({ text: text, delta: m.delta }); } catch (e) { report(e); } }
        } else if (m.t === 'sample.done') {
          finish();
          var res = m.result || {};
          if (asJSON) {
            var pj = parseJSONReply(text);
            if (!pj.ok || res.truncated) return reject(err('invalid_json', res.truncated ? 'the reply was cut short' : 'no JSON value in the reply', { text: text }));
            return resolve(pj.v);
          }
          var r = { text: text, truncated: !!res.truncated, modelTierApplied: res.modelTierApplied || 'default' };
          if (res.modelApplied) r.modelApplied = res.modelApplied;
          resolve(r);
        } else if (m.t === 'sample.err') {
          finish();
          var e = m.error || {};
          reject(err(e.code || 'upstream_error', e.message, text && e.code !== 'refused' ? { text: text } : null));
        }
      });
      later(function () {
        if (settled) return;
        post({ t: 'sample', id: id, req: { input: input, modelTier: opts.modelTier || 'default', model: typeof opts.model === 'string' ? opts.model : '', json: !!asJSON, cache: cache === undefined ? true : cache } });
      });
    });
  }
  var sampleNS = null;
  function makeSample() {
    if (sampleNS) return sampleNS;
    var fn = function sample(input, opts) { return runSample(input, opts, false); };
    fn.json = function (input, opts) { return runSample(input, opts, true); };
    fn.limits = function () { return Promise.resolve(Object.freeze({ maxPromptBytes: 262144, modelTiers: Object.freeze(['default', 'complex', 'quick']) })); };
    sampleNS = Object.freeze(fn);
    return sampleNS;
  }

  // ================= claude.use =================
  var memo = new Map();
  function use(name) {
    if (memo.has(name)) return memo.get(name);
    var p = new Promise(function (res) {
      setTimeout(function () {
        waitHello(10000).then(function (i) {
          if (!i) return res(null);
          var signedIn = !!i.user;
          switch (name) {
            case 'db': return res(hasCap('db') && signedIn ? makeDB() : null);
            case 'room': return res(hasCap('room') && signedIn ? makeRoom() : null);
            case 'sample': return res(hasCap('sample') && signedIn && i.sample ? makeSample() : null);
            case 'user': return res(makeUser());
            default: return res(null);
          }
        });
      }, 0);
    });
    memo.set(name, p);
    return p;
  }
  var claudeObj = Object.freeze({ use: use });
  try { Object.defineProperty(window, 'claude', { value: claudeObj, writable: false, configurable: false, enumerable: false }); }
  catch (e) { window.claude = claudeObj; }
})();
