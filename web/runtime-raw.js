/* vitrine page runtime outside the viewer (thumbnails, the bare page): there
   is no shell to serve claude.use, so every capability is null, as in
   runtime.js's raw mode. */
(function () {
  'use strict';
  var CFG = window.__vitrine_cfg || {};
  try { delete window.__vitrine_cfg; } catch (e) { window.__vitrine_cfg = undefined; }
  var claudeObj = Object.freeze({ use: function () { return Promise.resolve(null); } });
  try { Object.defineProperty(window, 'claude', { value: claudeObj, writable: false, configurable: false, enumerable: false }); }
  catch (e) { window.claude = claudeObj; }
  if (CFG.y > 0) { // opened scrolled down: a thumbnail; again on load, after late layout
    var goY = function () { window.scrollTo(0, CFG.y); };
    document.addEventListener('DOMContentLoaded', goY); window.addEventListener('load', goY); setTimeout(goY, 800);
  }
})();
