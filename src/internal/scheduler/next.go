package scheduler

import (
	"time"

	"github.com/robfig/cron/v3"
)

// standardParser parses 5-field cron expressions ("M H DoM Mo DoW") plus the
// @daily-style descriptors — exactly what Kubernetes CronJobs accept. The
// default robfig parser adds a leading seconds field that would reject every
// valid CronJob schedule, so the parser is built explicitly.
var standardParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// ValidateSchedule reports whether Kubernetes would accept the expression.
func ValidateSchedule(schedule string) error {
	_, err := standardParser.Parse(schedule)
	return err
}

// NextRun returns the first fire time strictly after `after`, or false when
// the expression is invalid or never fires. UTC in, UTC out.
func NextRun(schedule string, after time.Time) (time.Time, bool) {
	if schedule == "" {
		return time.Time{}, false
	}
	sched, err := standardParser.Parse(schedule)
	if err != nil {
		return time.Time{}, false
	}
	next := sched.Next(after)
	if next.IsZero() {
		return time.Time{}, false
	}
	return next, true
}
