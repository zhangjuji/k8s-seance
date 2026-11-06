// memory.js: the forgetting countdown (Requirement 14). The server renders the
// seconds left on each [data-memory-left] element. This script counts them down
// every 10 seconds in the browser, without asking the server, and keeps the
// classes and the list banner in step:
//   memory-fading     below 15 minutes
//   memory-vanishing  below 5 minutes (flickers, unless prefers-reduced-motion)
//   memory-gone       at zero ("forgotten")
// An element marked data-memory-main also sets these classes on its closest
// [data-memory-scope] (a list row, an evidence card, the séance panel).
// Elements that replace older ones (the live list) start from their own value.
(function () {
  'use strict';
  var TICK_MS = 10000;
  var FADING = 15 * 60;
  var VANISHING = 5 * 60;
  var STATES = ['memory-fading', 'memory-vanishing', 'memory-gone'];
  var started = typeof WeakMap === 'function' ? new WeakMap() : null;

  function labels() {
    var el = document.getElementById('memory-i18n');
    var d = el ? el.dataset : {};
    return {
      left: d.left || '{t}',
      gone: d.gone || '0',
      lt1: d.lt1 || '<1 min',
      h: d.h || 'h',
      min: d.min || 'min'
    };
  }

  function span(t, left) {
    if (left < 60) { return t.lt1; }
    var mins = Math.floor(left / 60);
    var h = Math.floor(mins / 60);
    var m = mins % 60;
    if (h === 0) { return m + ' ' + t.min; }
    if (m === 0) { return h + ' ' + t.h; }
    return h + ' ' + t.h + ' ' + m + ' ' + t.min;
  }

  function text(t, left) {
    return left <= 0 ? t.gone : t.left.replace('{t}', span(t, left));
  }

  function states(left) {
    if (left <= 0) { return ['memory-gone']; }
    if (left < VANISHING) { return ['memory-fading', 'memory-vanishing']; }
    if (left < FADING) { return ['memory-fading']; }
    return [];
  }

  function setStates(el, on) {
    STATES.forEach(function (c) { el.classList.toggle(c, on.indexOf(c) >= 0); });
  }

  // leftNow is the seconds left for el: its rendered value minus the time since
  // this script first saw it.
  function leftNow(el, now) {
    var initial = Number(el.getAttribute('data-memory-left'));
    if (!isFinite(initial)) { return null; }
    var t0 = started ? started.get(el) : Number(el.getAttribute('data-memory-t0'));
    if (!t0) {
      t0 = now;
      if (started) { started.set(el, t0); } else { el.setAttribute('data-memory-t0', String(t0)); }
    }
    return initial - Math.floor((now - t0) / 1000);
  }

  function scan() {
    var t = labels();
    var now = Date.now();
    var fading = false;
    var els = document.querySelectorAll('[data-memory-left]');
    for (var i = 0; i < els.length; i++) {
      var el = els[i];
      var left = leftNow(el, now);
      if (left === null) { continue; }
      var on = states(left);
      el.textContent = text(t, left);
      setStates(el, on);
      if (el.hasAttribute('data-memory-main')) {
        var scope = el.closest ? el.closest('[data-memory-scope]') : null;
        if (scope) { setStates(scope, on); }
      }
      if (el.hasAttribute('data-memory-banner') && left > 0 && left < FADING) {
        fading = true;
      }
    }
    var banner = document.getElementById('memory-banner');
    if (banner) { banner.hidden = !fading; }
  }

  window.seanceMemory = { scan: scan };
  function start() {
    scan();
    setInterval(scan, TICK_MS);
    if (document.body) {
      document.body.addEventListener('htmx:afterSettle', scan);
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
