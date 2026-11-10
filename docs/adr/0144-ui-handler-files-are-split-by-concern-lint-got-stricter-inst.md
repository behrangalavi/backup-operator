# 0144. UI handler files are split by concern; lint got stricter instead of adding `nolint`

**Status:** Accepted.

`handlers_api.go` (1700 lines) became `handlers_trigger.go`, `handlers_dest_diag.go`, `handlers_dashboard.go`, `events.go`, `api_types.go`; `data.go` became `data.go` / `models.go` / `heatmap.go` — pure moves. The expanded golangci configuration (`gocognit ≤40`, `noctx`, `errorlint`, `bodyclose`, `prealloc`, `unconvert`, `misspell`) surfaced 13 functions above the complexity bar — `Pipeline.Run` at 92 was the worst — and each was split into named phases rather than excluded; the only knob turned was `errorlint`'s `errorf=false`, because the sanitisation ADRs deliberately break error chains with `%v` where a driver error would echo a connection string.
