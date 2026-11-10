# 0064. Legacy template routes preserved

**Status:** Accepted.

The old Go HTML template routes are served under `/legacy` for backward compatibility. The SPA serves from `/` via a catch-all handler. This allows gradual migration without breaking existing bookmarks or monitoring that targets the old UI.
