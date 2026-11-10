# 0075. Helm chart sets `release: kube-prometheus-stack` by default

**Status:** Accepted.

kube-prometheus-stack's Prometheus selects ServiceMonitors and PrometheusRules via `matchLabels: { release: kube-prometheus-stack }`. Without this label, our chart-shipped CRDs would render but never be scraped — exactly the failure mode that hid the project's semantic alerts in early enterprise pilots. The single top-level value `prometheusReleaseLabel` (default `kube-prometheus-stack`) applies the label to both objects; set to `""` to opt out and use `serviceMonitor.labels` / `prometheusRule.labels` manually instead.
