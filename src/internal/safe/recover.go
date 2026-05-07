// Package safe provides small primitives for keeping long-lived processes
// resilient to programmer errors in goroutines.
package safe

import (
	"fmt"
	"runtime/debug"

	"github.com/go-logr/logr"
)

// Goroutine recovers from a panic in the calling goroutine and logs it with
// the supplied phase and key (e.g. destination name) plus the captured stack.
// Use as `defer safe.Goroutine(log, "phase", "key")` at the top of every
// goroutine in a long-lived process — without it, a single panic crashes
// the whole pod.
//
// The stack trace is intentional: a panic without a stack is nearly
// undebuggable in production. Logging at Error level surfaces it to the
// operator's normal log pipeline.
func Goroutine(log logr.Logger, phase, key string) {
	if r := recover(); r != nil {
		log.Error(fmt.Errorf("panic: %v", r), "goroutine recovered",
			"phase", phase, "key", key,
			"stack", string(debug.Stack()))
	}
}
