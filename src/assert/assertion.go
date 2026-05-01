package assert

import (
	"fmt"
	"os"
	"runtime"
)

func Assert(condition bool, messages ...any) {
	if !condition {
		fmt.Fprintf(os.Stderr, "== ASSERTION FAILURE ==\n")

		if len(messages) > 0 {
			fmt.Fprintf(os.Stderr, "Messages:\n")
			for _, message := range messages {
				fmt.Fprintf(os.Stderr, "  -> %v\n", tryStringify(message))
			}
		}

		stack := make([]uintptr, 5)
		length := runtime.Callers(2, stack)
		frames := runtime.CallersFrames(stack[:length])
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "Location:\n")
		for {
			frame, more := frames.Next()
			fmt.Fprintf(os.Stderr, "%s\n", frame.Function)
			fmt.Fprintf(os.Stderr, "    %s:%d\n", frame.File, frame.Line)
			if !more {
				break
			}
			if frame.Function == "main.main" {
				break
			}
		}

		os.Exit(1)
	}
}

func tryStringify(data any) any {
	err, ok := data.(error)
	if ok {
		return err
	}
	stringer, ok := data.(fmt.Stringer)
	if ok {
		return stringer.String()
	}
	return data
}
