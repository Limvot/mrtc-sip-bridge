'use strict';
// Minimal service worker: caches the static app shell only. API calls (the bridge endpoints) and every non-GET
// request are never intercepted, so responses from the bridge are never cached.
// Bump VERSION whenever any shell file changes.
var VERSION = '2035d85ed0dd';
var SHELL = ['./', 'app.js', 'app.css', 'icon.svg', 'icon-192.png', 'icon-512.png'];

// Caches are per scope: several bridges (or paths) on one origin each get their own prefix and only ever
// delete their own old caches.
function scopeHash(s) {
  var h = 0x811c9dc5; // FNV-1a, 32 bit
  for (var i = 0; i < s.length; i++) { h ^= s.charCodeAt(i); h = Math.imul(h, 0x01000193) >>> 0; }
  return ('0000000' + h.toString(16)).slice(-8);
}
var PREFIX = 'dialpad-' + scopeHash(self.registration.scope) + '-';
var CACHE = PREFIX + VERSION;

function shellUrls() {
  return SHELL.map(function (p) { return new URL(p, self.registration.scope).href; });
}

self.addEventListener('install', function (ev) {
  ev.waitUntil(caches.open(CACHE).then(function (c) { return c.addAll(SHELL); }).then(function () { return self.skipWaiting(); }));
});

self.addEventListener('activate', function (ev) {
  ev.waitUntil(caches.keys().then(function (keys) {
    return Promise.all(keys.filter(function (k) { return k.indexOf(PREFIX) === 0 && k !== CACHE; })
      .map(function (k) { return caches.delete(k); }));
  }).then(function () { return self.clients.claim(); }));
});

self.addEventListener('fetch', function (ev) {
  var req = ev.request;
  if (req.method !== 'GET') return;
  var u = new URL(req.url);
  if (u.origin !== self.location.origin) return;
  u.hash = '';
  u.search = '';
  if (shellUrls().indexOf(u.href) < 0) return;
  ev.respondWith(caches.open(CACHE).then(function (c) { return c.match(u.href); }).then(function (hit) { return hit || fetch(req); }));
});
