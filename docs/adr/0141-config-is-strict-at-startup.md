# 0141. Config is strict at startup

**Status:** Accepted.

Shared validators and default constants (`config/validate.go`) replace the per-binary copies; validation errors name the key; `GetInt`/`GetInt64` fail fast instead of returning 0 for garbage; `DEFAULT_SCHEDULE` is parsed with the CronJob parser before the manager starts; the eight `WORKER_*` scheduling knobs are strict JSON (unknown fields fail startup) because a silently ignored toleration is exactly the kind of misconfiguration that only shows up when a node pool drains.
