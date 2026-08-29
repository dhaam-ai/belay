package backoff

import (
	"context"
	"math/rand"
	"time"
)

// Run calls op repeatedly according to p: on error, it consults
// p.ShouldRetry and, if told to continue, sleeps for p.NextDelay before
// trying again. It stops and returns nil as soon as op succeeds, and
// stops and returns a wrapped error as soon as p.ShouldRetry says not to
// continue.
//
// Every attempt, including the first, is recorded against ledger under
// name before op runs. ledger may be nil, meaning "don't track attempts."
//
// sleep is called instead of time.Sleep so tests can inject an
// instant, cancellable stand-in; production callers typically pass a
// thin wrapper around context-aware sleeping.
func Run(ctx context.Context, name string, p Policy, ledger *Ledger, rng *rand.Rand, sleep func(context.Context, time.Duration) error, op func(context.Context) error) error {
	start := time.Now()
	for attempt := 1; ; attempt++ {
		if ledger != nil {
			ledger.Record(name)
		}

		err := op(ctx)
		if err == nil {
			return nil
		}

		if !p.ShouldRetry(attempt, time.Since(start), err) {
			return wrapAttempt(attempt, err)
		}

		delay, derr := p.NextDelay(attempt, rng)
		if derr != nil {
			return derr
		}
		if serr := sleep(ctx, delay); serr != nil {
			return serr
		}
	}
}
