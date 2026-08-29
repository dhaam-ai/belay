package backoff

import (
	"math"
	"testing"
	"time"
)

func TestClamp(t *testing.T) {
	t.Run("below_min_returns_min", func(t *testing.T) {
		if got := Clamp(1*time.Second, 5*time.Second, 10*time.Second); got != 5*time.Second {
			t.Fatalf("Clamp(1s, 5s, 10s) = %s, want 5s", got)
		}
	})

	t.Run("above_max_returns_max", func(t *testing.T) {
		if got := Clamp(20*time.Second, 5*time.Second, 10*time.Second); got != 10*time.Second {
			t.Fatalf("Clamp(20s, 5s, 10s) = %s, want 10s", got)
		}
	})

	t.Run("within_range_is_unchanged", func(t *testing.T) {
		if got := Clamp(7*time.Second, 5*time.Second, 10*time.Second); got != 7*time.Second {
			t.Fatalf("Clamp(7s, 5s, 10s) = %s, want 7s", got)
		}
	})

	t.Run("equal_to_min_is_unchanged", func(t *testing.T) {
		if got := Clamp(5*time.Second, 5*time.Second, 10*time.Second); got != 5*time.Second {
			t.Fatalf("Clamp(5s, 5s, 10s) = %s, want 5s", got)
		}
	})

	t.Run("equal_to_max_is_unchanged", func(t *testing.T) {
		if got := Clamp(10*time.Second, 5*time.Second, 10*time.Second); got != 10*time.Second {
			t.Fatalf("Clamp(10s, 5s, 10s) = %s, want 10s", got)
		}
	})

	t.Run("min_greater_than_max_panics", func(t *testing.T) {
		defer func() {
			if recover() == nil {
				t.Fatal("Clamp(d, 10s, 5s) did not panic, want a panic since min > max")
			}
		}()
		Clamp(7*time.Second, 10*time.Second, 5*time.Second)
	})
}

func TestExponentialDelay(t *testing.T) {
	t.Run("zero_attempt_returns_base", func(t *testing.T) {
		if got := ExponentialDelay(time.Second, 0, time.Minute); got != time.Second {
			t.Fatalf("ExponentialDelay(1s, 0, 1m) = %s, want 1s", got)
		}
	})

	t.Run("doubles_each_attempt", func(t *testing.T) {
		cases := []struct {
			attempt uint
			want    time.Duration
		}{
			{1, 2 * time.Second},
			{2, 4 * time.Second},
			{3, 8 * time.Second},
			{4, 16 * time.Second},
		}
		for _, tc := range cases {
			if got := ExponentialDelay(time.Second, tc.attempt, time.Hour); got != tc.want {
				t.Errorf("ExponentialDelay(1s, %d, 1h) = %s, want %s", tc.attempt, got, tc.want)
			}
		}
	})

	t.Run("caps_at_max", func(t *testing.T) {
		if got := ExponentialDelay(time.Second, 10, time.Minute); got != time.Minute {
			t.Fatalf("ExponentialDelay(1s, 10, 1m) = %s, want capped at 1m", got)
		}
	})

	t.Run("base_at_or_above_max_returns_max", func(t *testing.T) {
		if got := ExponentialDelay(time.Hour, 3, time.Minute); got != time.Minute {
			t.Fatalf("ExponentialDelay(1h, 3, 1m) = %s, want 1m", got)
		}
	})

	t.Run("zero_base_returns_zero", func(t *testing.T) {
		if got := ExponentialDelay(0, 5, time.Minute); got != 0 {
			t.Fatalf("ExponentialDelay(0, 5, 1m) = %s, want 0", got)
		}
	})

	t.Run("zero_max_returns_zero", func(t *testing.T) {
		if got := ExponentialDelay(time.Second, 5, 0); got != 0 {
			t.Fatalf("ExponentialDelay(1s, 5, 0) = %s, want 0", got)
		}
	})

	t.Run("large_attempt_does_not_overflow", func(t *testing.T) {
		limit := time.Hour
		for _, attempt := range []uint{40, 62, 63, 1000, math.MaxUint32} {
			got := ExponentialDelay(time.Second, attempt, limit)
			if got <= 0 {
				t.Errorf("ExponentialDelay(1s, %d, 1h) = %s, want a positive duration (overflow, or decayed to zero by shifting all bits out)", attempt, got)
			}
			if got > limit {
				t.Errorf("ExponentialDelay(1s, %d, 1h) = %s, want <= limit %s", attempt, got, limit)
			}
		}
	})
}

func TestMinDelay(t *testing.T) {
	t.Run("empty_input_returns_false", func(t *testing.T) {
		got, ok := MinDelay(nil)
		if ok {
			t.Fatalf("MinDelay(nil) = (%s, true), want (0, false)", got)
		}
		if got != 0 {
			t.Fatalf("MinDelay(nil) duration = %s, want 0", got)
		}
	})

	t.Run("single_value", func(t *testing.T) {
		got, ok := MinDelay([]time.Duration{7 * time.Second})
		if !ok || got != 7*time.Second {
			t.Fatalf("MinDelay([7s]) = (%s, %v), want (7s, true)", got, ok)
		}
	})

	t.Run("returns_the_smallest_value_regardless_of_position", func(t *testing.T) {
		got, ok := MinDelay([]time.Duration{5 * time.Second, 1 * time.Second, 9 * time.Second})
		if !ok || got != time.Second {
			t.Fatalf("MinDelay([5s,1s,9s]) = (%s, %v), want (1s, true)", got, ok)
		}
	})

	t.Run("handles_duplicate_minimums", func(t *testing.T) {
		got, ok := MinDelay([]time.Duration{2 * time.Second, 2 * time.Second})
		if !ok || got != 2*time.Second {
			t.Fatalf("MinDelay([2s,2s]) = (%s, %v), want (2s, true)", got, ok)
		}
	})
}
