# 0114. events.k8s.io/v1 required fields on UI-emitted Events

**Status:** Accepted.

Setting `EventTime` on a `corev1.Event` makes the API server apply the events.k8s.io/v1 schema rules: `ReportingController`, `ReportingInstance`, and `Action` must all be non-empty, or Create is rejected with `Required value` and the UI's audit trail goes silent. Three UI emitters (destination/source mutations, ConfigMap settings, age-recipient changes) hit this and went unnoticed because the failure is logged at error level but never user-visible. Single `emitUIEvent` helper now fills all three fields once; `ReportingInstance` falls back to `os.Hostname()` (the pod name inside K8s). The worker uses `record.EventRecorder` and is unaffected — only the manually-built UI events were broken.
