package bridgecontrol

import (
	"math/rand"
	"time"
)

// backoffBase/backoffMax/backoffMin are package-level vars (not consts) so
// tests can shrink them to keep reconnect tests fast without changing the
// production defaults' meaning.
var (
	backoffBase = 1 * time.Second
	backoffMax  = 60 * time.Second
	backoffMin  = 250 * time.Millisecond
)

// backoffDelay returns a bounded, jittered delay for reconnect attempt N
// (1-indexed). It uses "full jitter" (delay uniformly sampled from
// [0, min(backoffMax, backoffBase*2^(attempt-1))]): bounded so a flapping
// control endpoint never produces a busy loop or an unbounded wait, and
// jittered so many bridgectl installations reconnecting after a shared
// outage do not thunder-herd Bridge at the same instant.
func backoffDelay(attempt int, rng *rand.Rand) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := attempt - 1
	if shift > 6 { // 2^6 * 1s = 64s already exceeds backoffMax; avoid overflow beyond that.
		shift = 6
	}
	exp := backoffBase * time.Duration(int64(1)<<uint(shift))
	if exp > backoffMax {
		exp = backoffMax
	}
	d := time.Duration(rng.Int63n(int64(exp) + 1))
	if d < backoffMin {
		d = backoffMin
	}
	return d
}
