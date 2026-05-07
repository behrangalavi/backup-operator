# UI translations

The SPA reads dictionaries from `/static/i18n/<code>.json`. The Go binary
embeds the whole `static/` tree at build time (`//go:embed static` in
`ui/server.go`), so any JSON file dropped here ships with the next build.

## Adding a language

1. Copy `en.json` to `<code>.json` (e.g. `it.json`, `es.json`, `pt.json`).
2. Translate values in place. Keep the key shape identical — missing keys
   fall back to English, then to the raw key, so partial dictionaries are
   safe to ship.
3. Register the code in `static/app.js` → `availableLangs`.
4. Set `lang.name` to the language's native form (e.g. `"Italiano"`,
   `"Español"`); that is what the picker displays.

That is the entire contract — no backend code changes.

## Translating new strings

Visible strings live in `static/app.js` (page renderers) and
`static/index.html` (sidebar / shell).

- Page renderers: replace the literal with `${tr('namespace.key')}` and
  add the key to every `<code>.json`. The named export is `tr` (not `t`)
  because `t` is already used for `target` in dozens of lambdas.
- Static shell (sidebar, footer): use `<span data-i18n="namespace.key">`
  with the English fallback as inner text. `applyStaticTranslations()`
  re-applies after every language change.
- Tooltips: `<element data-i18n-title="namespace.key" title="…">`.
- Variables: `tr('toast.exportFail', {error: e.message})` — `{error}` in
  the dictionary value is replaced.

## Fallback rules

Lookup order: current language → English → key string itself. A missing
translation is therefore *visible* (you see `nav.dashboard` instead of
silent emptiness), which is what we want during development.
