# 0057. Vanilla JS SPA over React/Vue

**Status:** Accepted.

The UI is embedded via `go:embed` into the operator binary. A framework would require a Node.js build step, `node_modules`, and a bundler, adding complexity to CI and the Dockerfile. Vanilla JS with hash-based routing keeps the binary self-contained and the frontend instantly deployable without build tooling.
