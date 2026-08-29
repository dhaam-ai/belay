package belaytest

import "testing"

func TestPick(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		items []string
		n     int
		want  string
	}{
		{"empty", nil, 0, ""},
		{"empty, later index", nil, 5, ""},
		{"single entry, first call", []string{"a"}, 0, "a"},
		{"single entry, sticky on later call", []string{"a"}, 7, "a"},
		{"multiple entries, in range", []string{"a", "b", "c"}, 1, "b"},
		{"multiple entries, exact last", []string{"a", "b", "c"}, 2, "c"},
		{"multiple entries, sticky past end", []string{"a", "b", "c"}, 99, "c"},
		{"negative index clamps to first", []string{"a", "b"}, -1, "a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := pick(tt.items, tt.n); got != tt.want {
				t.Errorf("pick(%v, %d) = %q, want %q", tt.items, tt.n, got, tt.want)
			}
		})
	}
}
