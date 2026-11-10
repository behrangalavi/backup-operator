# 0062. Manual trigger creates Job from CronJob spec

**Status:** Accepted.

`POST /api/trigger/{target}` reads the existing CronJob's pod template and creates a one-off Job. This guarantees the manual run uses the exact same image, env vars, and mounts as the scheduled run. No separate template or spec duplication needed.
