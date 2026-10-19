(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };

  // ---- Constants and storage keys ----
  var API = '../api/v1/';
  var CODE_RE = /mrtcsip_dpc_[A-Za-z0-9_-]{43}/;
  // Tiny non-cryptographic hash of the app path: keeps two bridges on one host apart.
  function hashStr(s) {
    var h = 5381;
    for (var i = 0; i < s.length; i++) h = ((h * 33) ^ s.charCodeAt(i)) >>> 0;
    return h.toString(36);
  }
  var PH = hashStr(location.pathname);
  var KEY = 'mrtcsip.dialpad.device.v1:' + PH;
  var RECENT_KEY = 'mrtcsip.dialpad.recent.v1:' + PH;
  var RECENT_OFF_KEY = 'mrtcsip.dialpad.recentoff.v1:' + PH;
  var LOCK_NAME = 'mrtcsip.dialpad.renew:' + PH;
  var RECENT_MAX = 10;

  var S = { dev: null, session: '', sessionExp: 0, login: { ready: true, message: '' }, call: null,
            busyUntil: 0, hidden: document.hidden, errMsg: '', failures: 0, numberOK: false,
            e164: '', pollTimer: 0, renewTimer: 0, active: false, offline: false, saveFailed: false,
            features: {}, prefs: null, chatOK: false, actBusy: false, prefBusy: false,
            tab: 'dial' };
  // Contacts tab state. Memory only (never localStorage/IndexedDB/service worker): wiped on every sign-out.
  var C = { q: '', items: [], total: 0, next: null, loaded: true, fetched: false, loading: false,
            error: '', retry: false, seq: 0, at: 0, timer: 0 };
  var CONTACTS_PAGE = 25, CONTACTS_STALE_MS = 60000;
  var pendingCode = ''; // enrolment code, memory only
  var deferredInstall = null;

  // ---- UI helpers ----
  function show(id, on) { $(id).hidden = !on; }
  function screen(name) {
    ['setup', 'confirm', 'main'].forEach(function (n) { show('screen-' + n, n === name); });
  }
  function setStatus(text, isErr) {
    var el = $('status');
    el.textContent = text;
    el.className = 'status' + (isErr ? ' err' : '');
  }
  function setWarn(text) { var el = $('warn'); el.textContent = text || ''; el.hidden = !text; }
  function setNote(text) { var el = $('setup-note'); el.textContent = text || ''; el.hidden = !text; }
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }

  // ---- Device storage (one key, one atomic setItem) ----
  function loadDev() {
    try {
      var raw = localStorage.getItem(KEY);
      if (!raw) return null;
      var d = JSON.parse(raw);
      if (!d || typeof d.token !== 'string' || !d.token) return null;
      return { token: d.token, id: d.id || '', name: d.name || '', user: d.user || '', gen: d.gen | 0 };
    } catch (e) { return null; }
  }
  function storeDev(d) {
    try { localStorage.setItem(KEY, JSON.stringify(d)); S.dev = d; return true; } catch (e) { return false; }
  }
  function clearDev() { try { localStorage.removeItem(KEY); } catch (e) { /* ignore */ } S.dev = null; }

  // ---- HTTP ----
  // Resolves {status, body}; rejects (Error 'network') only when the bridge could not be reached.
  function api(method, path, body, session) {
    var opt = { method: method, headers: {}, cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer' };
    if (session) opt.headers.Authorization = 'Bearer ' + session;
    if (method === 'POST') { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body || {}); }
    var ctl = typeof AbortController === 'function' ? new AbortController() : null;
    var to = 0;
    if (ctl) { opt.signal = ctl.signal; to = setTimeout(function () { ctl.abort(); }, 20000); }
    return fetch(API + path, opt).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (j) {
        clearTimeout(to);
        return { status: res.status, body: j || {} };
      });
    }, function () { clearTimeout(to); throw new Error('network'); });
  }
  function errText(r) {
    var b = r.body || {};
    if (r.status === 429 || b.error === 'rate_limited') return 'Slow down: too many requests.';
    return b.message || (b.error ? 'Error: ' + b.error : 'Request failed (' + r.status + ').');
  }

  // ---- Signed-out handling ----
  var OUT_TEXT = {
    device_expired: 'This device was signed out because it was not used for a long time.',
    device_revoked: 'This device was signed out (the link was revoked).',
    device_invalid: 'This device is no longer signed in.',
    reuse: 'Signed out for safety: an old copy of this app\'s sign-in was used somewhere else.'
  };
  var NEW_LINK = ' Get a new link with dialpad-link in your management room (your chat with the bridge bot).';
  function signOut(note) {
    clearDev();
    stopApp();
    pendingCode = '';
    setNote(note);
    screen('setup');
  }
  function stopApp() {
    S.active = false; S.session = ''; S.sessionExp = 0; S.call = null; S.saveFailed = false;
    clearTimeout(S.pollTimer); clearTimeout(S.renewTimer); clearInterval(tickTimer);
    S.tab = 'dial'; S.features = {};
    wipeContacts(); // the next person on a shared device must not see this list
  }

  // ---- Rotation: renew() exchanges (and thereby rotates) the device token for a session ----
  var renewing = null;
  function renew() {
    if (renewing) return renewing;
    renewing = renewLoop().then(function (r) { renewing = null; return r; }, function (e) { renewing = null; throw e; });
    return renewing;
  }
  async function renewLoop() {
    var delay = 2000;
    for (;;) {
      var r = await withLock(renewOnce);
      if (r !== 'retry') { S.offline = false; return r; }
      S.offline = true;
      if (S.active) setStatus('No connection to the bridge. Retrying...', true);
      await sleep(delay);
      delay = Math.min(30000, delay * 2);
    }
  }
  function withLock(fn) {
    if (navigator.locks && navigator.locks.request) return navigator.locks.request(LOCK_NAME, fn);
    return fn(); // no Web Locks: rely on the server's grace window
  }
  // Runs inside the lock. Returns 'ok' | 'out' | 'blocked' | 'retry'.
  async function renewOnce() {
    var cur = loadDev(); // re-read: another tab may have rotated while we waited
    if (!cur) return 'out';
    var r;
    try { r = await api('POST', 'device/session', { device_token: cur.token }); } catch (e) { return 'retry'; }
    var b = r.body || {};
    if (r.status === 200 && b.session && b.next_token) {
      // 1. persist the successor FIRST, 2. then use the session, 3. confirm
      var dv = b.device || {};
      var saved = storeDev({ token: b.next_token, id: dv.id || cur.id, name: dv.name || cur.name,
                             user: dv.user || cur.user, gen: cur.gen + 1 });
      S.saveFailed = !saved;
      if (!saved) S.dev = cur; // keep the old token: it stays current on the server
      S.session = b.session;
      S.sessionExp = Date.now() + (b.expires_in > 0 ? b.expires_in * 1000 : 3600000);
      S.inactiveExpiry = dv.inactive_expiry_at || '';
      scheduleRenew();
      if (saved) confirmSoon(b.next_token);
      return 'ok';
    }
    if (r.status === 401) {
      var again = loadDev();
      if (again && again.token !== cur.token) return 'retry'; // another tab stored a newer token
      var code = b.reason === 'reuse' ? 'reuse' : (b.error || 'device_invalid');
      signOut((OUT_TEXT[code] || OUT_TEXT.device_invalid) + NEW_LINK);
      return 'out';
    }
    if (r.status === 429 || r.status >= 500) return 'retry';
    S.blockMsg = errText(r);
    return 'blocked';
  }
  var CONFIRM_DELAYS = [0, 1000, 5000, 30000];
  async function confirmSoon(token) {
    for (var i = 0; i < CONFIRM_DELAYS.length; i++) {
      if (CONFIRM_DELAYS[i]) await sleep(CONFIRM_DELAYS[i]);
      var cur = loadDev();
      if (!cur || cur.token !== token) return; // replaced; presenting it later confirms it anyway
      try {
        var r = await api('POST', 'device/confirm', { device_token: token });
        if (r.status === 200 || r.status === 401 || r.status === 403) return;
      } catch (e) { /* retry */ }
    }
  }
  function scheduleRenew() {
    clearTimeout(S.renewTimer);
    var wait = Math.max(10000, S.sessionExp - Date.now() - 5 * 60000);
    S.renewTimer = setTimeout(function () { S.renewTimer = 0; if (S.active && !S.hidden) ensureSession(); }, Math.min(wait, 2147483000));
  }
  function ensureSession() {
    if (S.session && S.sessionExp - Date.now() > 5 * 60000) return Promise.resolve('ok');
    return renew().then(afterRenew);
  }
  function afterRenew(r) {
    if (r === 'ok') { renderHeader(); } else if (r === 'blocked') { showBlocked(); }
    return r;
  }
  function showBlocked() {
    stopApp();
    setNote((S.blockMsg || 'Sign-in is not available for this device.'));
    screen('setup');
  }

  // Authenticated call; on 401 renew once and retry. Rejects with Error('auth') when signed out.
  async function authed(method, path, body) {
    if (!S.session) { var r0 = await renew().then(afterRenew); if (r0 !== 'ok') throw new Error('auth'); }
    var r = await api(method, path, body, S.session);
    if (r.status !== 401) return r;
    S.session = '';
    var rr = await renew().then(afterRenew);
    if (rr !== 'ok') throw new Error('auth');
    return api(method, path, body, S.session);
  }
  function failText(e) { return e && e.message === 'network' ? 'No connection to the bridge.' : 'Signed out.'; }

  // Cross-tab changes of the stored credential.
  window.addEventListener('storage', function (ev) {
    if (ev.key !== KEY) return;
    if (ev.newValue === null) {
      if (S.dev) signOut('This device was signed out in another tab or window.' + NEW_LINK);
      return;
    }
    var d = loadDev();
    if (d) S.dev = d;
  });

  // ---- Enrolment ----
  function extractCode(text) { var m = CODE_RE.exec(text || ''); return m ? m[0] : ''; }
  var LINK_ERR = {
    link_used: 'This link was already used. Request a new one.',
    link_expired: 'This link has expired. Request a new one.',
    link_invalid: 'This link is not valid. Request a new one.'
  };
  async function startEnroll(code) {
    pendingCode = code;
    setNote('');
    var r;
    try { r = await api('POST', 'device/enroll', { code: code, confirm: false }); } catch (e) {
      pendingCode = '';
      setNote('Cannot reach the bridge. Check your connection and try again.'); screen('setup'); return;
    }
    var b = r.body || {};
    if (r.status !== 200 || !b.user) {
      pendingCode = '';
      var msg = r.status === 429 ? 'Slow down: too many attempts. Try again in a few minutes.'
        : (LINK_ERR[b.error] || (r.status === 410 ? LINK_ERR.link_expired : LINK_ERR.link_invalid));
      setNote(msg + (r.status === 429 ? '' : NEW_LINK)); screen('setup'); return;
    }
    var t = $('confirm-text');
    t.textContent = '';
    t.appendChild(document.createTextNode('Set up this device for '));
    var u = document.createElement('strong'); u.textContent = b.user; t.appendChild(u);
    if (b.name) { t.appendChild(document.createTextNode(' (')); var n = document.createElement('em'); n.textContent = b.name; t.appendChild(n); t.appendChild(document.createTextNode(')')); }
    t.appendChild(document.createTextNode('? This app can place calls on your phone account.'));
    var old = loadDev();
    var rep = $('confirm-replace');
    rep.hidden = !old;
    rep.textContent = old ? 'This replaces the current sign-in' + (old.user ? ' (' + old.user + ')' : '') + '.' : '';
    $('confirm-err').hidden = true;
    $('confirm-go').disabled = false;
    screen('confirm');
  }
  async function doEnroll() {
    if (!pendingCode) return;
    $('confirm-go').disabled = true;
    var r;
    var prev = loadDev();
    var body = { code: pendingCode, confirm: true };
    if (prev && prev.token) body.previous_token = prev.token; // server signs out the replaced device
    try { r = await api('POST', 'device/enroll', body); } catch (e) {
      $('confirm-err').textContent = 'Cannot reach the bridge. Try again.'; $('confirm-err').hidden = false;
      $('confirm-go').disabled = false; return;
    }
    var b = r.body || {};
    if (r.status === 200 && b.device_token) {
      pendingCode = '';
      var dv = b.device || {};
      if (!storeDev({ token: b.device_token, id: dv.id || '', name: dv.name || '', user: dv.user || '', gen: 0 })) {
        $('confirm-err').textContent = 'Could not save the sign-in on this device (is private browsing on?). The link is used up: request a new one.';
        $('confirm-err').hidden = false; return;
      }
      enterMain();
      return;
    }
    pendingCode = '';
    setNote((r.status === 429 ? 'Slow down: too many attempts.' : (LINK_ERR[b.error] || LINK_ERR.link_invalid)) + NEW_LINK);
    screen('setup');
  }

  // ---- Main screen ----
  function renderHeader() {
    var d = S.dev || loadDev() || {};
    $('who').textContent = (d.user || 'Signed in') + (d.name ? ' · ' + d.name : '');
    var info = 'Signed in as ' + (d.user || 'unknown') + (d.name ? ' on device "' + d.name + '"' : '') + '.';
    if (S.inactiveExpiry) {
      var t = Date.parse(S.inactiveExpiry);
      if (!isNaN(t)) info += ' Signs out automatically if unused until ' + new Date(t).toLocaleDateString() + '.';
    }
    $('sheet-info').textContent = info;
    setWarn(S.saveFailed ? 'Could not save the sign-in on this device. Calls work for now, but you may need a new link later.' : '');
  }
  function enterMain() {
    S.dev = loadDev();
    S.active = true;
    S.failures = 0;
    screen('main');
    renderHeader();
    renderRecent();
    setStatus('Signing in...');
    renew().then(afterRenew).then(function (r) {
      if (r !== 'ok') return;
      return refresh().then(schedulePoll, schedulePoll);
    });
  }

  var PHASE = {
    ringing_you: 'Ringing your Matrix app: answer there to connect',
    connecting: 'Connecting...',
    calling: 'Calling',
    ringing_them: 'Ringing',
    connected: 'Connected',
    incoming: 'Incoming call',
    ending: 'Ending call...'
  };
  var tickTimer = 0;
  function fmtDur(ms) {
    var s = Math.max(0, Math.floor(ms / 1000)), m = Math.floor(s / 60);
    s = s % 60; return (m < 10 ? '0' : '') + m + ':' + (s < 10 ? '0' : '') + s;
  }
  function callText() {
    var c = S.call, t = PHASE[c.phase] || 'In call';
    if (c.number && (c.phase === 'calling' || c.phase === 'ringing_them' || c.phase === 'incoming')) t += ' ' + c.number;
    if (c.phase === 'connected' && c.answered_at) {
      var a = Date.parse(c.answered_at);
      if (!isNaN(a) && a > 0) t += ' ' + fmtDur(Date.now() - a);
    }
    return t;
  }
  function refresh() {
    return authed('GET', 'state').then(function (r) {
      if (r.status !== 200) { S.failures++; setStatus(errText(r), true); return; }
      S.failures = 0;
      var b = r.body;
      S.login = b.login || { ready: true, message: '' };
      S.call = b.call || null;
      S.features = b.features || {};
      S.prefs = b.prefs || null;
      render();
      // Retry on the next successful poll while the tab is open: the roster had not loaded, or the list
      // was wiped (login not ready for a while, feature gone) and the tab is back.
      if (contactsVisible() && !C.loading && !S.hidden && (C.fetched ? !C.loaded : !C.error)) fetchContacts(false);
    }, function (e) {
      if (!S.active) return;
      S.failures++;
      setStatus(failText(e) + ' Retrying...', true);
    });
  }
  function render() {
    if (!S.active) return;
    var inCall = !!S.call, ready = S.login.ready !== false;
    if (inCall) setStatus(callText());
    else if (!ready) setStatus(S.login.message || 'Calling is not available for this login.', true);
    else if (S.errMsg) setStatus(S.errMsg, true);
    else setStatus('Ready');

    // Tabs: only with the contacts feature; during a call the Dial panel is forced and the bar hidden.
    var tabsOn = !inCall && ready && !!S.features.contacts;
    var tab = (inCall || !tabsOn) ? 'dial' : S.tab;
    show('tabs', tabsOn);
    show('panel-dial', tab === 'dial');
    show('panel-contacts', tab === 'contacts');
    ['dial', 'contacts'].forEach(function (t) {
      $('tab-' + t).setAttribute('aria-selected', String(tab === t));
      $('tab-' + t).tabIndex = tab === t ? 0 : -1;
    });
    if (!ready || !S.features.contacts) wipeContacts();
    syncRowButtons();
    show('dialbox', !inCall && ready);
    $('call-number').disabled = !S.numberOK || inCall || !ready || S.actBusy;
    // Optional host features (state.features; absent = hidden). Input values are never written here.
    var canIdle = !inCall && ready;
    show('name-wrap', canIdle && !!S.features.save_contact);
    show('message-number', canIdle && !!S.features.chat);
    $('message-number').disabled = !(S.chatOK && !S.actBusy);
    show('save-number', canIdle && !!S.features.save_contact);
    $('save-number').disabled = !(S.chatOK && nameVal() !== '' && !S.actBusy);
    show('roster-only-wrap', ready && !!S.features.roster_only && !!S.prefs);
    if (S.prefs) {
      if (!S.prefBusy) $('roster-only').checked = !!S.prefs.roster_only;
      $('roster-only').disabled = S.prefBusy;
      $('roster-only-warn').textContent = S.prefs.warning || '';
      $('roster-only-warn').hidden = !S.prefs.warning;
    }
    show('keypad-wrap', true);
    setKeysEnabled(inCall ? !!S.call.can_dtmf : ready);
    show('hangup', inCall);
    $('hangup').textContent = (S.call && S.call.phase === 'incoming') ? 'Decline' : 'Hang up';
    show('recent-wrap', recentVisible());

    var link = $('open-room');
    if (S.call && S.call.room_id) {
      link.href = 'https://matrix.to/#/' + encodeURIComponent(S.call.room_id);
      link.hidden = false;
    } else link.hidden = true;

    clearInterval(tickTimer);
    if (inCall && S.call.phase === 'connected') tickTimer = setInterval(function () { if (S.call) setStatus(callText()); }, 1000);
  }

  // ---- Polling ----
  function nextDelay() {
    var base = (S.call || Date.now() < S.busyUntil) ? 1000 : 5000;
    if (S.failures > 0) base = Math.min(30000, base * Math.pow(2, Math.min(S.failures, 5)));
    return base;
  }
  function schedulePoll() {
    clearTimeout(S.pollTimer);
    if (S.hidden || !S.active) return;
    S.pollTimer = setTimeout(function () {
      if (S.hidden || !S.active) return;
      refresh().then(schedulePoll, schedulePoll);
    }, nextDelay());
  }
  document.addEventListener('visibilitychange', function () {
    S.hidden = document.hidden;
    if (S.hidden || !S.active) return;
    clearTimeout(S.pollTimer);
    ensureSession().then(function (r) {
      if (r === 'ok' && S.active) return refresh();
    }).then(schedulePoll, schedulePoll);
  });
  function afterAction() { S.busyUntil = Date.now() + 10000; return refresh().then(schedulePoll, schedulePoll); }

  // ---- Recent numbers (local only) ----
  function recentOn() { try { return localStorage.getItem(RECENT_OFF_KEY) !== '1'; } catch (e) { return true; } }
  function recentList() {
    if (!recentOn()) return [];
    try {
      var a = JSON.parse(localStorage.getItem(RECENT_KEY) || '[]');
      return Array.isArray(a) ? a.filter(function (x) { return typeof x === 'string'; }).slice(0, RECENT_MAX) : [];
    } catch (e) { return []; }
  }
  function addRecent(n) {
    if (!recentOn() || !/^\+\d{3,20}$/.test(n)) return;
    var a = recentList().filter(function (x) { return x !== n; });
    a.unshift(n);
    try { localStorage.setItem(RECENT_KEY, JSON.stringify(a.slice(0, RECENT_MAX))); } catch (e) { /* ignore */ }
    renderRecent();
  }
  // The local Recent list lives in the Dial tab.
  function recentVisible() {
    return S.active && !S.call && S.login.ready !== false && recentList().length > 0 && !(S.tab === 'contacts' && !!S.features.contacts);
  }
  function clearRecent() { try { localStorage.removeItem(RECENT_KEY); } catch (e) { /* ignore */ } renderRecent(); }
  function renderRecent() {
    var ul = $('recent');
    ul.textContent = '';
    recentList().forEach(function (n) {
      var li = document.createElement('li');
      var b = document.createElement('button');
      b.type = 'button'; b.className = 'btn'; b.textContent = n;
      b.addEventListener('click', function () {
        $('number').value = n; onNumberInput();
        if (!S.call) doCall(n, true);
      });
      li.appendChild(b); ul.appendChild(li);
    });
    show('recent-wrap', recentVisible());
  }

  // ---- Actions ----
  function nameVal() { return $('contact-name').value.trim(); }
  function clearResult() {
    $('result').hidden = true; $('result').textContent = '';
    $('open-chat').hidden = true;
  }
  function showResult(text) {
    if (!text) return;
    $('result').textContent = text; $('result').hidden = false;
  }
  function beginAction() { S.errMsg = ''; S.actBusy = true; clearResult(); render(); }
  function endAction(e) {
    S.actBusy = false;
    if (e) S.errMsg = failText(e);
    return e ? render() : afterAction();
  }
  function doCall(number, fromRecent) { // fromRecent: skip the Name field (recent list and contacts rows)
    var body = { number: number }, name = !fromRecent && S.features.save_contact ? nameVal() : '';
    if (name) body.name = name;
    beginAction();
    setStatus('Placing call...');
    authed('POST', 'call', body).then(function (r) {
      if (r.status === 200 && r.body.ok !== false) {
        var n = typeof r.body.number === 'string' && /^\+\d+$/.test(r.body.number) ? r.body.number : (S.e164 || number);
        addRecent(n);
        if (r.body.message) setStatus(r.body.message);
        var c = r.body.contact;
        showResult(r.body.warning || (c && c.changed ? 'Saved as ' + c.name : ''));
      } else { S.errMsg = errText(r); }
      return endAction();
    }, endAction);
  }
  function doMessage(number, fromList) {
    var body = { number: number }, name = !fromList && S.features.save_contact ? nameVal() : '';
    if (name) body.name = name;
    beginAction();
    authed('POST', 'chat', body).then(function (r) {
      if (r.status === 200 && r.body.ok !== false) {
        showResult(r.body.message);
        if (r.body.room_id) {
          $('open-chat').href = 'https://matrix.to/#/' + encodeURIComponent(r.body.room_id);
          $('open-chat').hidden = false;
        }
      } else { S.errMsg = errText(r); }
      return endAction();
    }, endAction);
  }
  function doSaveNumber() {
    beginAction();
    authed('POST', 'contact', { number: $('number').value.trim(), name: nameVal() }).then(function (r) {
      if (r.status === 200 && r.body.ok !== false) showResult(r.body.message); else S.errMsg = errText(r);
      return endAction();
    }, endAction);
  }
  function doRosterOnly() {
    var box = $('roster-only'), want = box.checked;
    S.errMsg = ''; S.prefBusy = true; clearResult(); render();
    authed('POST', 'prefs/roster_only', { on: want }).then(function (r) {
      if (r.status === 200 && r.body.ok !== false) {
        S.prefs = { roster_only: !!r.body.roster_only, warning: r.body.warning || '' };
        showResult(r.body.message);
      } else { box.checked = !want; S.errMsg = errText(r); }
    }, function (e) { box.checked = !want; S.errMsg = failText(e); }).then(function () {
      S.prefBusy = false; render();
    });
  }
  function doHangup() {
    authed('POST', 'hangup', {}).then(function (r) {
      S.errMsg = (r.status !== 200 || r.body.ok === false) ? errText(r) : '';
      return afterAction();
    }, function (e) { S.errMsg = failText(e); render(); });
  }

  // DTMF: one digit per press; presses made while a request is in flight are coalesced.
  var dtmfBuf = '', dtmfBusy = false, shownTimer = 0;
  function showKeys(d) {
    var el = $('keys-shown');
    el.hidden = false;
    el.textContent = (el.dataset.m === '1' ? '' : el.textContent) + d;
    el.dataset.m = '0';
    clearTimeout(shownTimer);
    shownTimer = setTimeout(function () {
      el.textContent = el.textContent.replace(/./g, '•'); el.dataset.m = '1';
      shownTimer = setTimeout(function () { el.hidden = true; el.textContent = ''; el.dataset.m = '0'; }, 3000);
    }, 5000);
  }
  function flushDtmf() {
    if (dtmfBusy || !dtmfBuf) return;
    var d = dtmfBuf.slice(0, 32); dtmfBuf = dtmfBuf.slice(d.length);
    dtmfBusy = true;
    authed('POST', 'dtmf', { digits: d }).then(function (r) {
      if (r.status !== 200 || r.body.ok === false) { S.errMsg = errText(r); setStatus(S.errMsg, true); }
    }, function (e) { setStatus(failText(e), true); }).then(function () {
      dtmfBusy = false; flushDtmf();
    });
  }
  function pressKey(d) {
    if (S.call) {
      if (!S.call.can_dtmf) return;
      showKeys(d); dtmfBuf += d; flushDtmf();
    } else if (!$('dialbox').hidden && /^[0-9*#]$/.test(d)) {
      $('number').value += d; onNumberInput();
    }
  }

  // ---- Number field with debounced server-side normalisation ----
  var normTimer = 0, normSeq = 0;
  function onNumberInput() {
    S.numberOK = false; S.chatOK = false; S.e164 = ''; $('call-number').disabled = true;
    render();
    var v = $('number').value.trim();
    clearTimeout(normTimer);
    ++normSeq;
    if (!v) { $('preview').textContent = ''; return; }
    var my = normSeq;
    normTimer = setTimeout(function () {
      authed('POST', 'normalize', { number: v }).then(function (r) {
        if (my !== normSeq) return;
        var b = r.body;
        if (r.status === 200 && b.ok) {
          S.numberOK = true; S.e164 = b.e164 || ''; $('preview').textContent = 'Will call ' + b.e164;
        } else {
          $('preview').textContent = (r.status === 200 && b.message) ? b.message : errText(r);
          if (r.status === 200 && b.chat_ok) $('preview').textContent += ' You can still message it.';
        }
        S.chatOK = r.status === 200 && !!b.chat_ok;
        render();
      }, function (e) { if (my === normSeq) $('preview').textContent = failText(e); });
    }, 300);
  }

  // ---- Keypad ----
  var SUB = { 2: 'ABC', 3: 'DEF', 4: 'GHI', 5: 'JKL', 6: 'MNO', 7: 'PQRS', 8: 'TUV', 9: 'WXYZ', 0: '+' };
  function mkKey(parent, d, sub) {
    var b = document.createElement('button');
    b.type = 'button'; b.className = 'btn key'; b.dataset.d = d;
    b.setAttribute('aria-label', d === '*' ? 'star' : d === '#' ? 'hash' : d);
    b.appendChild(document.createTextNode(d));
    var s = document.createElement('small'); s.textContent = sub || ''; s.setAttribute('aria-hidden', 'true');
    b.appendChild(s);
    b.addEventListener('click', function () { pressKey(d); });
    parent.appendChild(b);
  }
  '123456789*0#'.split('').forEach(function (d) { mkKey($('keypad'), d, SUB[d]); });
  'ABCD'.split('').forEach(function (d) { mkKey($('keypad-extra'), d, ''); });
  function setKeysEnabled(on) {
    var ks = document.querySelectorAll('.key');
    for (var i = 0; i < ks.length; i++) ks[i].disabled = !on || (!S.call && /[A-D]/.test(ks[i].dataset.d));
  }
  $('abcd-toggle').addEventListener('click', function () {
    var open = $('abcd-toggle').getAttribute('aria-expanded') === 'true';
    $('abcd-toggle').setAttribute('aria-expanded', String(!open));
    $('abcd-toggle').textContent = open ? 'Show A-D' : 'Hide A-D';
    $('keypad-extra').hidden = open;
  });

  // ---- Contacts tab ----
  function contactsVisible() { return S.active && !!S.features.contacts && S.login.ready !== false && !S.call && S.tab === 'contacts'; }
  function wipeContacts() {
    clearTimeout(C.timer);
    C.seq++; C.q = ''; C.items = []; C.total = 0; C.next = null; C.loaded = true; C.fetched = false;
    C.loading = false; C.error = ''; C.retry = false; C.at = 0;
    if ($('contact-search').value) $('contact-search').value = '';
    renderContacts();
  }
  function contactsStatusText() {
    if (C.error) return C.error;
    if (C.loading && !C.items.length) return 'Loading contacts...';
    if (!C.fetched) return '';
    if (!C.loaded) return 'Your contacts have not loaded yet. Try again in a moment.';
    if (!C.items.length) return C.q ? 'No matches' : 'No saved phone contacts yet. Save a number from the Dial tab.';
    var n = C.total, one = n === 1;
    return n + (C.q ? (one ? ' match' : ' matches') : (one ? ' contact' : ' contacts'));
  }
  function syncRowButtons() {
    var bs = $('contacts-list').querySelectorAll('button');
    var off = S.actBusy || !!S.call || S.login.ready === false;
    for (var i = 0; i < bs.length; i++) {
      bs[i].disabled = off;
      if (bs[i].dataset.act === 'message') bs[i].hidden = !S.features.chat;
    }
  }
  function renderContacts() {
    var list = $('contacts-list'), hadFocus = document.activeElement === $('contacts-more');
    while (list.firstChild) list.removeChild(list.firstChild);
    C.items.forEach(function (it) {
      var li = document.createElement('li');
      var who = document.createElement('div'); who.className = 'cwho';
      var nm = document.createElement('span'); nm.className = 'cname'; nm.textContent = it.name;
      var nu = document.createElement('span'); nu.className = 'cnum'; nu.textContent = it.number;
      who.appendChild(nm); who.appendChild(nu);
      var acts = document.createElement('div'); acts.className = 'cacts';
      [['call', 'Call'], ['message', 'Message']].forEach(function (a) {
        var b = document.createElement('button');
        b.type = 'button'; b.className = 'btn' + (a[0] === 'call' ? ' primary' : ''); b.dataset.act = a[0];
        b.textContent = a[1];
        b.setAttribute('aria-label', a[1] + ' ' + it.name);
        b.addEventListener('click', function () {
          if (S.actBusy || S.call) return;
          if (a[0] === 'call') doCall(it.number, true); else doMessage(it.number, true);
        });
        acts.appendChild(b);
      });
      li.appendChild(who); li.appendChild(acts);
      list.appendChild(li);
    });
    $('contacts-status').textContent = contactsStatusText();
    $('contacts-status').className = 'preview' + (C.error ? ' err' : '');
    $('contacts-retry').hidden = !C.retry;
    $('contacts-more').hidden = C.next === null || !!C.error && !C.items.length;
    syncRowButtons();
    if (hadFocus && $('contacts-more').hidden) {
      var last = list.querySelector('li:last-child button');
      if (last) last.focus();
    }
  }
  // Fetches page 1 (append=false) or the next page. Responses of superseded requests are dropped.
  function fetchContacts(append) {
    var my = ++C.seq, off = append && C.next !== null ? C.next : 0;
    var path = 'contacts?limit=' + CONTACTS_PAGE + '&offset=' + off + (C.q ? '&q=' + encodeURIComponent(C.q) : '');
    C.loading = true; C.error = ''; C.retry = false;
    renderContacts();
    authed('GET', path).then(function (r) {
      if (my !== C.seq) return;
      C.loading = false;
      var b = r.body;
      if (r.status !== 200 || b.ok === false) {
        C.error = errText(r); C.retry = r.status !== 429; // 429: keep the list, the next input retries
        return renderContacts();
      }
      var items = (b.items || []).filter(function (it) { return it && typeof it.name === 'string' && typeof it.number === 'string'; });
      if (append) {
        var seen = {}; C.items.forEach(function (it) { seen[it.number] = true; });
        items = items.filter(function (it) { return !seen[it.number]; });
        C.items = C.items.concat(items);
      } else C.items = items;
      C.loaded = b.loaded !== false; C.total = b.total || 0;
      C.next = typeof b.next_offset === 'number' ? b.next_offset : null;
      C.fetched = true; C.at = Date.now();
      renderContacts();
    }, function (e) {
      if (my !== C.seq) return;
      C.loading = false;
      if (e && e.message === 'auth') { wipeContacts(); return; } // signed out: show nothing
      C.error = failText(e); C.retry = true;
      renderContacts();
    });
  }
  function enterContacts() {
    if (!C.fetched || !C.loaded || Date.now() - C.at > CONTACTS_STALE_MS) fetchContacts(false);
    else renderContacts();
  }
  function setTab(t) {
    if (S.tab === t) return;
    S.tab = t;
    render();
    if (t === 'contacts') enterContacts(); else { C.seq++; C.loading = false; }
  }
  $('tab-dial').addEventListener('click', function () { setTab('dial'); });
  $('tab-contacts').addEventListener('click', function () { setTab('contacts'); });
  $('tabs').addEventListener('keydown', function (e) {
    var order = ['dial', 'contacts'], i = order.indexOf(S.tab), n = -1;
    if (e.key === 'ArrowRight') n = (i + 1) % order.length;
    else if (e.key === 'ArrowLeft') n = (i + order.length - 1) % order.length;
    else if (e.key === 'Home') n = 0;
    else if (e.key === 'End') n = order.length - 1;
    if (n < 0) return;
    e.preventDefault();
    setTab(order[n]);
    $('tab-' + order[n]).focus();
  });
  $('contact-search').addEventListener('input', function () {
    clearTimeout(C.timer);
    C.timer = setTimeout(function () {
      var v = $('contact-search').value.trim();
      if (v === C.q && C.fetched) return;
      C.q = v; C.items = []; C.total = 0; C.next = null;
      fetchContacts(false);
    }, 250);
  });
  $('contacts-more').addEventListener('click', function () { if (!C.loading && C.next !== null) fetchContacts(true); });
  $('contacts-retry').addEventListener('click', function () { fetchContacts(false); });

  // ---- Settings sheet ----
  $('who').addEventListener('click', function () {
    var open = $('who').getAttribute('aria-expanded') === 'true';
    $('who').setAttribute('aria-expanded', String(!open));
    $('sheet').hidden = open;
    $('forget-confirm').hidden = true;
    $('forget').hidden = false;
  });
  $('recent-toggle').checked = recentOn();
  $('recent-toggle').addEventListener('change', function () {
    var on = $('recent-toggle').checked;
    try {
      if (on) localStorage.removeItem(RECENT_OFF_KEY);
      else { localStorage.setItem(RECENT_OFF_KEY, '1'); localStorage.removeItem(RECENT_KEY); }
    } catch (e) { /* ignore */ }
    renderRecent();
  });
  $('recent-clear').addEventListener('click', clearRecent);
  $('forget').addEventListener('click', function () { $('forget').hidden = true; $('forget-confirm').hidden = false; });
  $('forget-cancel').addEventListener('click', function () { $('forget-confirm').hidden = true; $('forget').hidden = false; $('forget-err').hidden = true; });
  $('forget-go').addEventListener('click', async function () {
    $('forget-go').disabled = true;
    try {
      var r = await authed('POST', 'device/forget', {});
      if (r.status !== 200 && r.status !== 401) throw new Error('fail');
    } catch (e) {
      if (!S.dev) return; // already signed out through the 401 path
      $('forget-err').textContent = 'Could not reach the bridge. Try again.'; $('forget-err').hidden = false;
      $('forget-go').disabled = false; return;
    }
    if ($('forget-clear').checked) clearRecent();
    $('forget-go').disabled = false;
    $('sheet').hidden = true; $('who').setAttribute('aria-expanded', 'false');
    signOut('This device was forgotten. To use the app again, request a link with dialpad-link in your management room.');
  });
  window.addEventListener('beforeinstallprompt', function (ev) {
    ev.preventDefault(); deferredInstall = ev; show('install', true);
  });
  $('install').addEventListener('click', function () {
    if (!deferredInstall) return;
    deferredInstall.prompt(); deferredInstall = null; show('install', false);
  });
  var standalone = (window.matchMedia && window.matchMedia('(display-mode: standalone)').matches) || window.navigator.standalone;
  if (!standalone) {
    var hint = $('sheet-install-hint');
    hint.textContent = 'To install: Chrome or Firefox menu, then Install app / Add to Home Screen. On iPhone: Share, then Add to Home Screen.';
    hint.hidden = false;
  }

  // ---- Wiring ----
  $('call-number').addEventListener('click', function () { if (S.numberOK && !S.actBusy) doCall($('number').value.trim()); });
  $('message-number').addEventListener('click', function () { if (S.chatOK && !S.actBusy) doMessage($('number').value.trim(), false); });
  $('save-number').addEventListener('click', function () { if (S.chatOK && nameVal() && !S.actBusy) doSaveNumber(); });
  $('contact-name').addEventListener('input', render);
  $('roster-only').addEventListener('change', doRosterOnly);
  $('hangup').addEventListener('click', doHangup);
  $('number').addEventListener('input', onNumberInput);
  $('backspace').addEventListener('click', function () {
    var n = $('number'); n.value = n.value.slice(0, -1); onNumberInput(); n.focus();
  });
  $('paste-go').addEventListener('click', function () {
    var code = extractCode($('paste').value);
    $('paste').value = '';
    if (!code) { setNote('No link code found in that text. Paste the whole message from the bridge bot.'); return; }
    startEnroll(code);
  });
  $('confirm-go').addEventListener('click', doEnroll);
  $('confirm-cancel').addEventListener('click', function () {
    pendingCode = '';
    if (loadDev()) enterMain(); else { setNote(''); screen('setup'); }
  });
  document.addEventListener('keydown', function (e) {
    if (!S.active || e.ctrlKey || e.metaKey || e.altKey) return;
    var t = e.target, inNum = t === $('number');
    if (e.key === 'Escape' && S.call) { e.preventDefault(); doHangup(); return; }
    if (e.key === 'Enter' && inNum) { e.preventDefault(); if (S.numberOK && !S.call && !S.actBusy) doCall($('number').value.trim()); return; }
    if (S.call && S.call.can_dtmf && /^[0-9*#]$/.test(e.key) && t.tagName !== 'BUTTON') { e.preventDefault(); pressKey(e.key); }
  });

  // ---- Boot ----
  (function boot() {
    var frag = new URLSearchParams(location.hash.replace(/^#/, ''));
    var code = extractCode(frag.get('c') || '');
    // Remove the fragment before any network request (the service worker registration fetches sw.js).
    if (location.hash) { try { history.replaceState(null, '', location.pathname); } catch (e) { /* ignore */ } }
    if ('serviceWorker' in navigator) navigator.serviceWorker.register('sw.js', { scope: './' }).catch(function () { /* optional */ });
    if (code) { startEnroll(code); return; }
    if (loadDev()) { enterMain(); return; }
    screen('setup');
  })();
})();
