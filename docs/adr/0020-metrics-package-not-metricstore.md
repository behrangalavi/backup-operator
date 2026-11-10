# 0020. `metrics` package, not `metricStore`

**Status:** Accepted.

Go convention is lowercase single-word package names. The rename also narrows `Register()` from `ctrlmetrics.RegistererGatherer` to `prometheus.Registerer`, decoupling the metrics layer from controller-runtime so it can be reused in non-operator contexts (e.g. a future standalone worker metrics endpoint).
