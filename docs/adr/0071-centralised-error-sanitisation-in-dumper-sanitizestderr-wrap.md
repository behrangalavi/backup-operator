# 0071. Centralised error sanitisation in `dumper.SanitizeStderr` / `WrapExecError`

**Status:** Accepted.

Stderr from exec'd dump tools and error strings from in-process drivers are scrubbed before they enter `fmt.Errorf` chains, logs, Kubernetes Events, or UI responses. The scrubber masks: any literal value passed in `secrets...` (typically the password), `scheme://user:pass@host` URI patterns, and `password=`/`passwd=`/`pwd=`/`auth=` key-value pairs. `SanitizeError(prefix, err, secrets...)` deliberately does NOT preserve `errors.Is` chains for driver errors that may echo the connection string back — preserving the chain would also preserve the leak. Adding a new dumper without using these helpers is a review-time blocker.
