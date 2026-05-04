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
  // After insertion, scroll to a heading if the URL had one.
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

function runSearch(q) {
  q = q.trim().toLowerCase();
  const nav = $('#nav');
  if (!q) { renderNav(); return; }
  const hits = [];
  for (const p of state.pages) {
    if (p.kind !== 'markdown') {
      if (p.title.toLowerCase().includes(q)) {
        hits.push({ slug: p.slug, title: p.title, snippet: '' });
      }
      continue;
    }
    const cached = state.pageCache.get(p.slug);
    if (!cached) continue;
    // strip HTML tags for the snippet match
    const plain = cached.html.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ');
    const idx = plain.toLowerCase().indexOf(q);
    if (idx >= 0) {
      const start = Math.max(0, idx - 40);
      const end = Math.min(plain.length, idx + q.length + 40);
      hits.push({
        slug: p.slug,
        title: p.title,
        snippet: (start > 0 ? '…' : '') + plain.slice(start, end) + (end < plain.length ? '…' : ''),
      });
    } else if (p.title.toLowerCase().includes(q)) {
      hits.push({ slug: p.slug, title: p.title, snippet: '' });
    }
  }
  if (hits.length === 0) {
    nav.innerHTML = '<div class="search-empty">No matches.</div>';
    return;
  }
  nav.innerHTML = hits.map(h => `
    <a href="#${escapeHTML(h.slug)}" class="search-hit">
      <div class="hit-title">${escapeHTML(h.title)}</div>
      ${h.snippet ? '<div class="hit-snippet">' + escapeHTML(h.snippet) + '</div>' : ''}
    </a>`).join('');
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
  $('#search').addEventListener('input', e => {
    const v = e.target.value;
    clearTimeout(searchTimer);
    searchTimer = setTimeout(async () => {
      await primeSearch();
      runSearch(v);
    }, 150);
  });
}

init();
})();
