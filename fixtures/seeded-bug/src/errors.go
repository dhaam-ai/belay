package backoff

import (
	"errors"
	"fmt"
)

// ErrTransient marks an error as retryable when wrapped with fmt.Errorf's
// %w verb (or with Retryable directly). Compare against it with
// errors.Is, never ==, since callers commonly wrap it with additional
// context.
var ErrTransient = errors.New("backoff: transient error")

// retryable wraps an error to mark it retryable without changing its
// message. Unwrap returns the original error, so errors.Is and errors.As
// still see through it.
type retryable struct {
	err error
}

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

// Retryable marks err as retryable. A nil err returns nil.
func Retryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryable{err: err}
}

// IsRetryable reports whether err (or any error it wraps) was marked
// retryable via Retryable, or wraps ErrTransient. A nil error is never
// retryable.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	var r *retryable
	if errors.As(err, &r) {
		return true
	}
	return errors.Is(err, ErrTransient)
}

// Classify wraps err as retryable if predicate reports true for it,
// leaving err unchanged otherwise. It is a convenience for call sites
// that classify errors from an external source (an HTTP status code, a
// database error code, and so on) without I/O of its own. A nil err
// returns nil.
func Classify(err error, predicate func(error) bool) error {
	if err == nil {
		return nil
	}
	if predicate(err) {
		return Retryable(err)
	}
	return err
}

// wrapAttempt annotates err with the attempt number it occurred on,
// preserving the error chain (and therefore retryability) via %w. A nil
// err returns nil.
func wrapAttempt(attempt int, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("attempt %d: %w", attempt, err)
}
