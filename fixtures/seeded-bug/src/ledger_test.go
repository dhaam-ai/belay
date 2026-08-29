package backoff

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLedger_Record(t *testing.T) {
	t.Run("named_op_increments_its_own_counter", func(t *testing.T) {
		l := NewLedger()
		if got := l.Record("fetch"); got != 1 {
			t.Fatalf("first Record(\"fetch\") = %d, want 1", got)
		}
		if got := l.Record("fetch"); got != 2 {
			t.Fatalf("second Record(\"fetch\") = %d, want 2", got)
		}
		if got := l.Count("fetch"); got != 2 {
			t.Fatalf("Count(\"fetch\") = %d, want 2", got)
		}
	})

	t.Run("different_ops_are_counted_independently", func(t *testing.T) {
		l := NewLedger()
		l.Record("fetch")
		l.Record("fetch")
		l.Record("write")
		if got := l.Count("fetch"); got != 2 {
			t.Errorf("Count(\"fetch\") = %d, want 2", got)
		}
		if got := l.Count("write"); got != 1 {
			t.Errorf("Count(\"write\") = %d, want 1", got)
		}
	})

	t.Run("empty_op_is_not_recorded_per_name", func(t *testing.T) {
		l := NewLedger()
		if got := l.Record(""); got != 0 {
			t.Fatalf("Record(\"\") = %d, want 0", got)
		}
		if got := l.Count(""); got != 0 {
			t.Fatalf("Count(\"\") = %d, want 0", got)
		}
	})

	t.Run("empty_op_still_increments_the_overall_total", func(t *testing.T) {
		l := NewLedger()
		l.Record("")
		l.Record("")
		l.Record("")
		if got := l.Total(); got != 3 {
			t.Fatalf("Total() after 3 empty-op Records = %d, want 3", got)
		}
	})

	t.Run("named_and_empty_ops_both_count_toward_total", func(t *testing.T) {
		l := NewLedger()
		l.Record("fetch")
		l.Record("")
		l.Record("fetch")
		if got := l.Total(); got != 3 {
			t.Fatalf("Total() = %d, want 3", got)
		}
	})
}

func TestLedger_Count(t *testing.T) {
	t.Run("unknown_op_is_zero", func(t *testing.T) {
		l := NewLedger()
		if got := l.Count("never-seen"); got != 0 {
			t.Fatalf("Count(\"never-seen\") = %d, want 0", got)
		}
	})
}

func TestLedger_Total(t *testing.T) {
	t.Run("empty_ledger_is_zero", func(t *testing.T) {
		if got := NewLedger().Total(); got != 0 {
			t.Fatalf("Total() on an empty ledger = %d, want 0", got)
		}
	})
}

func TestLedger_Reset(t *testing.T) {
	t.Run("clears_all_recorded_attempts", func(t *testing.T) {
		l := NewLedger()
		l.Record("fetch")
		l.Record("fetch")
		l.Record("write")
		l.Reset()
		if got := l.Total(); got != 0 {
			t.Fatalf("Total() after Reset() = %d, want 0", got)
		}
		if got := l.Count("fetch"); got != 0 {
			t.Fatalf("Count(\"fetch\") after Reset() = %d, want 0", got)
		}
	})

	t.Run("ledger_is_usable_after_reset", func(t *testing.T) {
		l := NewLedger()
		l.Record("fetch")
		l.Reset()
		if got := l.Record("fetch"); got != 1 {
			t.Fatalf("Record(\"fetch\") after Reset() = %d, want 1", got)
		}
	})
}

func TestLedger_ConcurrentRecord(t *testing.T) {
	t.Run("many_goroutines_recording_the_same_op_produce_an_exact_count", func(t *testing.T) {
		const goroutines = 50
		const perGoroutine = 200
		l := NewLedger()
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < perGoroutine; j++ {
					l.Record("op")
				}
			}()
		}
		wg.Wait()

		want := goroutines * perGoroutine
		if got := l.Count("op"); got != want {
			t.Errorf("Count(\"op\") = %d, want %d", got, want)
		}
		if got := l.Total(); got != want {
			t.Errorf("Total() = %d, want %d", got, want)
		}
	})
}

func TestLedger_ConcurrentTotal(t *testing.T) {
	t.Run("concurrent_reads_and_writes_are_safe", func(t *testing.T) {
		const writers = 20
		const readers = 20
		const perWriter = 200
		l := NewLedger()

		var wg sync.WaitGroup
		wg.Add(writers + readers)
		for i := 0; i < writers; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < perWriter; j++ {
					l.Record("op")
				}
			}()
		}
		for i := 0; i < readers; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < perWriter; j++ {
					_ = l.Total()
				}
			}()
		}
		wg.Wait()

		if got := l.Total(); got != writers*perWriter {
			t.Errorf("Total() = %d, want %d", got, writers*perWriter)
		}
	})
}

func TestLedger_ConcurrentReset(t *testing.T) {
	t.Run("reset_during_concurrent_recording_does_not_corrupt_the_ledger", func(t *testing.T) {
		const goroutines = 20
		const perGoroutine = 200
		l := NewLedger()

		var wg sync.WaitGroup
		wg.Add(goroutines + 1)
		for i := 0; i < goroutines; i++ {
			go func() {
				defer wg.Done()
				for j := 0; j < perGoroutine; j++ {
					l.Record("op")
				}
			}()
		}
		go func() {
			defer wg.Done()
			time.Sleep(time.Millisecond)
			l.Reset()
		}()
		wg.Wait()

		if got := l.Total(); got < 0 {
			t.Errorf("Total() after concurrent Reset = %d, want >= 0", got)
		}
	})
}

func TestClassifyAndCount(t *testing.T) {
	t.Run("records_even_when_error_is_nil", func(t *testing.T) {
		l := NewLedger()
		if got := ClassifyAndCount(l, "op", nil); got {
			t.Error("ClassifyAndCount(l, \"op\", nil) = true, want false")
		}
		if got := l.Count("op"); got != 1 {
			t.Fatalf("Count(\"op\") after ClassifyAndCount with nil error = %d, want 1", got)
		}
	})

	t.Run("records_and_reports_retryable_errors", func(t *testing.T) {
		l := NewLedger()
		if got := ClassifyAndCount(l, "op", ErrTransient); !got {
			t.Error("ClassifyAndCount(l, \"op\", ErrTransient) = false, want true")
		}
		if got := l.Count("op"); got != 1 {
			t.Fatalf("Count(\"op\") = %d, want 1", got)
		}
	})

	t.Run("records_and_reports_non_retryable_errors", func(t *testing.T) {
		l := NewLedger()
		if got := ClassifyAndCount(l, "op", errors.New("nope")); got {
			t.Error("ClassifyAndCount(l, \"op\", plain error) = true, want false")
		}
		if got := l.Count("op"); got != 1 {
			t.Fatalf("Count(\"op\") = %d, want 1", got)
		}
	})
}

func TestWaitForEach(t *testing.T) {
	const timeout = 20 * time.Millisecond
	const pollEvery = time.Millisecond

	t.Run("returns_empty_when_all_names_are_already_recorded", func(t *testing.T) {
		l := NewLedger()
		l.Record("a")
		l.Record("b")

		got := WaitForEach(l, []string{"a", "b"}, timeout, pollEvery)
		if len(got) != 0 {
			t.Fatalf("WaitForEach = %v, want none timed out", got)
		}
	})

	t.Run("reports_names_that_never_get_recorded", func(t *testing.T) {
		l := NewLedger()

		got := WaitForEach(l, []string{"a", "b"}, timeout, pollEvery)
		want := []string{"a", "b"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("WaitForEach = %v, want %v", got, want)
		}
	})

	t.Run("only_reports_the_names_that_were_never_recorded", func(t *testing.T) {
		l := NewLedger()
		l.Record("a")

		got := WaitForEach(l, []string{"a", "b"}, timeout, pollEvery)
		if len(got) != 1 || got[0] != "b" {
			t.Fatalf("WaitForEach = %v, want [\"b\"]", got)
		}
	})

	t.Run("empty_names_returns_immediately_with_nothing_timed_out", func(t *testing.T) {
		got := WaitForEach(NewLedger(), nil, timeout, pollEvery)
		if len(got) != 0 {
			t.Fatalf("WaitForEach(nil names) = %v, want none", got)
		}
	})
}

func TestWatchTotal(t *testing.T) {
	t.Run("closes_once_limit_is_reached", func(t *testing.T) {
		l := NewLedger()
		l.Record("op")

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		done := WatchTotal(ctx, l, 1, 2*time.Millisecond)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("WatchTotal did not close done within 1s of the limit already being reached")
		}
	})

	t.Run("closes_when_context_is_cancelled_before_limit", func(t *testing.T) {
		l := NewLedger()
		ctx, cancel := context.WithCancel(context.Background())

		done := WatchTotal(ctx, l, 1_000_000, 2*time.Millisecond)
		cancel()

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("WatchTotal did not close done within 1s of ctx being cancelled")
		}
	})
}
