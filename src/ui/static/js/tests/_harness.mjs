// Test harness for the SPA's classic (non-module) scripts. Loads core.js,
// charts.js and pages.js into a vm context with just enough of a DOM stub
// for their top-level statements to run, then returns the global function
// declarations (escJS, tr, sortBy, …) for the tests. app.js is deliberately
// excluded — it boots the app (fetch, EventSource) on load.
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import vm from 'node:vm';

const here = dirname(fileURLToPath(import.meta.url));
export const jsDir = join(here, '..');
export const staticDir = join(jsDir, '..');

function stubElement() {
  const el = {
    children: [], dataset: {}, style: {}, attributes: {},
    classList: { add() {}, remove() {}, toggle() { return false; }, contains() { return false; } },
    addEventListener() {}, removeEventListener() {}, appendChild(c) { this.children.push(c); return c; },
    remove() {}, setAttribute(k, v) { this.attributes[k] = v; }, getAttribute(k) { return this.attributes[k]; },
    removeAttribute(k) { delete this.attributes[k]; }, querySelector() { return null; }, querySelectorAll() { return []; },
    focus() {}, click() {}, contains() { return false; }, matches() { return false; }, closest() { return null; },
    textContent: '', innerHTML: '', value: '', hidden: false, offsetParent: null,
  };
  return el;
}

export function loadSPA() {
  const sandbox = {
    console,
    document: {
      querySelector: () => stubElement(), querySelectorAll: () => [], createElement: () => stubElement(),
      addEventListener() {}, documentElement: stubElement(), activeElement: null, hidden: false,
      getElementById: () => null, body: stubElement(),
    },
    localStorage: { getItem() { return null; }, setItem() {} },
    navigator: { language: 'en-US' },
    location: { hash: '' },
    performance: { now: () => Date.now() },
    requestAnimationFrame: fn => setTimeout(fn, 0),
    setTimeout, clearTimeout, setInterval, clearInterval,
    URL, URLSearchParams, AbortController, Date, Math, JSON, Number, String, Object, Array, Set, Map, Promise, Error, RegExp,
    encodeURIComponent, decodeURIComponent, isNaN, parseFloat, parseInt,
  };
  sandbox.window = sandbox;
  sandbox.globalThis = sandbox;
  const ctx = vm.createContext(sandbox);
  for (const f of ['core.js', 'charts.js', 'pages.js']) {
    vm.runInContext(readFileSync(join(jsDir, f), 'utf8'), ctx, { filename: f });
  }
  return ctx;
}

export function sources() {
  const files = ['core.js', 'charts.js', 'pages.js', 'app.js'].map(f => [f, readFileSync(join(jsDir, f), 'utf8')]);
  files.push(['index.html', readFileSync(join(staticDir, 'index.html'), 'utf8')]);
  return files;
}
