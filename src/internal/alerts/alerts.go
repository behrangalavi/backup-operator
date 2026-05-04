// Package alerts surfaces backup-related Prometheus alerts to the UI.
//
// Two providers are supported:
//
//   - PrometheusProvider queries `/api/v1/alerts` on the Prometheus server
//     and filters for the alertnames our chart's PrometheusRule defines.
//     This is the canonical view: it respects the "for:" duration, label
//     overrides, and any custom rules the operator has added.
//
//   - LocalProvider re-evaluates the same six rule conditions on the
//     operator's own gathered metric state. It exists so the UI shows
//     useful information even when Prometheus is not yet configured —
//     no setup, but no "for:" debounce either.
//
// Both produce the same Alert shape so the UI does not care which one ran.
package alerts

import (
	"context"
	"time"
)

// Alert is the unified shape exposed via /api/alerts. Source records which
// provider produced it so the UI can show "(local heuristic)" vs "(from
// Prometheus)" subtly — the two are not interchangeable for compliance use.
type Alert struct {
	Alertname   string    `json:"alertname"`
	Target      string    `json:"target,omitempty"`
	Destination string    `json:"destination,omitempty"`
	Severity    string    `json:"severity"`
	State       string    `json:"state"` // "firing" | "pending"
	ActiveSince time.Time `json:"activeSince,omitempty"`
	Summary     string    `json:"summary"`
	Source      string    `json:"source"` // "prometheus" | "local"
}

// Provider is the alert source. Implementations must be safe for concurrent
// List calls — the UI handler may invoke this from multiple HTTP goroutines.
type Provider interface {
	List(ctx context.Context) ([]Alert, error)
}
