# 0068. Run history merged from ALL destinations

**Status:** Accepted.

The UI data layer iterates through all destinations (not just first-available) and deduplicates runs by timestamp, preferring successful over failed runs only when timestamps match. This prevents hidden backups when one destination is offline and avoids the stale-success bug where an older successful run from destination B would mask a newer failure from destination A.
