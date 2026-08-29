package backoff

import (
	"context"
	"sync"
	"time"
)

// Ledger counts retry attempts per operation name. It is safe for
// concurrent use by multiple goroutines.
type Ledger struct {
	mu       sync.Mutex
	attempts map[string]int
	total    int
}

// NewLedger returns an empty Ledger ready for use.
func NewLedger() *Ledger {
	return &Ledger{attempts: make(map[string]int)}
}

// Record increments the attempt counter for op (if op is non-empty) and
// always increments the overall total, even when op is empty. It returns
// op's new count, or 0 if op is empty.
func (l *Ledger) Record(op string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total++
	if op == "" {
		return 0
	}
	l.attempts[op]++
	return l.attempts[op]
}

// Count returns the number of attempts recorded for op.
func (l *Ledger) Count(op string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attempts[op]
}

// Total returns the number of attempts recorded across all operations,
// including anonymous ones recorded with an empty op name.
func (l *Ledger) Total() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.total
}

// Reset clears every recorded attempt.
func (l *Ledger) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts = make(map[string]int)
	l.total = 0
}

// ClassifyAndCount records an attempt against ledger under name, then
// reports whether err is retryable. It always records, even when err is
// nil, so callers can see how often an operation was attempted with no
// error at all.
func ClassifyAndCount(ledger *Ledger, name string, err error) bool {
	ledger.Record(name)
	if err == nil {
		return false
	}
	return IsRetryable(err)
}

// WatchTotal polls ledger every interval until its total reaches limit or
// ctx is cancelled, then closes the returned channel.
func WatchTotal(ctx context.Context, ledger *Ledger, limit int, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ledger.Total() >= limit {
					return
				}
			}
		}
	}()
	return done
}

// WaitForEach blocks until every name in names has been recorded at least
// once in ledger, checking each one in turn against its own timeout. It
// returns the subset of names that never reached the ledger before their
// timeout elapsed, in the order they timed out.
//
// Each name's timer and ticker are stopped as soon as that name is
// resolved, before WaitForEach moves on to the next one: this function
// processes names one at a time, so holding a whole name's resources
// open until the very end (via a deferred Stop) would pin one timer and
// one ticker per name for as long as the rest of the list takes to
// process.
func WaitForEach(ledger *Ledger, names []string, timeout, pollEvery time.Duration) []string {
	var timedOut []string
	for _, name := range names {
		timer := time.NewTimer(timeout)
		ticker := time.NewTicker(pollEvery)
	waitOne:
		for {
			select {
			case <-timer.C:
				timedOut = append(timedOut, name)
				break waitOne
			case <-ticker.C:
				if ledger.Count(name) > 0 {
					break waitOne
				}
			}
		}
		timer.Stop()
		ticker.Stop()
	}
	return timedOut
}
