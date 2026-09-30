//go:build fixrate

package fixrate

import "math"

// This file holds the three numbers a paired two-arm comparison needs and
// nothing else: an interval around each arm's resolve rate, a test on the
// paired outcomes, and the dollar cost of one resolved task. They are pure
// functions of counts, with no clock, no filesystem and no dispatcher, so
// they are checked here against hand-computed exact values rather than
// against whatever a run happened to produce.
//
// # Why these three, and what they deliberately do NOT claim
//
// A fix rate quoted as a bare fraction invites the reader to treat it as
// precise. Over the corpus sizes this harness can afford, it is not: 30
// tasks resolved out of 50 is compatible with a true rate anywhere from
// roughly 0.46 to 0.72. Reporting an interval is not decoration; it is the
// difference between a number and a claim.
//
// Equally, an interval on each arm separately is the WRONG tool for
// "did belay help?", and this file does not offer a difference-of-two-
// proportions interval that would invite that reading. Both arms run the
// SAME tasks, so their outcomes are paired, and the paired information --
// which tasks one arm got and the other did not -- is exactly what an
// unpaired comparison throws away. ExactMcNemarP uses it.
//
// None of these functions knows anything about WHY a task resolved. A
// tight interval around a rate measured with a scripted agent is a tight
// interval around a property of belay's graph plumbing, not around any
// agent's skill; see Mode.provenance. Statistics cannot launder
// provenance, and nothing in this file tries to.

// Interval is a two-sided confidence interval on a proportion, in the
// same units as the proportion itself (0 to 1, not percent).
type Interval struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// Z95 is the standard normal distribution's 97.5th percentile, i.e. the
// multiplier for a two-sided 95% interval. It is spelled out to full
// float64 precision rather than the customary 1.96 so that a reported
// bound is reproducible to the last digit across implementations.
const Z95 = 1.959963984540054

// WilsonInterval returns the Wilson score interval for successes out of n
// at the confidence level implied by z (Z95 for 95%).
//
// The Wilson interval is used rather than the textbook normal
// approximation p̂ ± z·sqrt(p̂(1-p̂)/n) because that one is actively
// misleading at exactly the values a fix-rate report most often has to
// print. At successes == 0 or successes == n it collapses to a
// zero-width interval -- "0/20 resolved, 95% CI [0.00, 0.00]" -- which
// asserts certainty from twenty observations, and near either end it
// happily reports bounds outside [0,1]. An arm that resolved nothing is a
// real, reportable outcome for a control arm, so the interval that
// describes it must not be the one that breaks there.
//
// n == 0 returns the whole range [0, 1]. No observations carry no
// information, and the alternatives -- a zero-width interval at 0, or a
// NaN -- would both read as a finding. A caller that wants to say "no
// tasks were run" should say so in words; see PairedResult.Warnings.
//
// It is an interval on the observed proportion only. It says nothing
// about whether the tasks were representative, and nothing about what the
// arm was measuring; see this file's header comment.
func WilsonInterval(successes, n int, z float64) Interval {
	if n <= 0 {
		return Interval{Lo: 0, Hi: 1}
	}
	nf := float64(n)
	p := float64(successes) / nf
	z2 := z * z
	den := 1 + z2/nf
	centre := (p + z2/(2*nf)) / den
	half := (z / den) * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf))
	return Interval{
		Lo: math.Max(0, centre-half),
		Hi: math.Min(1, centre+half),
	}
}

// ExactMcNemarP returns the two-sided exact McNemar p-value for b
// discordant pairs favouring one arm and c favouring the other.
//
// b and c are the counts of tasks the two arms DISAGREED on: b that the
// first arm resolved and the second did not, c the other way round. The
// tasks both arms resolved, and the tasks neither resolved, carry no
// information about which arm is better and are correctly absent from the
// signature -- a reader who expects the concordant counts to matter is
// thinking of an unpaired test.
//
// Under the null hypothesis that the two arms are equally likely to be
// the one that resolves a task the other misses, the number of pairs
// falling to the first arm is Binomial(b+c, 1/2). The two-sided p-value
// is therefore twice the lower tail through min(b, c), capped at 1:
//
//	p = min(1, 2 * sum(C(n,k) / 2^n) for k = 0..min(b,c)), n = b+c
//
// This is the EXACT binomial form, not the chi-squared approximation
// (b-c)²/(b+c), and not Edwards's continuity-corrected variant. The
// approximation is the one usually quoted, and it is wrong in the regime
// this harness lives in: it needs b+c to be large (the usual rule of
// thumb is at least 25), and a corpus of a few dozen tasks with a handful
// of disagreements is nowhere near that. The exact form costs a loop over
// at most min(b,c) terms, which is free at any corpus size a paid run
// could afford.
//
// b+c == 0 returns exactly 1: the arms disagreed about nothing, so there
// is no evidence of a difference. That is not the same as evidence that
// they are equivalent, and a report must not present it as such.
//
// The terms are summed through math.Lgamma rather than by multiplying
// binomial coefficients, so nothing overflows for any b+c a corpus could
// reach; the result agrees with exact rational arithmetic to 15
// significant digits across the range stats_test.go checks.
func ExactMcNemarP(b, c int) float64 {
	if b < 0 || c < 0 {
		return math.NaN()
	}
	n := b + c
	if n == 0 {
		return 1
	}
	m := min(b, c)
	lnHalfPowN := float64(n) * math.Log(0.5)
	lgammaNPlus1, _ := math.Lgamma(float64(n + 1))

	var tail float64
	for k := 0; k <= m; k++ {
		lgammaK, _ := math.Lgamma(float64(k + 1))
		lgammaNK, _ := math.Lgamma(float64(n - k + 1))
		tail += math.Exp(lgammaNPlus1 - lgammaK - lgammaNK + lnHalfPowN)
	}
	return math.Min(1, 2*tail)
}

// USDPerResolved returns usd/resolved -- the dollar cost of one resolved
// task -- and false when resolved is 0.
//
// This is the number the report leads with, not a footnote: an arm's
// resolve rate alone cannot be compared against another arm's without it,
// because spending more is always available as a way to resolve more.
// "belay resolves 4 points more of the corpus" and "belay resolves 4
// points more of the corpus at 60% of the cost per fix" are different
// claims, and only the second one is worth publishing.
//
// resolved == 0 is undefined rather than infinite, and the bool says so,
// because an arm that spent money and resolved nothing has no cost per
// resolved task to quote. Returning +Inf would print as "+Inf" in a
// report and, worse, would compare as larger than any real figure in any
// code that sorted arms by it. A caller must print the absence, not a
// stand-in for it.
func USDPerResolved(usd float64, resolved int) (float64, bool) {
	if resolved <= 0 {
		return 0, false
	}
	return usd / float64(resolved), true
}
