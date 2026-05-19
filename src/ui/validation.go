package ui

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// --- Input validation helpers ---

var k8sNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)

func validateK8sName(name string) string {
	if len(name) > 253 {
		return "name must be at most 253 characters"
	}
	if !k8sNameRe.MatchString(name) {
		return "name must consist of lowercase alphanumeric characters, '-' or '.', and start/end with alphanumeric"
	}
	return ""
}

func validatePort(port string) string {
	p, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil {
		return "port must be a number"
	}
	if p < 1 || p > 65535 {
		return "port must be between 1 and 65535"
	}
	return ""
}

// isSupportedDBType is the single allow-list checked at the API edge. The
// dumper factory is the only other place that knows about DB types; the UI
// must stay in sync with it.
func isSupportedDBType(t string) bool {
	switch t {
	case "postgres", "mysql", "mariadb", "mongo", "redis":
		return true
	}
	return false
}

// validateCronSchedule validates a cron expression using the same 5-field
// parser Kubernetes CronJobs use. Catches field-count errors AND invalid
// values (e.g. minute=99) that the old len-only check missed.
func validateCronSchedule(schedule string) string {
	if _, err := cronParser.Parse(schedule); err != nil {
		return fmt.Sprintf("invalid cron schedule: %v", err)
	}
	return ""
}
