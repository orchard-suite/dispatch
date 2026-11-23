// The desk's one script: live regions, changes in place, clocks that count,
// sortable tables, and the keyboard with its command menu. Pages work
// without it; it only makes them faster.
(function () {
  'use strict';
  var $ = function (s, r) { return (r || document).querySelector(s); };
  var $$ = function (s, r) { return Array.prototype.slice.call((r || document).querySelectorAll(s)); };

  // ---- clocks ------------------------------------------------------------
  // <time data-since="RFC3339"> counts up; data-until counts down.
  function span(ms) {
    var s = Math.max(0, Math.floor(ms / 1000));
    var d = Math.floor(s / 86400), h = Math.floor(s % 86400 / 3600), m = Math.floor(s % 3600 / 60), sec = s % 60;
    if (d > 0) return d + 'd ' + h + 'h';
    if (h > 0) return h + 'h ' + (m < 10 ? '0' : '') + m + 'm';
    if (m > 0) return m + 'm ' + (sec < 10 ? '0' : '') + sec + 's';
    return sec + 's';
  }
  function tick() {
    var now = Date.now();
    $$('[data-since]').forEach(function (el) {
      var t = Date.parse(el.getAttribute('data-since'));
      if (!isNaN(t)) { var v = span(now - t); if (el.textContent !== v) el.textContent = v; }
    });
    $$('[data-until]').forEach(function (el) {
      var t = Date.parse(el.getAttribute('data-until'));
      if (!isNaN(t)) { var v = t > now ? span(t - now) : 'now'; if (el.textContent !== v) el.textContent = v; }
    });
  }
  setInterval(tick, 1000);
  tick();

  // ---- swapping ----------------------------------------------------------
  // A region is any element with data-live and an id. A fetched copy of the
  // page replaces each region that changed, except one the person is typing in.
  function busy(el) {
    var a = document.activeElement;
    if (a && el.contains(a) && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName)) return true;
    return !!$('details[open][data-hold]', el) || !!$('dialog[open]', el);
  }
  function adopt(doc) {
    var swapped = 0;
    $$('[data-live][id]', doc).forEach(function (fresh) {
      var old = document.getElementById(fresh.id);
      if (!old || busy(old) || old.outerHTML === fresh.outerHTML) return;
      old.replaceWith(document.importNode(fresh, true));
      swapped++;
    });
    if (doc.title && doc.title !== document.title) document.title = doc.title;
    if (swapped) { tick(); wire(document); }
    return swapped;
  }
  function parse(html) { return new DOMParser().parseFromString(html, 'text/html'); }
  var inflight = null;
  function refresh() {
    if (inflight) { inflight.again = true; return; }
    inflight = { again: false };
    fetch(location.href, { headers: { 'X-Desk-Live': '1' }, credentials: 'same-origin' })
      .then(function (r) { return r.ok ? r.text() : null; })
      .then(function (t) { if (t) adopt(parse(t)); })
      .catch(function () {})
      .then(function () { var again = inflight.again; inflight = null; if (again) refresh(); });
  }

  // ---- live --------------------------------------------------------------
  var live = $('[data-live-dot]');
  function setLive(on) { if (live) { live.classList.toggle('on', on); live.title = on ? 'Live: this page updates itself' : 'Reconnecting'; } }
  if (window.EventSource && document.body.hasAttribute('data-stream')) {
    var es = new EventSource('/live');
    var timer = 0;
    es.addEventListener('open', function () { setLive(true); });
    es.addEventListener('error', function () { setLive(false); });
    es.addEventListener('change', function () { clearTimeout(timer); timer = setTimeout(refresh, 250); });
  }

  // ---- changes in place ----------------------------------------------------
  function toast(text, bad) {
    var t = $('#toast');
    if (!t) return;
    t.textContent = text;
    t.className = 'toast show' + (bad ? ' bad' : '');
    clearTimeout(toast.t);
    toast.t = setTimeout(function () { t.className = 'toast'; }, 4000);
  }
  document.addEventListener('submit', function (e) {
    var f = e.target;
    if (!(f instanceof HTMLFormElement) || (f.method || '').toLowerCase() !== 'post' || f.hasAttribute('data-reload')) return;
    if (f.hasAttribute('data-confirm') && !confirm(f.getAttribute('data-confirm'))) { e.preventDefault(); return; }
    e.preventDefault();
    // The form is done with: its region may be swapped once the answer comes.
    if (document.activeElement && f.contains(document.activeElement)) document.activeElement.blur();
    var body = new URLSearchParams(new FormData(f, e.submitter || undefined));
    var btns = $$('button', f);
    btns.forEach(function (b) { b.disabled = true; });
    fetch(f.action, { method: 'POST', body: body, credentials: 'same-origin', headers: { 'X-Desk-Live': '1' } })
      .then(function (r) {
        return r.text().then(function (t) { return { r: r, doc: parse(t) }; });
      })
      .then(function (x) {
        var url = new URL(x.r.url);
        var msg = $('[data-message]', x.doc);
        if (!x.r.ok) { toast(msg ? msg.textContent.trim() : 'That did not work.', true); return; }
        if (x.r.redirected && url.pathname === location.pathname) {
          if (url.search !== location.search || url.hash) history.replaceState(null, '', url.pathname + url.search + url.hash);
          adopt(x.doc);
          var said = $('[data-flash]', x.doc);
          toast(said ? said.textContent.trim() : (f.getAttribute('data-done') || 'Done.'));
          if (f.hasAttribute('data-clear')) f.reset();
          var d = f.closest('dialog'); if (d) d.close();
          return;
        }
        // Another page answered: show it whole, as a navigation would.
        document.title = x.doc.title;
        document.body.replaceWith(document.importNode(x.doc.body, true));
        history.pushState(null, '', x.r.redirected ? url.pathname + url.search + url.hash : location.href);
        boot();
      })
      .catch(function () { toast('The desk did not answer. Try again.', true); })
      .then(function () { btns.forEach(function (b) { b.disabled = false; }); });
  });

  // ---- sortable tables -----------------------------------------------------
  function sortBy(th) {
    var table = th.closest('table'), tbody = table.tBodies[0];
    var i = Array.prototype.indexOf.call(th.parentNode.children, th);
    var dir = th.getAttribute('aria-sort') === 'ascending' ? -1 : 1;
    $$('th', th.parentNode).forEach(function (h) { h.removeAttribute('aria-sort'); });
    th.setAttribute('aria-sort', dir === 1 ? 'ascending' : 'descending');
    var rows = $$('tr', tbody);
    var val = function (tr) { var c = tr.children[i]; if (!c) return ''; var v = c.getAttribute('data-v'); return v !== null ? v : c.textContent.trim().toLowerCase(); };
    rows.sort(function (a, b) {
      var x = val(a), y = val(b), nx = parseFloat(x), ny = parseFloat(y);
      if (!isNaN(nx) && !isNaN(ny) && String(nx) === x && String(ny) === y) return (nx - ny) * dir;
      return x < y ? -dir : x > y ? dir : 0;
    });
    rows.forEach(function (r) { tbody.appendChild(r); });
    try { localStorage.setItem('sort:' + location.pathname + ':' + (table.id || ''), i + ':' + dir); } catch (_) {}
  }

  // ---- command menu --------------------------------------------------------
  // Every element with data-cmd is a command: its text is the label, and
  // data-key its shortcut. Rail links and incident jumps come with it.
  var menu, input, list, items = [], cur = 0;
  function commands() {
    var out = [];
    $$('[data-cmd]').forEach(function (el) {
      if (el.closest('.cmdk')) return;
      out.push({ label: el.getAttribute('data-cmd'), hint: el.getAttribute('data-key') || '', group: el.getAttribute('data-group') || 'This page', run: function () { el.click(); if (el.matches('input,textarea')) el.focus(); } });
    });
    $$('.rail a[href], .tabbar a[href]').forEach(function (a) {
      var label = (a.getAttribute('data-name') || a.textContent).trim().replace(/\s+\d+$/, '');
      if (!out.some(function (c) { return c.label === 'Go to ' + label; })) out.push({ label: 'Go to ' + label, hint: '', group: 'Go to', run: function () { location.href = a.href; } });
    });
    return out;
  }
  function render(q) {
    q = q.trim().toLowerCase();
    var all = commands();
    var m = q.match(/^#?(\d+)$/);
    items = [];
    if (m) items.push({ label: 'Open incident #' + m[1], hint: 'Enter', group: 'Jump', run: function () { location.href = '/incidents/' + m[1]; } });
    all.forEach(function (c) { if (!q || c.label.toLowerCase().indexOf(q) >= 0 || c.group.toLowerCase().indexOf(q) >= 0) items.push(c); });
    if (q && !m) items.push({ label: 'Search incidents for “' + q + '”', hint: '', group: 'Search', run: function () { location.href = '/history?q=' + encodeURIComponent(q); } });
    cur = Math.min(cur, Math.max(0, items.length - 1));
    list.innerHTML = '';
    var group = '';
    items.forEach(function (c, i) {
      if (c.group !== group) { group = c.group; var h = document.createElement('li'); h.className = 'g'; h.textContent = group; list.appendChild(h); }
      var li = document.createElement('li');
      li.className = 'c' + (i === cur ? ' on' : '');
      li.setAttribute('role', 'option');
      li.setAttribute('aria-selected', i === cur ? 'true' : 'false');
      var l = document.createElement('span'); l.textContent = c.label; li.appendChild(l);
      if (c.hint) { var k = document.createElement('kbd'); k.textContent = c.hint; li.appendChild(k); }
      li.addEventListener('mousedown', function (e) { e.preventDefault(); choose(i); });
      list.appendChild(li);
    });
    if (!items.length) { var e = document.createElement('li'); e.className = 'none'; e.textContent = 'Nothing matches.'; list.appendChild(e); }
  }
  function choose(i) { var c = items[i]; if (!c) return; menu.close(); c.run(); }
  function openMenu(q) {
    menu = $('#cmdk');
    if (!menu) return;
    input = $('input', menu); list = $('ul', menu);
    input.value = q || ''; cur = 0; render(input.value);
    if (!menu.open) menu.showModal();
    input.focus();
  }
  document.addEventListener('input', function (e) { if (menu && e.target === input) { cur = 0; render(input.value); } });
  document.addEventListener('keydown', function (e) {
    if (menu && menu.open && document.activeElement === input) {
      if (e.key === 'ArrowDown') { cur = Math.min(items.length - 1, cur + 1); render(input.value); e.preventDefault(); }
      else if (e.key === 'ArrowUp') { cur = Math.max(0, cur - 1); render(input.value); e.preventDefault(); }
      else if (e.key === 'Enter') { choose(cur); e.preventDefault(); }
      return;
    }
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); openMenu(''); return; }
    var t = e.target;
    if (e.metaKey || e.ctrlKey || e.altKey || (t && (/^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName) || t.isContentEditable))) return;
    if (e.key === '/') { var s = $('[data-search]'); e.preventDefault(); if (s) { s.focus(); s.select && s.select(); } else openMenu(''); return; }
    if (e.key === '?') { e.preventDefault(); openMenu(''); return; }
    if (e.key === 'j' || e.key === 'k') {
      var rows = $$('[data-row]'); if (!rows.length) return;
      var i = rows.indexOf(document.activeElement);
      i = e.key === 'j' ? Math.min(rows.length - 1, i + 1) : Math.max(0, i - 1);
      rows[i].focus(); rows[i].scrollIntoView({ block: 'nearest' }); e.preventDefault(); return;
    }
    var k = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    var el = $('[data-key="' + k.toUpperCase() + '"]:not([disabled])');
    if (el && !el.closest('dialog:not([open])')) { e.preventDefault(); el.click(); if (el.matches('input,textarea')) el.focus(); }
  });

  // ---- wiring --------------------------------------------------------------
  function wire(root) {
    $$('th[data-sort]', root).forEach(function (th) {
      if (th._w) return; th._w = 1;
      th.tabIndex = 0;
      th.addEventListener('click', function () { sortBy(th); });
      th.addEventListener('keydown', function (e) { if (e.key === 'Enter') sortBy(th); });
    });
    $$('[data-open-menu]', root).forEach(function (b) {
      if (b._w) return; b._w = 1;
      b.addEventListener('click', function () { openMenu(''); });
    });
    $$('[data-row][data-href]', root).forEach(function (r) {
      if (r._w) return; r._w = 1;
      r.addEventListener('keydown', function (e) { if (e.key === 'Enter' && e.target === r) location.href = r.getAttribute('data-href'); });
    });
    $$('dialog[data-modal]', root).forEach(function (d) {
      if (d._w) return; d._w = 1;
      d.addEventListener('click', function (e) { if (e.target === d) d.close(); });
    });
    $$('[data-dialog]', root).forEach(function (b) {
      if (b._w) return; b._w = 1;
      b.addEventListener('click', function () { var d = document.getElementById(b.getAttribute('data-dialog')); if (d) { d.showModal(); var f = $('input,textarea,select', d); if (f) f.focus(); } });
    });
    $$('[data-filter]', root).forEach(function (b) {
      if (b._w) return; b._w = 1;
      b.addEventListener('click', function () {
        var scope = document.getElementById(b.getAttribute('data-filter'));
        if (!scope) return;
        var v = b.getAttribute('data-value');
        scope.setAttribute('data-show', v);
        $$('[data-filter="' + b.getAttribute('data-filter') + '"]').forEach(function (x) { x.setAttribute('aria-pressed', x === b ? 'true' : 'false'); });
        try { localStorage.setItem('filter:' + b.getAttribute('data-filter'), v); } catch (_) {}
      });
    });
    $$('table[id]', root).forEach(function (t) {
      try {
        var s = localStorage.getItem('sort:' + location.pathname + ':' + t.id); if (!s) return;
        var p = s.split(':'), th = t.tHead && t.tHead.rows[0] && t.tHead.rows[0].children[+p[0]];
        if (th && th.hasAttribute('data-sort')) { th.setAttribute('aria-sort', p[1] === '1' ? 'descending' : 'ascending'); sortBy(th); }
      } catch (_) {}
    });
    $$('[data-filter]', root).forEach(function (b) {
      try { var v = localStorage.getItem('filter:' + b.getAttribute('data-filter')); if (v && v === b.getAttribute('data-value') && b.getAttribute('aria-pressed') !== 'true') b.click(); } catch (_) {}
    });
  }
  function boot() { wire(document); tick(); }
  window.addEventListener('popstate', function () { location.reload(); });
  boot();
})();
