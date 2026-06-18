package safe

import (
	"fmt"
	"os"
	"runtime/debug"
)

// Go runs a goroutine with panic recovery to prevent program crashes.
func Go(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Fprintf(os.Stderr, "\n[CRITICAL PANIC IN GOROUTINE] %v\nStack trace:\n%s\n", r, debug.Stack())
			}
		}()
		fn()
	}()
}
