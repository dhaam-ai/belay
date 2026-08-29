package belay_test

import (
	"encoding/json"

	"github.com/google/go-cmp/cmp"
)

// rawMessageComparer treats a nil json.RawMessage and the literal JSON
// value null as equal. json.RawMessage.MarshalJSON renders nil as "null",
// and unmarshaling "null" back into a json.RawMessage stores the literal
// bytes "null" rather than restoring nil (see doc.go) — a real, documented
// property of the stdlib type, not a bug in this package. Tests that
// round-trip a struct containing a RawMessage field through JSON use this
// comparer so that documented, harmless behavior does not masquerade as a
// test failure.
var rawMessageComparer = cmp.Comparer(func(a, b json.RawMessage) bool {
	norm := func(m json.RawMessage) string {
		if len(m) == 0 || string(m) == "null" {
			return ""
		}
		return string(m)
	}
	return norm(a) == norm(b)
})
