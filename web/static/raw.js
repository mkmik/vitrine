/* vitrine bare view: the artifact full-window, with no chrome and no runtime.
   It is framed so this page's CSP keeps it on /c/ (see shellCSP). */
(function () {
  'use strict';
  var boot = JSON.parse(document.body.dataset.boot);
  var frame = document.getElementById('frame');
  frame.src = boot.src;
  window.addEventListener('message', function (ev) {
    if (ev.source !== frame.contentWindow) return;
    var m = ev.data;
    if (!m || m.__vitrine_open !== 1 || typeof m.url !== 'string') return;
    var u;
    try { u = new URL(m.url); } catch (e) { return; }
    if (!/^(https?|mailto):$/.test(u.protocol)) return;
    if (navigator.userActivation && !navigator.userActivation.isActive) return;
    window.open(u.href, '_blank', 'noopener,noreferrer');
  });
})();
