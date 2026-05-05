// backup-operator docs SPA. Hash-routed, vanilla JS, no build step —
// matches the management UI's deliberate "self-contained binary" stance.
(function() {
'use strict';

const $ = (sel, ctx) => (ctx || document).querySelector(sel);
const $$ = (sel, ctx) => [...(ctx || document).querySelectorAll(sel)];

const state = {
  pages: [],
  current: null,
  pageCache: new Map(),
  pendingHighlight: null, // search term to scroll/highlight after next render
};

async function api(path) {
  const resp = await fetch(path);
  if (!resp.ok) throw new Error(resp.statusText);
  return resp.json();
}

function escapeHTML(s) {
  const div = document.createElement('div');
  div.textContent = s == null ? '' : String(s);
  return div.innerHTML;
}

async function loadPage(slug) {
  if (slug === 'tech-stack') return loadTechStack();
  if (state.pageCache.has(slug)) return state.pageCache.get(slug);
  const data = await api('/api/page/' + encodeURIComponent(slug));
  state.pageCache.set(slug, data);
  return data;
}

async function loadTechStack() {
  if (state.pageCache.has('tech-stack')) return state.pageCache.get('tech-stack');
  const data = await api('/api/tech-stack');
  state.pageCache.set('tech-stack', { tech: data });
  return state.pageCache.get('tech-stack');
}

function renderNav() {
  const nav = $('#nav');
  nav.innerHTML = state.pages.map(p =>
    `<a href="#${escapeHTML(p.slug)}" data-slug="${escapeHTML(p.slug)}" class="nav-link">${escapeHTML(p.title)}</a>`
  ).join('');
  highlightCurrent();
}

function highlightCurrent() {
  $$('.nav-link').forEach(a => {
    a.classList.toggle('active', a.dataset.slug === state.current);
  });
}

function renderTOC(headings) {
  const toc = $('#toc');
  if (!headings || headings.length === 0) { toc.innerHTML = ''; return; }
  toc.innerHTML = '<div class="toc-title">On this page</div><ul>' +
    headings.map(h =>
      `<li class="toc-l${h.level}"><a href="#${escapeHTML(state.current)}/${escapeHTML(h.id)}">${escapeHTML(h.text)}</a></li>`
    ).join('') + '</ul>';
}

function renderMarkdownPage(p) {
  const article = $('#page');
  article.classList.remove('page-loading');
  article.innerHTML = `
    <header class="page-head">
      <h1>${escapeHTML(p.title)}</h1>
      <div class="meta">
        <span>Source: <code>${escapeHTML(p.source || '')}</code></span>
        ${p.updated ? '<span>Updated: ' + escapeHTML(new Date(p.updated).toLocaleString()) + '</span>' : ''}
      </div>
    </header>
    <div class="md-body">${p.html}</div>
  `;
  renderTOC(p.headings || []);
  // Apply a pending search highlight (set by a search-hit click) before
  // honouring an explicit heading anchor — the highlight does its own
  // scrollIntoView, and a later anchor would override it.
  if (state.pendingHighlight) {
    const q = state.pendingHighlight;
    state.pendingHighlight = null;
    highlightInArticle(q);
    return;
  }
  const parts = location.hash.slice(1).split('/');
  if (parts.length >= 2 && parts[1]) {
    const target = document.getElementById(parts[1]);
    if (target) target.scrollIntoView({ behavior: 'instant', block: 'start' });
  } else {
    window.scrollTo(0, 0);
  }
}

function renderTechStack(tech) {
  const article = $('#page');
  article.classList.remove('page-loading');
  const dep = d => `<li>
    <div class="dep-head">
      <code class="dep-name">${escapeHTML(d.name)}</code>
      ${d.version ? '<span class="dep-version">' + escapeHTML(d.version) + '</span>' : ''}
      ${d.license ? '<span class="dep-license">' + escapeHTML(d.license) + '</span>' : ''}
    </div>
    <div class="dep-purpose">${escapeHTML(d.purpose || '—')}</div>
  </li>`;
  article.innerHTML = `
    <header class="page-head">
      <h1>Tech Stack</h1>
      <div class="meta">
        <span>Module: <code>${escapeHTML(tech.module)}</code></span>
        <span>Go: <code>${escapeHTML(tech.goVersion)}</code></span>
        <span>${tech.indirectCount} transitive deps</span>
      </div>
    </header>
    <div class="md-body">
      <h2 id="backend">Backend (Go) — direct dependencies</h2>
      <ul class="dep-list">${(tech.directDeps || []).map(dep).join('')}</ul>
      <h2 id="frontend">Frontend</h2>
      <ul class="dep-list">${(tech.frontend || []).map(dep).join('')}</ul>
      <h2 id="operational">Operational dependencies</h2>
      <p>Tools the worker pod shells out to or services the cluster must provide.</p>
      <ul class="dep-list">${(tech.operationalDeps || []).map(dep).join('')}</ul>
      <h2 id="build">Build &amp; release tooling</h2>
      <ul class="dep-list">${(tech.buildTooling || []).map(dep).join('')}</ul>
    </div>
  `;
  renderTOC([
    { level: 2, text: 'Backend (Go)', id: 'backend' },
    { level: 2, text: 'Frontend', id: 'frontend' },
    { level: 2, text: 'Operational dependencies', id: 'operational' },
    { level: 2, text: 'Build & release tooling', id: 'build' },
  ]);
  if (state.pendingHighlight) {
    const q = state.pendingHighlight;
    state.pendingHighlight = null;
    highlightInArticle(q);
    return;
  }
  window.scrollTo(0, 0);
}

async function navigate() {
  const hash = location.hash.slice(1);
  const slug = (hash.split('/')[0]) || (state.pages[0] && state.pages[0].slug);
  if (!slug) return;
  state.current = slug;
  highlightCurrent();
  $('#page').classList.add('page-loading');
  $('#page').textContent = 'Loading…';
  try {
    const data = await loadPage(slug);
    if (slug === 'tech-stack') renderTechStack(data.tech);
    else renderMarkdownPage(data);
  } catch (e) {
    $('#page').innerHTML = '<div class="error">Failed to load: ' + escapeHTML(e.message) + '</div>';
  }
}

// Lightweight client-side full-text search across all loaded pages.
// Pre-loads each page on first search so the user gets a single warmup
// hit instead of N round-trips per keystroke.
let searchPrimed = false;
async function primeSearch() {
  if (searchPrimed) return;
  searchPrimed = true;
  await Promise.all(state.pages.map(async p => {
    if (p.kind === 'markdown') await loadPage(p.slug);
  }));
}

// Build up to `max` snippets per page so a single page with many matches
// surfaces multiple jump points instead of just the first.
function collectMatches(plain, q, max) {
  const out = [];
  const lower = plain.toLowerCase();
  let from = 0;
  while (out.length < max) {
    const idx = lower.indexOf(q, from);
    if (idx < 0) break;
    const start = Math.max(0, idx - 40);
    const end = Math.min(plain.length, idx + q.length + 40);
    out.push({
      idx,
      snippet: (start > 0 ? '…' : '') + plain.slice(start, end) + (end < plain.length ? '…' : ''),
    });
    from = idx + q.length;
  }
  return out;
}

function highlightSnippet(snippet, q) {
  const lower = snippet.toLowerCase();
  const ql = q.toLowerCase();
  let i = 0;
  let out = '';
  while (i < snippet.length) {
    const idx = lower.indexOf(ql, i);
    if (idx < 0) { out += escapeHTML(snippet.slice(i)); break; }
    if (idx > i) out += escapeHTML(snippet.slice(i, idx));
    out += '<mark>' + escapeHTML(snippet.slice(idx, idx + ql.length)) + '</mark>';
    i = idx + ql.length;
  }
  return out;
}

function runSearch(q) {
  q = q.trim();
  const results = $('#search-results');
  if (!q) { results.classList.remove('open'); results.innerHTML = ''; return; }
  const ql = q.toLowerCase();
  const hits = [];
  for (const p of state.pages) {
    if (p.kind !== 'markdown') {
      if (p.title.toLowerCase().includes(ql)) {
        hits.push({ slug: p.slug, title: p.title, snippet: '' });
      }
      continue;
    }
    const cached = state.pageCache.get(p.slug);
    if (!cached) continue;
    const plain = cached.html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ');
    const matches = collectMatches(plain, ql, 5);
    if (matches.length > 0) {
      for (const m of matches) {
        hits.push({ slug: p.slug, title: p.title, snippet: m.snippet });
      }
    } else if (p.title.toLowerCase().includes(ql)) {
      hits.push({ slug: p.slug, title: p.title, snippet: '' });
    }
  }
  results.classList.add('open');
  if (hits.length === 0) {
    results.innerHTML = '<div class="search-empty">No matches.</div>';
    return;
  }
  results.innerHTML = hits.map(h => `
    <a href="#${escapeHTML(h.slug)}" class="search-hit" data-slug="${escapeHTML(h.slug)}" data-q="${escapeHTML(q)}">
      <div class="hit-title">${escapeHTML(h.title)}</div>
      ${h.snippet ? '<div class="hit-snippet">' + highlightSnippet(h.snippet, q) + '</div>' : ''}
    </a>`).join('');
}

function clearArticleHighlights() {
  const article = $('#page');
  if (!article) return;
  $$('mark.search-highlight', article).forEach(m => {
    const parent = m.parentNode;
    parent.replaceChild(document.createTextNode(m.textContent), m);
    parent.normalize();
  });
}

// Walk the article's text nodes, wrap every occurrence of `q` in
// <mark class="search-highlight">, and scroll the first match into view.
// Skips text inside <script>/<style>/existing marks so we don't recurse.
function highlightInArticle(q) {
  if (!q) return;
  clearArticleHighlights();
  const article = $('#page');
  if (!article) return;
  const ql = q.toLowerCase();
  const walker = document.createTreeWalker(article, NodeFilter.SHOW_TEXT, {
    acceptNode(n) {
      if (!n.nodeValue || !n.nodeValue.toLowerCase().includes(ql)) return NodeFilter.FILTER_REJECT;
      let p = n.parentNode;
      while (p && p !== article) {
        const tag = p.nodeName;
        if (tag === 'SCRIPT' || tag === 'STYLE' || tag === 'MARK') return NodeFilter.FILTER_REJECT;
        p = p.parentNode;
      }
      return NodeFilter.FILTER_ACCEPT;
    },
  });
  const targets = [];
  let n;
  while ((n = walker.nextNode())) targets.push(n);
  let firstMark = null;
  for (const tn of targets) {
    const text = tn.nodeValue;
    const lower = text.toLowerCase();
    const frag = document.createDocumentFragment();
    let i = 0;
    while (i < text.length) {
      const idx = lower.indexOf(ql, i);
      if (idx < 0) { frag.appendChild(document.createTextNode(text.slice(i))); break; }
      if (idx > i) frag.appendChild(document.createTextNode(text.slice(i, idx)));
      const mark = document.createElement('mark');
      mark.className = 'search-highlight';
      mark.textContent = text.slice(idx, idx + ql.length);
      frag.appendChild(mark);
      if (!firstMark) firstMark = mark;
      i = idx + ql.length;
    }
    tn.parentNode.replaceChild(frag, tn);
  }
  if (firstMark) {
    firstMark.scrollIntoView({ behavior: 'smooth', block: 'center' });
  }
}

async function init() {
  try {
    state.pages = await api('/api/pages');
  } catch (e) {
    $('#page').innerHTML = '<div class="error">Cannot load page index: ' + escapeHTML(e.message) + '</div>';
    return;
  }
  renderNav();
  if (!location.hash) location.hash = state.pages[0].slug;
  navigate();
  window.addEventListener('hashchange', navigate);

  let searchTimer = null;
  const searchInput = $('#search');
  searchInput.addEventListener('input', e => {
    const v = e.target.value;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(async () => {
      await primeSearch();
      runSearch(v);
    }, 150);
  });
  searchInput.addEventListener('keydown', e => {
    if (e.key === 'Escape') {
      searchInput.value = '';
      $('#search-results').classList.remove('open');
      $('#search-results').innerHTML = '';
      clearArticleHighlights();
    }
  });

  // Event delegation: a single click handler on the results container
  // covers all current and future hits without re-binding on each render.
  $('#search-results').addEventListener('click', ev => {
    const a = ev.target.closest('.search-hit');
    if (!a) return;
    ev.preventDefault();
    const slug = a.dataset.slug;
    const q = a.dataset.q;
    if (slug === state.current) {
      highlightInArticle(q);
    } else {
      // Stash the term so renderMarkdownPage / renderTechStack can apply
      // it after the new article is in the DOM.
      state.pendingHighlight = q;
      location.hash = slug;
    }
  });

  // A click outside the search area closes the dropdown without losing
  // the typed query — re-focusing the input re-opens it.
  document.addEventListener('click', ev => {
    if (ev.target.closest('.search-wrap')) return;
    $('#search-results').classList.remove('open');
  });
  searchInput.addEventListener('focus', () => {
    if (searchInput.value.trim()) $('#search-results').classList.add('open');
  });
}

init();
})();
