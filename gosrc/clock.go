// A logical clock: every timestamp the forge stamps comes from here, never
// from the wall clock and never from randomness, so a run's state is
// deterministic and a replay sees the same bytes. The base is
// 2025-01-01T00:00:00Z; each call advances one second.

package main

import (
	"fmt"
	"sync"
	"time"
)

const clockBaseEpoch = 1735689600

var (
	clockMu   sync.Mutex
	clockTick int64
)

// tickNow returns the next logical second, as epoch seconds.
func tickNow() int64 {
	clockMu.Lock()
	defer clockMu.Unlock()
	now := clockBaseEpoch + clockTick
	clockTick++
	return now
}

// isoNow returns the next logical second, as an ISO-8601 UTC string.
// Format matches JS Date.toISOString(): "2025-01-01T00:00:00.000Z".
func isoNow() string {
	return time.Unix(tickNow(), 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// gitTimestamp renders an epoch second as "<epoch> +0000".
func gitTimestamp(epochSec int64) string {
	return fmt.Sprintf("%d +0000", epochSec)
}
