// Package scheduler holds the operator-side schedule transformations
// applied between the user's annotation value and the materialised
// CronJob.Spec.Schedule. Currently: per-source minute jitter to avoid
// fleet-wide thundering-herd on the storage backend when many sources
// share the same `0 H * * *` pattern.
package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"
)

// JitterMinutesUnset is the sentinel value parsers use when the
// `backup.mogenius.io/jitter-minutes` annotation is absent. It selects
// the conservative default behaviour: jitter only when the user wrote
// the schedule with minute=="0" (the canonical "default" form), leave
// non-zero literal minutes untouched as a respect for explicit user
// intent.
const JitterMinutesUnset = -1

// defaultJitterWindow is the spread applied when the annotation is
// absent. 60 = full hour. Picked because the most common user input
// is `0 H * * *`, where spreading inside the hour preserves the user's
// hour intent.
const defaultJitterWindow = 60

// ApplyJitter transforms a cron expression by replacing its minute
// field with a deterministic per-source offset. The decision tree:
//
//	jitterMinutes == 0          -> schedule returned unchanged (opt-out)
//	jitterMinutes == Unset      -> jitter ONLY if minute == "0",
//	                               window = defaultJitterWindow (60)
//	jitterMinutes > 0 (set)     -> jitter even on non-zero literal minute
//	                               (user opt-in to fleet-spreading),
//	                               but multi-fire schedules are still
//	                               skipped — `*/15`, `0,30`, `0-30`, `*`
//	                               would change semantics if rewritten.
//
// The offset is hash(sourceName) % window, which gives stable spread
// across reconciles AND a uniform distribution across the fleet — a
// rolling counter would couple to creation order and concentrate on
// the early minutes.
//
// Schedules with anything other than 5 space-separated fields are
// returned untouched: an invalid expression should fail loudly via
// the K8s CronJob validator, not be silently "fixed" by us.
func ApplyJitter(schedule string, jitterMinutes int, sourceName string) string {
	if jitterMinutes == 0 {
		return schedule // explicit opt-out
	}

	fields := strings.Fields(schedule)
	if len(fields) != 5 {
		return schedule // invalid; let K8s reject it
	}
	minute := fields[0]

	// Multi-fire schedules express "fire N times within the hour" or
	// "every N minutes" intents. Replacing the minute field would
	// reduce them to one fire per hour — a semantic change, never the
	// user's intent.
	if strings.ContainsAny(minute, ",-/*") {
		return schedule
	}

	annotationAbsent := jitterMinutes == JitterMinutesUnset
	if annotationAbsent {
		// Conservative default: only jitter the canonical "0" form.
		// Any other literal minute (e.g. "15") is treated as deliberate.
		if minute != "0" {
			return schedule
		}
		jitterMinutes = defaultJitterWindow
	}

	// Cap the window at 60. A larger value would wrap the hour
	// boundary and shift the user's intended hour, which is not what
	// fleet spreading is for.
	if jitterMinutes < 0 || jitterMinutes > defaultJitterWindow {
		jitterMinutes = defaultJitterWindow
	}

	offset := minuteOffset(sourceName, jitterMinutes)
	fields[0] = strconv.Itoa(offset)
	return strings.Join(fields, " ")
}

// minuteOffset returns a deterministic offset in [0, window) for the
// given source name. SHA-256 is overkill for hash quality but matches
// the rest of the codebase's hashing choice (storage_pool, anonymize,
// SHA256 in meta) — one less primitive to reason about.
func minuteOffset(sourceName string, window int) int {
	if window <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(sourceName))
	// Take the first 4 bytes as an unsigned int. The mask drops the
	// sign bit so the int conversion never produces a negative value
	// on 32-bit platforms.
	n := int(binary.BigEndian.Uint32(sum[:4]) & 0x7fffffff)
	return n % window
}
