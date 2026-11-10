# 0016. No notifier built-in

**Status:** Accepted.

The cluster already has Alertmanager. Building a Slack/Email notifier would re-invent routing, deduplication, and silencing. Shipping `PrometheusRule` defaults instead is the right interface.
