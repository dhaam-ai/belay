package belaytest

// pick returns items[n], clamped to the last element once n runs past the
// end of items, and the zero value of T if items is empty. This is the
// "queued responses, sticky on the last one" semantics every fake in this
// package uses for its Responses/Errs slices: a fake configured with one
// entry behaves identically on the first call and the hundredth, and a
// fake configured with a short "fails, then succeeds" sequence keeps
// returning the final outcome once the sequence runs out.
func pick[T any](items []T, n int) T {
	var zero T
	if len(items) == 0 {
		return zero
	}
	if n >= len(items) {
		n = len(items) - 1
	}
	if n < 0 {
		n = 0
	}
	return items[n]
}
