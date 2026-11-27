(function () {
  'use strict';
  var boot = JSON.parse(document.body.dataset.boot);
  var main = document.getElementById('main'), nav = document.getElementById('nav');
  function el(tag, attrs, kids) {
    var e = document.createElement(tag);
    for (var k in attrs || {}) { if (k === 'text') e.textContent = attrs[k]; else if (k.slice(0, 2) === 'on') e.addEventListener(k.slice(2), attrs[k]); else e.setAttribute(k, attrs[k]); }
    (kids || []).forEach(function (c) { if (c) e.appendChild(typeof c === 'string' ? document.createTextNode(c) : c); });
    return e;
  }
  function api(method, path, body) {
    return fetch(path, { method: method, credentials: 'same-origin', headers: { 'Content-Type': 'application/json', 'X-CSRF': boot.csrf || '' }, body: body ? JSON.stringify(body) : undefined })
      .then(function (r) {
        return r.json().then(function (j) {
          if (!r.ok) throw new Error((j && j.error) || r.statusText || 'HTTP ' + r.status);
          return j;
        }, function () { throw new Error(r.ok ? 'the server sent an unreadable reply' : r.statusText || 'HTTP ' + r.status); });
      });
  }
  function ago(t) {
    var s = Date.now() / 1000 - t;
    if (s < 60) return 'just now';
    if (s < 3600) return Math.floor(s / 60) + ' min ago';
    if (s < 86400) return Math.floor(s / 3600) + ' h ago';
    if (s < 86400 * 7) return Math.floor(s / 86400) + ' d ago';
    return new Date(t * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric', year: 'numeric' });
  }

  if (!boot.user) {
    main.appendChild(el('section', { class: 'empty' }, [
      el('h1', { text: 'Artifacts' }),
      el('p', { class: 'muted', text: 'Pages that agents make, at links that stay put.' }),
      el('a', { class: 'button primary', href: '/auth/login?next=' + encodeURIComponent(location.pathname), text: 'Sign in' })
    ]));
    return;
  }
  nav.appendChild(el('a', { href: '/', text: 'Artifacts' }));
  nav.appendChild(el('a', { href: '/settings', text: 'Connect agents' }));
  // the signed-in user: an avatar that opens a menu with who they are and "Sign out"
  var me = boot.user, who = me.name && me.name !== me.email ? me.name : '';
  var face = el('img', { src: me.avatar, alt: '', width: '28', height: '28' });
  face.addEventListener('error', function () { face.replaceWith(el('span', { text: (who || me.email).charAt(0).toUpperCase() })); });
  var userBtn = el('button', { class: 'avatar', title: me.email, 'aria-label': 'Account: ' + me.email, popovertarget: 'usermenu' }, [face]);
  function signOut() {
    // leaves even if the request fails (an expired session answers 401): the server side is gone either way
    var done = function (j) { try { indexedDB.deleteDatabase('vitrine'); } catch (e) { } location.href = (j && j.next) || '/?signed_out'; };
    api('POST', '/auth/logout').then(done, function () { done(); });
  }
  var userMenu = el('div', { class: 'menu usermenu', id: 'usermenu', popover: '' }, [
    who ? el('div', { class: 'menu-name', text: who }) : null,
    el('div', { class: 'menu-head', text: me.email }),
    el('hr'),
    el('button', { onclick: signOut }, [el('span', { text: 'Sign out' })])
  ]);
  // placed before it paints, right-aligned under the avatar
  userMenu.addEventListener('beforetoggle', function (e) {
    if (e.newState !== 'open') return;
    var r = userBtn.getBoundingClientRect();
    userMenu.style.top = r.bottom + 6 + 'px';
    userMenu.style.left = 'auto';
    userMenu.style.right = Math.max(8, innerWidth - r.right) + 'px';
  });
  document.body.appendChild(userMenu);
  nav.appendChild(userBtn);

  if (location.pathname === '/settings') return settings();

  // ---- the artifacts list: tabs, search, type filter, grid or rows, and personal folders ----
  var I = {
    search: 'M11 4a7 7 0 1 0 0 14a7 7 0 1 0 0-14zM20 20l-4-4',
    filter: 'M4 6h16M7 12h10M10 18h4',
    rows: 'M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01',
    grid: 'M4 4h7v7H4zM13 4h7v7h-7zM4 13h7v7H4zM13 13h7v7h-7z',
    newFolder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2zM12 10v6M9 13h6',
    folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
    more: 'M5 12h.01M12 12h.01M19 12h.01',
    check: 'M5 12l5 5L20 7',
    pen: 'M4 20h4L19 9l-4-4L4 16v4zM14 6l4 4',
    trash: 'M4 7h16M9 7V4h6v3M6 7l1 13h10l1-13',
    out: 'M15 18l-6-6 6-6'
  };
  function icon(d, size) {
    var NS = 'http://www.w3.org/2000/svg', s = document.createElementNS(NS, 'svg'), p = document.createElementNS(NS, 'path');
    s.setAttribute('viewBox', '0 0 24 24'); s.setAttribute('width', size || 20); s.setAttribute('height', size || 20); s.setAttribute('aria-hidden', 'true');
    p.setAttribute('d', d); p.setAttribute('fill', 'none'); p.setAttribute('stroke', 'currentColor');
    p.setAttribute('stroke-width', d === I.more || d === I.rows ? '2.6' : '1.8'); p.setAttribute('stroke-linecap', 'round'); p.setAttribute('stroke-linejoin', 'round');
    s.appendChild(p);
    return s;
  }
  function iconBtn(d, label, onclick) {
    return el('button', { class: 'icon', title: label, 'aria-label': label, onclick: onclick }, [icon(d)]);
  }
  function fail(e) { alert(e.message); }

  // what is shown lives in the URL: ?q=&owner=mine|shared&kind=html|markdown&folder=<id>
  var st;
  function readURL() {
    var p = new URLSearchParams(location.search);
    st = { q: p.get('q') || '', owner: p.get('owner') || '', kind: p.get('kind') || '', folder: p.get('folder') || '' };
  }
  function writeURL(push) {
    var p = new URLSearchParams();
    for (var k in st) if (st[k]) p.set(k, st[k]);
    var u = '/' + (p.toString() ? '?' + p : '');
    if (push) history.pushState(null, '', u); else history.replaceState(null, '', u);
  }
  readURL();
  // the top level, unfiltered, shows folders and the artifacts in none; anything else is a flat list
  function isRoot() { return !st.folder && !st.q && !st.owner && !st.kind; }

  var tabs = [['', 'All'], ['mine', 'Yours'], ['shared', 'Shared with you']].map(function (t) {
    return el('button', { class: 'tab', role: 'tab', 'data-v': t[0], text: t[1], onclick: function () { st.owner = t[0]; writeURL(); load(); } });
  });
  var searchBtn = iconBtn(I.search, 'Search', function () {
    if (searchRow.hidden) { searchRow.hidden = false; search.focus(); return; }
    searchRow.hidden = true;
    if (st.q) { search.value = st.q = ''; writeURL(); load(); }
  });
  var filterBtn = iconBtn(I.filter, 'Filter by type', function () {
    openMenu(filterBtn, [{ head: 'Type' }].concat([['', 'All types'], ['html', 'Pages'], ['markdown', 'Documents']].map(function (k) {
      return { label: k[1], check: st.kind === k[0], act: function () { st.kind = k[0]; writeURL(); load(); } };
    })));
  });
  var viewBtn = iconBtn(I.rows, '', function () { setView(view === 'rows' ? 'grid' : 'rows'); });
  var newBtn = iconBtn(I.newFolder, 'New folder', function () { newFolder(); });
  var search = el('input', { type: 'search', 'aria-label': 'Search' });
  var searchRow = el('div', { class: 'searchrow' }, [search]);
  var crumbHome = el('a', { href: '/', text: 'All artifacts', onclick: function (e) { if (plainClick(e)) { e.preventDefault(); go(''); } } });
  var crumbName = el('b');
  var crumbMore = iconBtn(I.more, 'Folder actions', function () { folderMenu(crumbMore, folderById(st.folder)); });
  var crumbs = el('div', { class: 'crumbs' }, [crumbHome, el('span', { class: 'muted', text: '›' }), crumbName, crumbMore]);
  dropTarget(crumbHome, function () { return ''; });
  var list = el('div', { class: 'cards' });
  var more = el('button', { class: 'small', text: 'More' });
  more.hidden = true;
  main.appendChild(el('div', { class: 'toolbar' }, [el('div', { class: 'tabs', role: 'tablist' }, tabs), el('div', { class: 'tools' }, [searchBtn, filterBtn, viewBtn, newBtn])]));
  main.appendChild(searchRow);
  main.appendChild(crumbs);
  main.appendChild(list);
  main.appendChild(more);
  search.value = st.q;
  var t = 0;
  search.addEventListener('input', function () {
    clearTimeout(t);
    t = setTimeout(function () { st.q = search.value; writeURL(); load(); }, 200);
  });
  more.addEventListener('click', function () { api('GET', listURL()).then(function (j) { showItems(j.items); }, fail); });
  window.addEventListener('popstate', function () { readURL(); search.value = st.q; load(); });

  var view = 'grid';
  try { view = localStorage.getItem('vitrine.view') || 'grid'; } catch (e) { }
  function setView(v) {
    view = v;
    try { localStorage.setItem('vitrine.view', v); } catch (e) { }
    list.classList.toggle('rows', v === 'rows');
    viewBtn.replaceChildren(icon(v === 'rows' ? I.grid : I.rows));
    viewBtn.title = v === 'rows' ? 'Show as grid' : 'Show as list';
    viewBtn.setAttribute('aria-label', viewBtn.title);
    rescale();
  }

  function plainClick(e) { return !(e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button); }
  function go(folder) { st = { q: '', owner: '', kind: '', folder: folder }; search.value = ''; writeURL(true); load(); window.scrollTo(0, 0); }

  // toolbar, search box and breadcrumbs follow the state
  function syncChrome() {
    tabs.forEach(function (b) { b.classList.toggle('on', b.dataset.v === st.owner); b.setAttribute('aria-selected', b.dataset.v === st.owner); });
    filterBtn.classList.toggle('on', !!st.kind);
    if (st.q) searchRow.hidden = false; else if (document.activeElement !== search) searchRow.hidden = true;
    var f = folderById(st.folder);
    crumbs.hidden = !st.folder;
    crumbName.textContent = f ? f.name : '';
    search.placeholder = f ? 'Search in ' + f.name : 'Search titles and content';
    newBtn.hidden = !!st.folder; // folders hold artifacts, not other folders
    document.title = f ? f.name + ' · Artifacts' : 'Artifacts';
  }

  var folders = [], last = 0, seq = 0;
  function folderById(id) { return folders.filter(function (f) { return f.id === id; })[0]; }
  function listURL() {
    return '/api/artifacts?q=' + encodeURIComponent(st.q) + (st.owner ? '&owner=' + st.owner : '') + (st.kind ? '&kind=' + st.kind : '') +
      (st.folder ? '&folder=' + encodeURIComponent(st.folder) : isRoot() ? '&folder=none' : '') + (last ? '&before=' + last : '');
  }
  function load() {
    var my = ++seq;
    last = 0;
    syncChrome();
    Promise.all([api('GET', '/api/folders'), api('GET', listURL())]).then(function (r) {
      if (my !== seq) return;
      folders = r[0].folders;
      if (st.folder && !folderById(st.folder)) { go(''); return; } // deleted, or not ours
      syncChrome();
      list.textContent = '';
      showFolders();
      showItems(r[1].items);
    }).catch(function (e) { if (my === seq) list.textContent = e.message; });
  }
  function showFolders() {
    list.querySelectorAll('.fcell').forEach(function (n) { n.remove(); });
    if (isRoot()) list.prepend.apply(list, folders.map(folderCell));
    emptyNote();
  }
  function showItems(items) {
    items.forEach(function (it) { list.appendChild(cell(it)); last = it.updated_at; });
    more.hidden = items.length < 60;
    emptyNote();
    rescale();
  }
  function emptyNote() {
    var old = list.querySelector('.empty');
    if (old && list.querySelector('.cell')) old.remove();
    if (list.querySelector('.cell, .empty')) return;
    var p = [el('p', { text: st.q ? 'Nothing matches.' : st.owner || st.kind ? 'Nothing here.' : st.folder ? 'This folder is empty.' : 'No artifacts yet.' })];
    if (st.folder && !st.q) p.push(el('p', { class: 'muted', text: 'Drag artifacts onto it, or use "Move to folder" in an artifact\'s menu.' }));
    else if (isRoot()) p.push(el('p', { class: 'muted' }, ['Ask an agent to make one — ', el('a', { href: '/settings', text: 'connect it first' }), '.']));
    list.appendChild(el('div', { class: 'empty small' }, p));
  }
  function refreshFolders() {
    return api('GET', '/api/folders').then(function (j) { folders = j.folders; syncChrome(); showFolders(); rescale(); });
  }

  function badge(it) {
    if (it.level !== 'owner') return el('span', { class: 'tag', text: 'from ' + it.owner_email.split('@')[0] + ' · ' + it.level });
    var gs = it.grants || [];
    if (boot.public_sharing && gs.some(function (g) { return g.audience === 'public'; })) return el('span', { class: 'tag pub', text: 'public' });
    if (gs.length) return el('span', { class: 'tag', text: 'shared' });
    return el('span', { class: 'tag muted', text: 'private' });
  }
  var DT = 'application/x-vitrine-artifact';
  function cell(it) {
    var snip = null;
    if (it.snippet) {
      snip = el('p', { class: 'snip' });
      it.snippet.split(/(«[^»]*»)/).forEach(function (part) {
        if (part[0] === '«') snip.appendChild(el('mark', { text: part.slice(1, -1) })); else snip.appendChild(document.createTextNode(part));
      });
    }
    var card = el('a', { class: 'card', href: '/a/' + it.id }, [
      el('div', { class: 'thumb' }, [el('div', { class: 'paper' }, [preview(it)])]),
      el('div', { class: 'card-body' }, [
        el('div', { class: 'card-title', text: it.title }),
        it.description ? el('p', { class: 'muted small desc', text: it.description }) : null,
        snip,
        el('div', { class: 'meta' }, [el('span', { class: 'muted small', text: (it.kind === 'markdown' ? 'Doc · ' : '') + ago(it.updated_at) }), badge(it)])
      ])
    ]);
    var c = el('div', { class: 'cell' }, [card]);
    var btn = iconBtn(I.more, 'Actions for ' + it.title, function () { itemMenu(btn, it, c); });
    btn.classList.add('more');
    c.appendChild(btn);
    card.addEventListener('dragstart', function (e) {
      e.dataTransfer.setData(DT, it.id);
      e.dataTransfer.effectAllowed = 'move';
      var r = card.getBoundingClientRect();
      e.dataTransfer.setDragImage(card, e.clientX - r.left, e.clientY - r.top);
      dragging = { it: it, cell: c };
      document.body.classList.add('dragging');
    });
    card.addEventListener('dragend', function () { dragging = null; document.body.classList.remove('dragging'); });
    return c;
  }
  // a page's thumbnail is the page itself, scaled down (lazily, inert), unless an image was uploaded
  function preview(it) {
    return it.thumb_img
      ? el('img', { src: '/a/' + it.id + '/thumb?v=' + it.thumb_img, alt: '', loading: 'lazy', draggable: 'false' })
      : el('iframe', { src: '/c/' + it.id + '/0?raw=1' + (it.thumb_y ? '&vy=' + it.thumb_y : ''), loading: 'lazy', tabindex: '-1', 'aria-hidden': 'true',
        sandbox: 'allow-scripts', referrerpolicy: 'no-referrer' });
  }
  // a folder shows its newest artifacts in a 2×2 grid, empty slots dashed
  function folderCell(f) {
    var slots = [];
    for (var i = 0; i < 4; i++) slots.push(f.items[i] ? el('div', { class: 'paper' }, [preview(f.items[i])]) : el('div', { class: 'slot' }));
    var card = el('a', { class: 'card folder', href: '/?folder=' + encodeURIComponent(f.id), draggable: 'false',
      onclick: function (e) { if (plainClick(e)) { e.preventDefault(); go(f.id); } } }, [
      el('div', { class: 'thumb minis' }, slots),
      el('div', { class: 'card-body frow' }, [icon(I.folder, 22), el('div', { class: 'card-title', text: f.name }),
        el('span', { class: 'muted small', text: f.count === 1 ? '1 item' : f.count + ' items' })])
    ]);
    dropTarget(card, function () { return f.id; });
    var c = el('div', { class: 'cell fcell' }, [card]);
    var btn = iconBtn(I.more, 'Actions for folder ' + f.name, function () { folderMenu(btn, f); });
    btn.classList.add('more');
    c.appendChild(btn);
    return c;
  }
  // scaled frames render at 1024px wide: set the scale from the papers' real widths
  function rescale() {
    var p = list.querySelector('.cell:not(.fcell) .paper'), m = list.querySelector('.fcell .paper');
    if (p) list.style.setProperty('--s', p.clientWidth / 1024);
    if (m) list.style.setProperty('--fs', m.clientWidth / 1024);
  }
  new ResizeObserver(rescale).observe(list);

  // drag an artifact onto a folder (or "All artifacts" to take it out)
  var dragging = null;
  function dropTarget(node, folderOf) {
    node.addEventListener('dragover', function (e) {
      if (!dragging) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      node.classList.add('drop');
    });
    node.addEventListener('dragleave', function (e) { if (!node.contains(e.relatedTarget)) node.classList.remove('drop'); });
    node.addEventListener('drop', function (e) {
      node.classList.remove('drop');
      if (!dragging) return;
      e.preventDefault();
      fileTo(dragging.it, folderOf(), dragging.cell);
    });
  }
  function fileTo(it, fid, c) {
    if ((it.folder || '') === fid) return;
    api('POST', '/api/artifacts/' + it.id + '/folder', { folder: fid }).then(function () {
      it.folder = fid;
      if (st.folder ? fid !== st.folder : isRoot() && fid) c.remove(); // no longer belongs in this view
      refreshFolders();
    }, fail);
  }
  function newFolder() {
    var name = prompt('New folder name', 'Untitled folder');
    if (!name || !name.trim()) return Promise.reject();
    return api('POST', '/api/folders', { name: name.trim() }).then(function (f) { refreshFolders(); return f; }, function (e) { fail(e); throw e; });
  }

  // menus: one popover, filled for whichever button opened it
  var menu = el('div', { class: 'menu', popover: '', role: 'menu' });
  document.body.appendChild(menu);
  // menuFrom: the button the open menu belongs to; dismissed: the one whose menu a click outside just closed
  var menuFrom = null, dismissed = null, dismissedAt = 0;
  function closeMenu() { if (menuFrom) menuFrom.setAttribute('aria-expanded', 'false'); menuFrom = null; }
  menu.addEventListener('beforetoggle', function (e) {
    if (e.newState === 'closed' && menuFrom) { dismissed = menuFrom; dismissedAt = Date.now(); closeMenu(); }
  });
  function openMenu(anchor, items) {
    if (anchor === dismissed && Date.now() - dismissedAt < 400) { dismissed = null; return; } // that click closed it: leave it closed
    menuFrom = anchor;
    menu.replaceChildren.apply(menu, items.map(function (m) {
      if (m.head) return el('div', { class: 'menu-head', text: m.head });
      if (m.sep) return el('hr');
      return el('button', { role: 'menuitem', class: m.danger ? 'danger' : '', onclick: function () { closeMenu(); menu.hidePopover(); m.act(); } },
        [icon(m.check ? I.check : m.icon || '', 17), el('span', { text: m.label })]);
    }));
    menu.showPopover();
    anchor.setAttribute('aria-expanded', 'true');
    var r = anchor.getBoundingClientRect(), w = menu.offsetWidth, h = menu.offsetHeight;
    menu.style.left = Math.max(8, Math.min(r.right - w, innerWidth - w - 8)) + 'px';
    menu.style.top = (r.bottom + 6 + h > innerHeight - 8 ? Math.max(8, r.top - h - 6) : r.bottom + 6) + 'px';
    var first = menu.querySelector('button'); if (first) first.focus();
  }
  function itemMenu(btn, it, c) {
    var items = [{ head: 'Move to folder' }];
    folders.forEach(function (f) { items.push({ label: f.name, icon: I.folder, check: it.folder === f.id, act: function () { fileTo(it, f.id, c); } }); });
    items.push({ label: 'New folder…', icon: I.newFolder, act: function () { newFolder().then(function (f) { fileTo(it, f.id, c); }, function () { }); } });
    if (it.folder) items.push({ sep: true }, { label: 'Remove from folder', icon: I.out, act: function () { fileTo(it, '', c); } });
    openMenu(btn, items);
  }
  function folderMenu(btn, f) {
    if (!f) return;
    openMenu(btn, [
      { label: 'Rename', icon: I.pen, act: function () {
        var name = prompt('Rename folder', f.name);
        if (name && name.trim() && name.trim() !== f.name) api('POST', '/api/folders/' + f.id, { name: name.trim() }).then(refreshFolders, fail);
      } },
      { label: 'Delete folder', icon: I.trash, danger: true, act: function () {
        if (!confirm('Delete the folder "' + f.name + '"? The artifacts in it are kept, back at the top level.')) return;
        api('DELETE', '/api/folders/' + f.id).then(function () { if (st.folder) go(''); else load(); }, fail);
      } }
    ]);
  }

  setView(view);
  load();

  function settings() {
    var mcp = boot.mcp_url;
    main.appendChild(el('h2', { text: 'Connect agents' }));
    main.appendChild(el('p', { text: 'Add this MCP server to any agent. Clients that support MCP sign-in (Claude, ChatGPT, Claude Code) open a browser once to sign you in.' }));
    main.appendChild(el('pre', { class: 'code', text: mcp }));
    main.appendChild(el('ul', { class: 'howto' }, [
      el('li', {}, [el('b', { text: 'Claude (web, desktop, mobile): ' }), 'Settings → Connectors → Add custom connector, paste the URL.']),
      el('li', {}, [el('b', { text: 'Claude Code: ' }), el('code', { text: 'claude mcp add --transport http artifacts ' + mcp })]),
      el('li', {}, [el('b', { text: 'Static token: ' }), 'for harnesses that only take a header, create a token below and send ', el('code', { text: 'Authorization: Bearer <token>' }),
        '. The same tools also answer at ', el('code', { text: boot.base + '/api/v1/tools/<name>' }), ' (POST JSON).'])
    ]));
    main.appendChild(el('h3', { text: 'Tokens and connected apps' }));
    var tbox = el('div', { class: 'tokens' });
    var name = el('input', { placeholder: 'token name, e.g. laptop' });
    var out = el('pre', { class: 'code' });
    out.hidden = true;
    main.appendChild(el('div', { class: 'add' }, [name, el('button', { class: 'primary', text: 'Create token', onclick: function () {
      api('POST', '/api/tokens', { name: name.value }).then(function (j) {
        out.hidden = false; out.textContent = j.token + '\n\nCopy it now: it is not shown again.'; name.value = ''; loadTokens();
      });
    } })]));
    main.appendChild(out);
    main.appendChild(tbox);
    function loadTokens() {
      api('GET', '/api/tokens').then(function (j) {
        tbox.textContent = '';
        if (!j.tokens.length) tbox.appendChild(el('p', { class: 'muted pad', text: 'No tokens or connected apps yet.' }));
        j.tokens.forEach(function (tk) {
          var label = tk.kind === 'pat' ? tk.name + ' (token)' : (tk.client_name || 'MCP client') + ' (signed in)';
          tbox.appendChild(el('div', { class: 'grant' }, [
            el('span', { text: label }),
            el('span', { class: 'muted small', text: 'created ' + ago(tk.created_at) + (tk.last_used_at ? ' · used ' + ago(tk.last_used_at) : '') }),
            el('button', { class: 'small', text: 'Revoke', onclick: function () { if (confirm('Revoke ' + label + '?')) api('DELETE', '/api/tokens/' + tk.id).then(loadTokens); } })
          ]));
        });
      });
    }
    loadTokens();
    main.appendChild(el('p', { class: 'muted small', text: 'Signed in as ' + boot.user.email + ' · vitrine ' + boot.version }));
  }
})();
