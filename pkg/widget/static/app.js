(function () {
  'use strict';
  var $ = function (id) { return document.getElementById(id); };
  var q = new URLSearchParams(location.search);
  var frag = new URLSearchParams(location.hash.replace(/^#/, ''));
  var widgetId = q.get('widgetId') || '';
  var roomId = frag.get('room') || '';
  var roomKey = frag.get('k') || '';
  // Drop the fragment (room key) from the address bar and history.
  try { history.replaceState(null, '', location.pathname + location.search); } catch (e) { /* ignore */ }

  var API = 'api/v1/';
  var parentOrigin = '*';
  try { if (q.get('parentUrl')) parentOrigin = new URL(q.get('parentUrl')).origin; } catch (e) { /* ignore */ }

  var S = { session: '', room: null, login: { ready: true, message: '' }, call: null,
            busyUntil: 0, hidden: document.hidden, errMsg: '', failures: 0,
            numberOK: false, pollTimer: 0, pollSeq: 0,
            features: {}, prefs: null, chatOK: false, actBusy: false, prefBusy: false,
            tab: 'dial' };
  // Contacts tab state. Memory only: never stored, wiped when the login or feature goes away.
  var C = { q: '', items: [], total: 0, next: null, loaded: true, fetched: false, loading: false,
            error: '', retry: false, seq: 0, at: 0, timer: 0 };
  var CONTACTS_PAGE = 25, CONTACTS_STALE_MS = 60000;

  // ---- UI helpers ----
  function setStatus(text, isErr) {
    var el = $('status');
    el.textContent = text;
    el.className = 'status' + (isErr ? ' err' : '');
  }
  function show(id, on) { $(id).hidden = !on; }
  function setTheme(name) {
    if (name === 'light' || name === 'dark') document.documentElement.setAttribute('data-theme', name);
  }
  setTheme(q.get('theme'));

  // ---- Minimal Matrix Widget API client ----
  var pending = {}; // requestId -> {resolve, reject}
  var seq = 0;
  var haveParent = window.parent !== window;
  var openidWaiter = null;
  var openidFallback = 0;

  function post(msg) {
    msg.widgetId = widgetId;
    window.parent.postMessage(msg, parentOrigin);
  }
  function reply(req, response) {
    var m = {}; for (var k in req) m[k] = req[k];
    m.response = response || {};
    post(m);
  }
  function request(action, data) {
    return new Promise(function (resolve, reject) {
      var id = 'dp-' + (++seq) + '-' + Math.random().toString(36).slice(2, 8);
      pending[id] = { resolve: resolve, reject: reject };
      post({ api: 'fromWidget', requestId: id, action: action, data: data || {} });
      setTimeout(function () {
        if (pending[id]) { delete pending[id]; reject(new Error('timeout')); }
      }, 20000);
    });
  }

  function onMessage(ev) {
    if (ev.source !== window.parent) return;
    var m = ev.data;
    if (!m || typeof m !== 'object' || typeof m.api !== 'string') return;
    if (widgetId && m.widgetId && m.widgetId !== widgetId) return;
    if (m.response !== undefined) { // answer to one of our requests
      var p = pending[m.requestId];
      if (p) { delete pending[m.requestId]; if (m.response && m.response.error) p.reject(new Error('widget api')); else p.resolve(m.response); }
      return;
    }
    if (m.api !== 'toWidget') return;
    switch (m.action) {
      case 'capabilities':
        reply(m, { capabilities: [] });
        // Fallback if the client never sends notify_capabilities.
        clearTimeout(openidFallback);
        openidFallback = setTimeout(startAuth, 1500);
        break;
      case 'notify_capabilities':
        reply(m, {});
        clearTimeout(openidFallback);
        startAuth();
        break;
      case 'supported_api_versions':
        reply(m, { supported_versions: ['0.0.1', '0.0.2', '1.0.0', '1.1.0'] });
        break;
      case 'theme_change':
        if (m.data) setTheme(m.data.name || (m.data.theme));
        reply(m, {});
        break;
      case 'visibility':
        reply(m, {});
        break;
      case 'openid_credentials':
        reply(m, {});
        if (m.data && openidWaiter) {
          var w = openidWaiter; openidWaiter = null;
          if (m.data.state === 'allowed') w.resolve(m.data); else w.reject(new Error('blocked'));
        }
        break;
      default:
        reply(m, {});
    }
  }
  window.addEventListener('message', onMessage);

  function getOpenID() {
    return request('get_openid', {}).then(function (r) {
      if (r.state === 'allowed') return r;
      if (r.state === 'request') {
        return new Promise(function (resolve, reject) {
          openidWaiter = { resolve: resolve, reject: reject };
          setTimeout(function () { if (openidWaiter) { openidWaiter = null; reject(new Error('timeout')); } }, 120000);
        });
      }
      throw new Error('blocked');
    });
  }

  // ---- API ----
  function api(method, path, body) {
    var opt = { method: method, headers: {}, cache: 'no-store', credentials: 'omit', referrerPolicy: 'no-referrer' };
    if (S.session) opt.headers.Authorization = 'Bearer ' + S.session;
    if (method === 'POST') { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body || {}); }
    return fetch(API + path, opt).then(function (res) {
      return res.json().catch(function () { return {}; }).then(function (j) {
        return { status: res.status, body: j || {} };
      });
    });
  }
  // Text of a rejected authed() call: the server's error when re-authentication failed with a response.
  function failText(e) { return e && e.res ? errText(e.res) : 'Cannot reach the bridge.'; }
  function errText(r) {
    var b = r.body || {};
    if (r.status === 429 || b.error === 'rate_limited') return 'Slow down: too many requests.';
    return b.message || (b.error ? 'Error: ' + b.error : 'Request failed (' + r.status + ').');
  }

  var authInFlight = null;
  function authenticate() {
    if (authInFlight) return authInFlight;
    authInFlight = getOpenID().then(function (o) {
      return api('POST', 'session', {
        openid: { access_token: o.access_token, matrix_server_name: o.matrix_server_name, expires_in: o.expires_in },
        room_id: roomId, key: roomKey, widget_id: widgetId
      });
    }).then(function (r) {
      if (r.status !== 200 || !r.body.session) { var e = new Error('session'); e.res = r; throw e; }
      S.session = r.body.session;
      S.room = r.body.room || null;
    }).then(function () { authInFlight = null; }, function (e) { authInFlight = null; throw e; });
    return authInFlight;
  }

  // Authenticated call; on 401 re-exchange once and retry.
  function authed(method, path, body) {
    return api(method, path, body).then(function (r) {
      if (r.status !== 401) return r;
      S.session = '';
      return authenticate().then(function () { return api(method, path, body); });
    });
  }

  var authStarted = false;
  function startAuth() {
    if (authStarted) return;
    authStarted = true;
    setStatus('Signing in...');
    authenticate().then(function () { return refresh(); }).then(function () {
      schedulePoll();
    }, function (e) {
      authStarted = false;
      if (e && e.res) setStatus(errText(e.res), true);
      else if (e && e.message === 'blocked') setStatus('This client did not allow sign-in.', true);
      else setStatus('Sign-in failed. Close and reopen the widget to retry.', true);
    });
  }

  // ---- State and rendering ----
  function refresh() {
    return authed('GET', 'state').then(function (r) {
      if (r.status !== 200) { S.failures++; setStatus(errText(r), true); return; }
      S.failures = 0;
      var b = r.body;
      S.room = b.room || S.room;
      S.login = b.login || { ready: true, message: '' };
      S.call = b.call || null;
      S.features = b.features || {};
      S.prefs = b.prefs || null;
      render();
      // The roster had not loaded: retry on the next successful poll while the tab is open.
      if (contactsVisible() && C.fetched && !C.loaded && !C.loading && !S.hidden) fetchContacts(false);
    }, function (e) {
      S.failures++;
      setStatus(e && e.res ? errText(e.res) : 'Cannot reach the bridge. Retrying...', true);
    });
  }

  var PHASE = {
    ringing_you: 'Ringing your devices: answer to connect',
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
  function render() {
    var inCall = !!S.call, kind = S.room && S.room.kind, num = S.room && S.room.number;
    var ready = S.login.ready !== false;
    if (inCall) setStatus(callText());
    else if (!ready) setStatus(S.login.message || 'Calling is not available for this login.', true);
    else if (S.errMsg) setStatus(S.errMsg, true);
    else setStatus('Ready');

    var rl = $('roomline');
    rl.hidden = !(kind === 'portal' && num);
    rl.textContent = num ? 'Contact: ' + num : '';

    var canIdle = !inCall && ready;
    // Tabs: only with the contacts feature; during a call the Dial panel is forced and the bar hidden.
    var tabsOn = ready && !inCall && !!S.features.contacts;
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
    var portal = kind === 'portal' && !!num;
    show('callbox', canIdle && portal);
    show('call-room', canIdle && portal);
    $('call-room').textContent = 'Call ' + num;
    $('call-room').disabled = S.actBusy;
    show('other-toggle', canIdle && portal);
    var otherOpen = $('other-toggle').getAttribute('aria-expanded') === 'true';
    show('dialbox', canIdle && (!portal || otherOpen));
    $('call-number').disabled = !S.numberOK || S.actBusy;

    var canDtmf = inCall && !!S.call.can_dtmf;
    var dialOpen = !$('dialbox').hidden;
    // Optional host features (the server reports them in state.features; absent = hidden). Input
    // values are never written from here, so polling cannot clobber typing.
    show('name-wrap', canIdle && dialOpen && !!S.features.save_contact);
    show('message-number', canIdle && dialOpen && !!S.features.chat);
    $('message-number').disabled = !(S.chatOK && !S.actBusy);
    show('save-number', canIdle && dialOpen && !!S.features.save_contact);
    $('save-number').disabled = !(S.chatOK && nameVal() !== '' && !S.actBusy);
    var c = S.room && S.room.contact;
    show('contact-line', portal && canIdle && !!c);
    if (c) $('contact-line').textContent = c.name ? 'Saved as ' + c.name : (c.saved ? 'In your contacts' : 'Not in your contacts');
    show('save-toggle', portal && canIdle && !!S.features.save_contact);
    $('save-toggle').textContent = c && c.name ? 'Rename contact' : 'Save contact';
    show('savebox', portal && canIdle && !!S.features.save_contact && $('save-toggle').getAttribute('aria-expanded') === 'true');
    $('save-room').disabled = !(saveNameVal() !== '' && !S.actBusy);
    show('settings', ready && !inCall && !!S.features.roster_only && !!S.prefs);
    if (S.prefs) {
      if (!S.prefBusy) $('roster-only').checked = !!S.prefs.roster_only;
      $('roster-only').disabled = S.prefBusy;
      $('roster-only-warn').textContent = S.prefs.warning || '';
      $('roster-only-warn').hidden = !S.prefs.warning;
    }

    show('keypad-wrap', canDtmf || dialOpen || inCall);
    setKeysEnabled(inCall ? canDtmf : dialOpen);
    show('hangup', inCall);
    $('hangup').textContent = (S.call && S.call.phase === 'incoming') ? 'Decline' : 'Hang up';

    var link = $('open-room');
    if (S.call && S.call.room_id && S.call.room_id !== roomId) {
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
    if (S.hidden || !S.session && !authStarted) return;
    S.pollTimer = setTimeout(function () {
      if (S.hidden) return;
      refresh().then(schedulePoll, schedulePoll);
    }, nextDelay());
  }
  document.addEventListener('visibilitychange', function () {
    S.hidden = document.hidden;
    if (!S.hidden && authStarted) { clearTimeout(S.pollTimer); refresh().then(schedulePoll, schedulePoll); }
  });
  function afterAction() { S.busyUntil = Date.now() + 10000; return refresh().then(schedulePoll, schedulePoll); }

  // ---- Actions ----
  function nameVal() { return $('contact-name').value.trim(); }
  function saveNameVal() { return $('save-name').value.trim(); }
  function clearResult() {
    $('result').hidden = true; $('result').textContent = '';
    $('open-chat').hidden = true;
  }
  function showResult(text) {
    if (!text) return;
    $('result').textContent = text; $('result').hidden = false;
  }
  // Start of a feature action: clears the last result and error, disables the buttons.
  function beginAction() { S.errMsg = ''; S.actBusy = true; clearResult(); render(); }
  function endAction(e) {
    S.actBusy = false;
    if (e) S.errMsg = failText(e);
    return e ? render() : afterAction();
  }
  function doCall(number, fromList) {
    S.errMsg = '';
    var body = number ? { number: number } : {};
    var name = number && !fromList && S.features.save_contact ? nameVal() : '';
    if (name) body.name = name;
    beginAction();
    setStatus('Placing call...');
    authed('POST', 'call', body).then(function (r) {
      if (r.status === 200 && r.body.ok !== false) {
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
  function doSaveRoom() {
    beginAction();
    authed('POST', 'contact', { name: saveNameVal() }).then(function (r) {
      if (r.status === 200 && r.body.ok !== false) {
        showResult(r.body.message);
        $('save-toggle').setAttribute('aria-expanded', 'false');
        $('save-name').value = '';
      } else { S.errMsg = errText(r); }
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
      if (r.status !== 200 || r.body.ok === false) S.errMsg = errText(r); else S.errMsg = '';
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
      var n = $('number');
      n.value += d; onNumberInput();
    }
  }

  // ---- Contacts tab ----
  function contactsVisible() { return !!S.features.contacts && S.login.ready !== false && !S.call && S.tab === 'contacts'; }
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
      if (e && e.res) { wipeContacts(); S.errMsg = failText(e); return render(); } // re-authentication failed
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

  // ---- Number field with debounced server-side normalisation ----
  var normTimer = 0, normSeq = 0;
  function onNumberInput() {
    S.numberOK = false; S.chatOK = false; $('call-number').disabled = true;
    render();
    var v = $('number').value.trim();
    clearTimeout(normTimer);
    if (!v) { $('preview').textContent = ''; return; }
    var my = ++normSeq;
    normTimer = setTimeout(function () {
      authed('POST', 'normalize', { number: v }).then(function (r) {
        if (my !== normSeq) return;
        var b = r.body;
        if (r.status === 200 && b.ok) {
          S.numberOK = true; $('preview').textContent = 'Will call ' + b.e164;
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

  // ---- Wiring ----
  $('call-room').addEventListener('click', function () { if (!S.actBusy) doCall(''); });
  $('call-number').addEventListener('click', function () { if (S.numberOK && !S.actBusy) doCall($('number').value.trim()); });
  $('message-number').addEventListener('click', function () { if (S.chatOK && !S.actBusy) doMessage($('number').value.trim(), false); });
  $('save-number').addEventListener('click', function () { if (S.chatOK && nameVal() && !S.actBusy) doSaveNumber(); });
  $('contact-name').addEventListener('input', render);
  $('save-name').addEventListener('input', render);
  $('save-room').addEventListener('click', function () { if (saveNameVal() && !S.actBusy) doSaveRoom(); });
  $('save-toggle').addEventListener('click', function () {
    var open = $('save-toggle').getAttribute('aria-expanded') === 'true';
    $('save-toggle').setAttribute('aria-expanded', String(!open));
    if (!open) { $('save-name').value = (S.room && S.room.contact && S.room.contact.name) || ''; }
    render();
    if (!open) $('save-name').focus();
  });
  $('roster-only').addEventListener('change', doRosterOnly);
  $('hangup').addEventListener('click', doHangup);
  $('number').addEventListener('input', onNumberInput);
  $('other-toggle').addEventListener('click', function () {
    var open = $('other-toggle').getAttribute('aria-expanded') === 'true';
    $('other-toggle').setAttribute('aria-expanded', String(!open));
    render();
    if (!open) $('number').focus();
  });
  document.addEventListener('keydown', function (e) {
    if (e.ctrlKey || e.metaKey || e.altKey) return;
    var t = e.target, inNum = t === $('number');
    if (e.key === 'Escape' && S.call) { e.preventDefault(); doHangup(); return; }
    if (e.key === 'Enter' && inNum) { e.preventDefault(); if (S.numberOK && !S.call && !S.actBusy) doCall($('number').value.trim()); return; }
    if (S.call && S.call.can_dtmf && /^[0-9*#]$/.test(e.key) && t.tagName !== 'BUTTON') { e.preventDefault(); pressKey(e.key); }
  });

  // ---- Boot ----
  if (!haveParent || !widgetId) {
    setStatus('Open this from the Dialpad widget in a room in your Matrix client.', true);
  } else if (!roomId || !roomKey) {
    setStatus('This widget link is incomplete. Remove and re-add the widget from the bridge.', true);
  } else {
    // Hidden until signed in; capabilities/notify_capabilities from the client start the flow.
    setTimeout(function () { if (!authStarted) startAuth(); }, 6000);
  }
})();
