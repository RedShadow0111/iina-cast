package main

import (
	"os"
	"time"
)

// WatchParent polls rather than using kqueue. It fires when the watched pid
// is gone, and also when this process is adopted by launchd: IINA's quit
// does not always reap the helper, and a helper left running keeps the
// television playing.
func WatchParent(pid int, onGone func()) {
	born := os.Getppid()
	go func() {
		for {
			if !processAlive(pid) || (born != 1 && os.Getppid() == 1) {
				onGone()
				return
			}
			time.Sleep(500 * time.Millisecond)
		}
	}()
}
