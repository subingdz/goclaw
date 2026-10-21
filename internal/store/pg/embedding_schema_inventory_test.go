package pg

import "testing"

func TestQuotePgIdent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"memory_chunks", `"memory_chunks"`},
		{`weird"name`, `"weird""name"`},
		{"", `""`},
	}
	for _, tc := range cases {
		if got := quotePgIdent(tc.in); got != tc.want {
			t.Errorf("quotePgIdent(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}
