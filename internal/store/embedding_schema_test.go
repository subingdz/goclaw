package store

import (
	"testing"
)

func TestParseVectorTypeMod(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    int
		wantErr bool
	}{
		{"typical default", "vector(1536)", 1536, false},
		{"common smaller model", "vector(1024)", 1024, false},
		{"single digit", "vector(3)", 3, false},
		{"spaced", "  vector( 768 )  ", 768, false},
		{"dimensionless cannot be indexed", "vector", 0, true},
		{"not a vector column", "integer", 0, true},
		{"text column", "text", 0, true},
		{"zero", "vector(0)", 0, true},
		{"negative", "vector(-1)", 0, true},
		{"non-numeric", "vector(abc)", 0, true},
		{"empty", "", 0, true},
		{"only spaces", "   ", 0, true},
		{"prefix lookalike must not pass", "vectorish(10)", 0, true},
		{"unclosed paren", "vector(10", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseVectorTypeMod(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ParseVectorTypeMod(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("ParseVectorTypeMod(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// "vectorish(10)" must not parse: HasPrefix-style matching alone would accept it.
func TestParseVectorTypeMod_RejectsLookalikePrefix(t *testing.T) {
	if _, err := ParseVectorTypeMod("vectorish(10)"); err == nil {
		t.Fatal("lookalike type name should not parse as a vector column")
	}
}
