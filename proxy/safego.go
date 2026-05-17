package proxy

import (
	"kiro-go/logger"
	"runtime/debug"
)

// safeGo launches fn in a new goroutine and recovers any panic, logging the
// stack trace instead of crashing the entire process. Use for every
// long-running background goroutine the proxy starts (refresh ticker, stats
// saver, debounced lazy refresh, etc.). Without this, a panic in any
// background path takes the whole proxy down — net/http only protects the
// per-request goroutine.
//
// `name` is the goroutine label used in the panic log so operators can
// trace which background worker died.
func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[safeGo:%s] panic recovered: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn()
	}()
}

// safeGoArg is the single-argument variant of safeGo for goroutines that
// need a captured value. The captured value is passed by name so the
// goroutine cannot accidentally close over a loop variable.
func safeGoArg[T any](name string, arg T, fn func(T)) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[safeGo:%s] panic recovered: %v\n%s", name, r, debug.Stack())
			}
		}()
		fn(arg)
	}()
}
