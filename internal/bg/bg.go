// Package bg tracks work that outlives the request that started it.
//
// It exists because a detached goroutine has no lifetime: a background `git fetch`
// kept writing into .git after the test that started it had finished, racing the
// removal of its own repository. Anything spawned to run "later" goes through here, so
// there is one place that can be waited on — by a test before it deletes its fixtures,
// or by a server on the way out so it doesn't leave a subprocess behind.
package bg

import (
	"sync"
	"time"
)

var group sync.WaitGroup

// Go runs fn in the background and counts it.
func Go(fn func()) {
	group.Add(1)
	go func() {
		defer group.Done()
		fn()
	}()
}

// Wait blocks until the background work is done, or until the cap passes — a hung
// subprocess must not turn into a hung shutdown.
func Wait() {
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(50 * time.Second):
	}
}
