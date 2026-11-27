// The index page opens from this worker's copy at once, and the copy is
// refreshed from the network in the background (stale-while-revalidate).
// app.js then checks the copy against the server (see revalidate there).
// Only the index is handled; every other request goes to the network as usual.
'use strict';
var CACHE = 'vitrine-pages';

self.addEventListener('install', function () { self.skipWaiting(); });
self.addEventListener('activate', function (e) { e.waitUntil(self.clients.claim()); });

self.addEventListener('fetch', function (e) {
  var req = e.request, url = new URL(req.url);
  if (req.mode !== 'navigate' || req.method !== 'GET' || url.origin !== location.origin || url.pathname !== '/' || url.searchParams.has('signed_out')) return;
  var fresh = fetch(req).then(function (res) {
    return caches.open(CACHE).then(function (c) {
      // a signed-out visitor is redirected to sign in: forget the copy so the next visit asks too
      if (res.ok && res.type === 'basic' && !res.redirected) return c.put(req, res.clone()).then(function () { return res; });
      return c.delete(req).then(function () { return res; });
    });
  });
  e.respondWith(caches.match(req).then(function (hit) { return hit || fresh; }));
  e.waitUntil(fresh.catch(function () { }));
});
