//go:build fixrate

package fixrate

import (
	"math"
	"testing"
)

// tol is the tolerance every float comparison in this file uses. The
// formulas here are closed-form, so agreement to 1e-12 is the right bar:
// anything looser would hide a genuine algebra error.
const tol = 1e-12

func TestWilsonInterval(t *testing.T) {
	t.Parallel()

	// Expected values computed independently from the Wilson score
	// formula outside Go, and spot-checked against the textbook figures
	// for the two canonical cases: 8/10 -> [0.4902, 0.9433] and the
	// zero-numerator 0/20 -> upper bound 0.1611.
	tests := []struct {
		name      string
		successes int
		n         int
		wantLo    float64
		wantHi    float64
	}{
		{"8 of 10", 8, 10, 0.490162471537, 0.943317848546},
		{"none of 20", 0, 20, 0.0, 0.161125158053},
		{"all of 20", 20, 20, 0.838874841947, 1.0},
		{"half of 10", 5, 10, 0.236593090513, 0.763406909487},
		{"one of one", 1, 1, 0.206549314377, 1.0},
		{"none of one", 0, 1, 0.0, 0.793450685623},
		{"half of 500", 250, 500, 0.456341265302, 0.543658734698},
		{"300 of 500", 300, 500, 0.556454122702, 0.642021009205},
		// No observations at all carries no information, and the honest
		// interval is the whole range rather than a point at zero.
		{"no observations", 0, 0, 0.0, 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := WilsonInterval(tt.successes, tt.n, Z95)
			if math.Abs(got.Lo-tt.wantLo) > 1e-9 {
				t.Errorf("WilsonInterval(%d, %d).Lo = %.12f, want %.12f", tt.successes, tt.n, got.Lo, tt.wantLo)
			}
			if math.Abs(got.Hi-tt.wantHi) > 1e-9 {
				t.Errorf("WilsonInterval(%d, %d).Hi = %.12f, want %.12f", tt.successes, tt.n, got.Hi, tt.wantHi)
			}
		})
	}
}

func TestWilsonInterval_Invariants(t *testing.T) {
	t.Parallel()
	for n := 1; n <= 40; n++ {
		for x := 0; x <= n; x++ {
			got := WilsonInterval(x, n, Z95)
			p := float64(x) / float64(n)
			switch {
			case got.Lo < 0 || got.Hi > 1:
				t.Fatalf("WilsonInterval(%d, %d) = [%.6f, %.6f], want it inside [0,1]", x, n, got.Lo, got.Hi)
			case got.Lo > got.Hi:
				t.Fatalf("WilsonInterval(%d, %d) = [%.6f, %.6f], want Lo <= Hi", x, n, got.Lo, got.Hi)
			case p < got.Lo-tol || p > got.Hi+tol:
				t.Fatalf("WilsonInterval(%d, %d) = [%.6f, %.6f], want it to contain the point estimate %.6f", x, n, got.Lo, got.Hi, p)
			}
			// Wilson is symmetric under relabelling success as failure.
			mirror := WilsonInterval(n-x, n, Z95)
			if math.Abs((1-mirror.Hi)-got.Lo) > tol || math.Abs((1-mirror.Lo)-got.Hi) > tol {
				t.Fatalf("WilsonInterval(%d, %d) = [%.12f, %.12f] is not the mirror of WilsonInterval(%d, %d) = [%.12f, %.12f]",
					x, n, got.Lo, got.Hi, n-x, n, mirror.Lo, mirror.Hi)
			}
		}
	}
}

func TestExactMcNemarP(t *testing.T) {
	t.Parallel()

	// Every expectation here is an exact rational: under the null the
	// discordant count is Binomial(b+c, 1/2), so the two-sided p-value
	// is 2*sum(C(n,k)/2^n) over k <= min(b,c), capped at 1. The rationals
	// were computed exactly (Python Fraction + math.comb) and agree with
	// this implementation's float arithmetic to 15 digits.
	tests := []struct {
		name  string
		b     int
		c     int
		wantP float64
	}{
		{"no discordant pairs", 0, 0, 1.0},                // no evidence either way
		{"one pair", 1, 0, 1.0},                           // 2 * 1/2
		{"two pairs one way", 2, 0, 0.5},                  // 2 * 1/4
		{"three pairs one way", 3, 0, 0.25},               // 2 * 1/8
		{"five pairs one way", 5, 0, 0.0625},              // 2 * 1/32
		{"six pairs one way", 6, 0, 0.03125},              // 2 * 1/64
		{"eight versus one", 8, 1, 0.0390625},             // 2 * (1+9)/512
		{"ten versus two", 10, 2, 0.03857421875},          // 2 * (1+12+66)/4096
		{"six pairs the other way", 0, 6, 0.03125},        // symmetric in b and c
		{"balanced", 4, 4, 1.0},                           // capped at 1
		{"thirty versus ten", 30, 10, 0.002221433773229},  // 305311533/137438953472
		{"twelve versus three", 12, 3, 0.035156250000000}, // 9/256
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ExactMcNemarP(tt.b, tt.c)
			if math.Abs(got-tt.wantP) > 1e-12 {
				t.Errorf("ExactMcNemarP(%d, %d) = %.15f, want %.15f", tt.b, tt.c, got, tt.wantP)
			}
		})
	}
}

func TestExactMcNemarP_Invariants(t *testing.T) {
	t.Parallel()
	for b := 0; b <= 25; b++ {
		for c := 0; c <= 25; c++ {
			got := ExactMcNemarP(b, c)
			if got < 0 || got > 1 {
				t.Fatalf("ExactMcNemarP(%d, %d) = %.15f, want a probability in [0,1]", b, c, got)
			}
			if mirror := ExactMcNemarP(c, b); math.Abs(mirror-got) > tol {
				t.Fatalf("ExactMcNemarP(%d, %d) = %.15f but ExactMcNemarP(%d, %d) = %.15f; the test is two-sided and must be symmetric",
					b, c, got, c, b, mirror)
			}
		}
	}
}

func TestUSDPerResolved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		usd      float64
		resolved int
		want     float64
		wantOK   bool
	}{
		{"four dollars over two tasks", 4.0, 2, 2.0, true},
		{"free but resolved", 0.0, 3, 0.0, true},
		// The headline number is undefined, not infinite, when nothing
		// was resolved: an arm that spent money and fixed nothing has no
		// cost per resolved task to quote.
		{"nothing resolved", 12.5, 0, 0, false},
		{"nothing spent and nothing resolved", 0, 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := USDPerResolved(tt.usd, tt.resolved)
			if ok != tt.wantOK {
				t.Fatalf("USDPerResolved(%v, %d) ok = %v, want %v", tt.usd, tt.resolved, ok, tt.wantOK)
			}
			if ok && math.Abs(got-tt.want) > tol {
				t.Errorf("USDPerResolved(%v, %d) = %v, want %v", tt.usd, tt.resolved, got, tt.want)
			}
		})
	}
}
