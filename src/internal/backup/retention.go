package backup

import (
	"context"
	"path"
	"sort"
	"strings"
	"time"

	"backup-operator/internal/secrets"
	"backup-operator/metricStore"
	"backup-operator/storage"
	storageFactory "backup-operator/storage/factory"

	"github.com/go-logr/logr"
)

// RetentionPolicy defines the effective retention rules for one run. The
// resolver in the pipeline turns annotation values + global defaults into a
// concrete RetentionPolicy before invoking applyRetention.
type RetentionPolicy struct {
	Days    int // 0 = disabled (keep forever)
	MinKeep int // floor: never delete below this many most-recent dumps
}

// Disabled returns true when the policy should not delete anything.
func (p RetentionPolicy) Disabled() bool { return p.Days <= 0 }

// applyRetention enforces the policy against every destination of the source.
// Errors against one destination do NOT abort the others — we record them via
// metrics and move on. The backup run's success status is unaffected by
// retention failures: deleting old dumps is best-effort.
func (p *Pipeline) applyRetention(
	ctx context.Context,
	dests []*secrets.Destination,
	target string,
	policy RetentionPolicy,
	now time.Time,
	log logr.Logger,
) {
	if policy.Disabled() {
		log.V(1).Info("retention disabled", "target", target)
		return
	}

	for _, dest := range dests {
		st, err := storageFactory.NewStorage(dest.StorageType, dest.Name, dest.Data, log)
		if err != nil {
			log.Error(err, "retention: init storage", "destination", dest.Name)
			metricStore.IncRetentionFailure(target, dest.Name)
			continue
		}
		objs, err := st.List(ctx, target+"/")
		if err != nil {
			log.Error(err, "retention: list", "destination", dest.Name)
			metricStore.IncRetentionFailure(target, dest.Name)
			continue
		}

		victims := selectForDeletion(objs, policy, now)
		if len(victims) == 0 {
			continue
		}
		log.Info("retention deleting",
			"destination", dest.Name,
			"target", target,
			"count", len(victims),
			"policy_days", policy.Days,
			"min_keep", policy.MinKeep,
		)

		for _, v := range victims {
			if err := st.Delete(ctx, v); err != nil {
				log.Error(err, "retention: delete", "destination", dest.Name, "path", v)
				metricStore.IncRetentionFailure(target, dest.Name)
				continue
			}
			metricStore.IncRetentionDeleted(target, dest.Name, classifyKind(v))
		}
	}
}

// selectForDeletion is the pure decision function — no I/O, fully testable.
// Algorithm:
//   1. Group objects by timestamp (one dump file + one meta file per timestamp).
//   2. Sort timestamps newest-first.
//   3. Always keep the MinKeep newest timestamps (safety floor).
//   4. From the rest, mark for deletion any timestamp older than Days.
//   5. Return the list of paths to delete (dump + meta together).
func selectForDeletion(objs []storage.Object, policy RetentionPolicy, now time.Time) []string {
	if policy.Disabled() {
		return nil
	}

	groups := groupByTimestamp(objs)
	if len(groups) == 0 {
		return nil
	}

	stamps := make([]string, 0, len(groups))
	for ts := range groups {
		stamps = append(stamps, ts)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(stamps))) // ISO timestamps sort lexically

	cutoff := now.Add(-time.Duration(policy.Days) * 24 * time.Hour)

	var victims []string
	for i, ts := range stamps {
		if i < policy.MinKeep {
			continue // safety floor — never delete the N most recent
		}
		t, err := time.Parse(timestampLayout, ts)
		if err != nil {
			continue // unparseable name; leave it alone
		}
		if t.Before(cutoff) {
			victims = append(victims, groups[ts]...)
		}
	}
	return victims
}

// timestampLayout matches the suffix written by buildObjectPath in pipeline.go
// — keep these two in sync.
const timestampLayout = "20060102T150405Z"

// groupByTimestamp buckets dump and meta files by their shared timestamp,
// recognised from the basename pattern dump-<timestamp>.<ext>.
func groupByTimestamp(objs []storage.Object) map[string][]string {
	out := make(map[string][]string)
	for _, o := range objs {
		base := path.Base(o.Path)
		if !strings.HasPrefix(base, "dump-") {
			continue
		}
		stripped := strings.TrimPrefix(base, "dump-")
		// Filename pattern: <timestamp>.<ext...>
		// Take everything before the first '.' as the timestamp.
		dot := strings.Index(stripped, ".")
		if dot <= 0 {
			continue
		}
		ts := stripped[:dot]
		out[ts] = append(out[ts], o.Path)
	}
	return out
}

// classifyKind labels metric counts by what was actually deleted, so a
// dashboard can distinguish between "dropped a dump" and "dropped a meta".
func classifyKind(p string) string {
	if strings.HasSuffix(p, ".meta.json") {
		return "meta"
	}
	if strings.HasSuffix(p, ".sql.gz.age") {
		return "dump"
	}
	return "other"
}
