package backoff

import (
	"errors"
	"fmt"
	"testing"
)

func TestRetryable(t *testing.T) {
	t.Run("nil_stays_nil", func(t *testing.T) {
		if got := Retryable(nil); got != nil {
			t.Fatalf("Retryable(nil) = %v, want nil", got)
		}
	})

	t.Run("wraps_the_original_error", func(t *testing.T) {
		base := errors.New("boom")
		got := Retryable(base)
		if got == nil {
			t.Fatal("Retryable(base) = nil, want a wrapped error")
		}
		if !errors.Is(got, base) {
			t.Fatalf("errors.Is(Retryable(base), base) = false, want true")
		}
		if got.Error() != base.Error() {
			t.Fatalf("Retryable(base).Error() = %q, want %q", got.Error(), base.Error())
		}
	})
}

func TestIsRetryable(t *testing.T) {
	t.Run("nil_is_not_retryable", func(t *testing.T) {
		if IsRetryable(nil) {
			t.Fatal("IsRetryable(nil) = true, want false")
		}
	})

	t.Run("plain_error_is_not_retryable", func(t *testing.T) {
		if IsRetryable(errors.New("boom")) {
			t.Fatal("IsRetryable(plain error) = true, want false")
		}
	})

	t.Run("retryable_wrapped_error_is_retryable", func(t *testing.T) {
		if !IsRetryable(Retryable(errors.New("boom"))) {
			t.Fatal("IsRetryable(Retryable(err)) = false, want true")
		}
	})

	t.Run("retryable_wrapped_error_survives_further_wrapping", func(t *testing.T) {
		wrapped := fmt.Errorf("while doing X: %w", Retryable(errors.New("boom")))
		if !IsRetryable(wrapped) {
			t.Fatal("IsRetryable(fmt.Errorf(%%w, Retryable(err))) = false, want true")
		}
	})

	t.Run("bare_transient_sentinel_is_retryable", func(t *testing.T) {
		if !IsRetryable(ErrTransient) {
			t.Fatal("IsRetryable(ErrTransient) = false, want true")
		}
	})

	t.Run("wrapped_transient_sentinel_is_retryable", func(t *testing.T) {
		wrapped := fmt.Errorf("dial failed: %w", ErrTransient)
		if !IsRetryable(wrapped) {
			t.Fatal("IsRetryable(fmt.Errorf(%%w, ErrTransient)) = false, want true")
		}
	})

	t.Run("unrelated_sentinel_is_not_retryable", func(t *testing.T) {
		other := errors.New("backoff: transient error")
		if IsRetryable(other) {
			t.Fatal("IsRetryable(a different error with the same message) = true, want false")
		}
	})
}

func TestClassify(t *testing.T) {
	always := func(error) bool { return true }
	never := func(error) bool { return false }

	t.Run("nil_error_stays_nil", func(t *testing.T) {
		if got := Classify(nil, always); got != nil {
			t.Fatalf("Classify(nil, always) = %v, want nil", got)
		}
	})

	t.Run("wraps_when_predicate_true", func(t *testing.T) {
		base := errors.New("rate limited")
		got := Classify(base, always)
		if !IsRetryable(got) {
			t.Fatal("Classify(err, always-true) is not retryable, want retryable")
		}
		if !errors.Is(got, base) {
			t.Fatal("Classify(err, always-true) lost the original error from its chain")
		}
	})

	t.Run("leaves_unchanged_when_predicate_false", func(t *testing.T) {
		base := errors.New("permission denied")
		got := Classify(base, never)
		if got == nil {
			t.Fatal("Classify(err, always-false) = nil, want the original error preserved")
		}
		if !errors.Is(got, base) {
			t.Fatalf("Classify(err, always-false) = %v, want it to still be (or wrap) the original error", got)
		}
		if IsRetryable(got) {
			t.Fatal("Classify(err, always-false) is retryable, want not retryable")
		}
	})
}

func TestWrapAttempt(t *testing.T) {
	t.Run("nil_error_stays_nil", func(t *testing.T) {
		if got := wrapAttempt(3, nil); got != nil {
			t.Fatalf("wrapAttempt(3, nil) = %v, want nil", got)
		}
	})

	t.Run("wraps_the_error_with_the_attempt_number", func(t *testing.T) {
		base := errors.New("boom")
		got := wrapAttempt(2, base)
		if !errors.Is(got, base) {
			t.Fatalf("errors.Is(wrapAttempt(2, base), base) = false, want true")
		}
		want := "attempt 2: boom"
		if got.Error() != want {
			t.Fatalf("wrapAttempt(2, base).Error() = %q, want %q", got.Error(), want)
		}
	})
}
