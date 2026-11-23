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
      .then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { if (!r.ok) throw new Error(j.error || r.statusText); return j; }); });
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
  nav.appendChild(el('button', { class: 'link', text: 'Sign out', title: boot.user.email, onclick: function () { api('POST', '/auth/logout').then(function (j) { try { indexedDB.deleteDatabase('vitrine'); } catch (e) { } location.href = (j && j.next) || '/'; }); } }));

  if (location.pathname === '/settings') return settings();

  var search = el('input', { type: 'search', placeholder: 'Search titles and content', 'aria-label': 'Search' });
  search.value = new URLSearchParams(location.search).get('q') || '';
  var list = el('div', { class: 'cards' });
  var more = el('button', { class: 'small', text: 'More' });
  more.hidden = true;
  main.appendChild(el('div', { class: 'searchrow' }, [search]));
  main.appendChild(list);
  main.appendChild(more);
  var t = 0, last = 0;
  search.addEventListener('input', function () {
    clearTimeout(t);
    t = setTimeout(function () { history.replaceState(null, '', search.value ? '?q=' + encodeURIComponent(search.value) : '/'); fetchList(true); }, 200);
  });
  more.addEventListener('click', function () { fetchList(false); });

  function badge(it) {
    if (it.level !== 'owner') return el('span', { class: 'tag', text: 'from ' + it.owner_email.split('@')[0] + ' · ' + it.level });
    var gs = it.grants || [];
    if (gs.some(function (g) { return g.audience === 'public'; })) return el('span', { class: 'tag pub', text: 'public' });
    if (gs.length) return el('span', { class: 'tag', text: 'shared' });
    return el('span', { class: 'tag muted', text: 'private' });
  }
  function card(it) {
    var snip = null;
    if (it.snippet) {
      snip = el('p', { class: 'snip' });
      it.snippet.split(/(«[^»]*»)/).forEach(function (part) {
        if (part[0] === '«') snip.appendChild(el('mark', { text: part.slice(1, -1) })); else snip.appendChild(document.createTextNode(part));
      });
    }
    return el('a', { class: 'card', href: '/a/' + it.id }, [
      el('div', { class: 'card-title', text: it.title }),
      it.description ? el('p', { class: 'muted small desc', text: it.description }) : null,
      snip,
      el('div', { class: 'meta' }, [el('span', { class: 'muted small', text: 'v' + it.head + ' · ' + ago(it.updated_at) }),
        it.kind === 'markdown' ? el('span', { class: 'tag kind', text: 'doc' }) : null, badge(it)])
    ]);
  }
  function fetchList(reset) {
    if (reset) { last = 0; list.textContent = ''; }
    api('GET', '/api/artifacts?q=' + encodeURIComponent(search.value) + (last ? '&before=' + last : '')).then(function (j) {
      if (reset && !j.items.length) {
        list.appendChild(el('div', { class: 'empty small' }, [
          el('p', { text: search.value ? 'Nothing matches.' : 'No artifacts yet.' }),
          search.value ? null : el('p', { class: 'muted' }, ['Ask an agent to make one — ', el('a', { href: '/settings', text: 'connect it first' }), '.'])
        ]));
      }
      j.items.forEach(function (it) { list.appendChild(card(it)); last = it.updated_at; });
      more.hidden = j.items.length < 60;
    }).catch(function (e) { list.textContent = e.message; });
  }
  fetchList(true);

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
