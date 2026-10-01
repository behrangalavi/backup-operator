import { test } from 'node:test';
import assert from 'node:assert/strict';
import { loadSPA } from './_harness.mjs';

const g = loadSPA();

test('sortBy keeps nulls last regardless of direction', () => {
  const rows = [{ v: 2 }, { v: null }, { v: 1 }, { v: 3 }];
  assert.deepEqual(g.sortBy(rows, r => r.v, 'asc').map(r => r.v), [1, 2, 3, null]);
  assert.deepEqual(g.sortBy(rows, r => r.v, 'desc').map(r => r.v), [3, 2, 1, null]);
  assert.deepEqual(rows.map(r => r.v), [2, null, 1, 3], 'input is not mutated');
});

test('sortBy compares strings with localeCompare', () => {
  const rows = ['b', 'a', 'c'].map(v => ({ v }));
  assert.deepEqual(g.sortBy(rows, r => r.v, 'asc').map(r => r.v), ['a', 'b', 'c']);
});

test('parseTsCompact parses the storage timestamp and falls back to Date.parse', () => {
  assert.equal(g.parseTsCompact('20260501T020000Z'), Date.UTC(2026, 4, 1, 2, 0, 0));
  assert.equal(g.parseTsCompact('2026-05-01T02:00:00Z'), Date.UTC(2026, 4, 1, 2, 0, 0));
  assert.equal(g.parseTsCompact('garbage'), null);
  assert.equal(g.parseTsCompact(''), null);
});

test('fmtCompact abbreviates large numbers', () => {
  assert.equal(g.fmtCompact(999), '999');
  assert.equal(g.fmtCompact(1500), '1.5K');
  assert.equal(g.fmtCompact(2_500_000), '2.5M');
  assert.equal(g.fmtCompact(3_000_000_000), '3.0B');
  assert.equal(g.fmtCompact(null), '—');
});

test('humanBytes picks binary units', () => {
  assert.equal(g.humanBytes(0), '0 B');
  assert.equal(g.humanBytes(1536), '1.5 KiB');
  assert.equal(g.humanBytes(5 * 1024 * 1024), '5.0 MiB');
});
