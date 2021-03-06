package misc

import (
	"sync"
	"time"
)

/*
RateLimit tracks number of hits performed by each source ("actor") to determine whether a source has exceeded
specified rate limit. Instead of being a rolling counter, the tracking data is reset to empty at regular interval.
Remember to call Initialise() before use!
*/
type RateLimit struct {
	UnitSecs      int64
	MaxCount      int
	Logger        Logger
	lastTimestamp int64
	counter       map[string]int
	logged        map[string]struct{}
	counterMutex  *sync.Mutex
}

// Initialise rate limiter internal states.
func (limit *RateLimit) Initialise() {
	limit.counter = make(map[string]int)
	limit.counterMutex = new(sync.Mutex)
	if limit.UnitSecs < 1 || limit.MaxCount < 1 {
		limit.Logger.Panicf("Initialise", "RateLimit", nil, "UnitSecs and MaxCount must be greater than 0")
		return
	}
}

// Increase counter of the actor by one. If the counter exceeds max limit, return false, otherwise return true.
func (limit *RateLimit) Add(actor string, logIfLimitHit bool) bool {
	limit.counterMutex.Lock()
	// Reset all counters if unit of time has past
	if now := time.Now().Unix(); now-limit.lastTimestamp >= limit.UnitSecs {
		limit.counter = make(map[string]int)
		limit.logged = make(map[string]struct{})
		limit.lastTimestamp = now
	}
	if count, exists := limit.counter[actor]; exists {
		if count >= limit.MaxCount {
			if _, hasLogged := limit.logged[actor]; !hasLogged && logIfLimitHit {
				limit.Logger.Warningf("Add", "RateLimit", nil, "%s exceeded limit of %d hits per %d seconds", actor, limit.MaxCount, limit.UnitSecs)
				limit.logged[actor] = struct{}{}
			}
			limit.counterMutex.Unlock()
			return false
		} else {
			limit.counter[actor] = count + 1
		}
	} else {
		limit.counter[actor] = 1
	}
	limit.counterMutex.Unlock()
	return true
}
