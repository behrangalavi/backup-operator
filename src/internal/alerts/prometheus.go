package alerts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// PrometheusProvider queries Prometheus's HTTP alerts API. We deliberately use
// net/http instead of pulling in the prometheus/client_golang query client —
// the API surface we need is one endpoint, two fields, and we already keep
// the binary lean.
type PrometheusProvider struct {
	URL    string // e.g. http://prometheus-operated.alert.svc:9090
	HTTP   *http.Client
	Prefix string // alertname prefix to keep ("Backup" by default)
}

// NewPrometheusProvider returns a provider with sane defaults; HTTP timeout
// is short because the UI calls this on every page load and we'd rather show
// a friendly degraded state than hang the page.
//
// We trim whitespace AND trailing slashes from the input. A space in the URL
// (easy to introduce via Helm --set or copy-paste) would otherwise be
// preserved by net/url and fail the request with a confusing "invalid
// character" error far from the misconfiguration source.
func NewPrometheusProvider(promURL string) *PrometheusProvider {
	return &PrometheusProvider{
		URL:    strings.TrimRight(strings.TrimSpace(promURL), "/"),
		HTTP:   &http.Client{Timeout: 5 * time.Second},
		Prefix: "Backup",
	}
}

type promAlertsResponse struct {
	Status string `json:"status"`
	Data   struct {
		Alerts []promAlert `json:"alerts"`
	} `json:"data"`
	ErrorType string `json:"errorType,omitempty"`
	Error     string `json:"error,omitempty"`
}

type promAlert struct {
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	State       string            `json:"state"`
	ActiveAt    time.Time         `json:"activeAt"`
}

func (p *PrometheusProvider) List(ctx context.Context) ([]Alert, error) {
	endpoint := p.URL + "/api/v1/alerts"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", redactURL(endpoint), err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %s", resp.Status)
	}

	var body promAlertsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode prometheus response: %w", err)
	}
	if body.Status != "success" {
		return nil, fmt.Errorf("prometheus error: %s: %s", body.ErrorType, body.Error)
	}

	out := make([]Alert, 0, len(body.Data.Alerts))
	for _, a := range body.Data.Alerts {
		name := a.Labels["alertname"]
		if !strings.HasPrefix(name, p.Prefix) {
			continue
		}
		out = append(out, Alert{
			Alertname:   name,
			Target:      a.Labels["target"],
			Destination: a.Labels["destination"],
			Severity:    a.Labels["severity"],
			State:       a.State,
			ActiveSince: a.ActiveAt,
			Summary:     a.Annotations["summary"],
			Source:      "prometheus",
		})
	}
	return out, nil
}

// redactURL strips userinfo from a URL so accidental basic-auth credentials
// in PROMETHEUS_URL do not leak into error messages or logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User(u.User.Username()) // drop password
	return u.String()
}
