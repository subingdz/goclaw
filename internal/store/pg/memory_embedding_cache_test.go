package pg

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var placeholderRe = regexp.MustCompile(`\$(\d+)`)

// The positional-arg invariant: every placeholder $1..$N appears and N equals the
// number of args. A mismatch here does not fail loudly at runtime — it silently
// returns no rows (or errors on the server), which is exactly how a dimension filter
// would look like "cache miss" instead of "wrong query".
func assertPlaceholdersMatchArgs(t *testing.T, query string, args []any) {
	t.Helper()
	found := placeholderRe.FindAllStringSubmatch(query, -1)
	if len(found) == 0 {
		t.Fatalf("query has no positional placeholders: %q", query)
	}
	seen := make(map[int]bool, len(found))
	for _, m := range found {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("unparsable placeholder %q: %v", m[0], err)
		}
		seen[n] = true
	}
	for i := 1; i <= len(args); i++ {
		if !seen[i] {
			t.Errorf("arg %d has no placeholder in query: %q", i, query)
		}
	}
	if len(args) < len(seen) {
		t.Errorf("query references %d distinct placeholders but only %d args were built", len(seen), len(args))
	}
}

func TestBuildEmbeddingCacheLookup_FiltersByDimsWhenConfigured(t *testing.T) {
	query, args := buildEmbeddingCacheLookup([]string{"h1", "h2"}, "openai", "text-embedding-3-small", 1024)

	assertPlaceholdersMatchArgs(t, query, args)

	if want := " AND dims = $5"; !strings.Contains(query, want) {
		t.Fatalf("dims filter missing; want %q in:\n%s", want, query)
	}
	if got := len(args); got != 5 {
		t.Fatalf("arg count = %d, want 5 (2 hashes + provider + model + dims)", got)
	}
	if got := args[4]; got != 1024 {
		t.Fatalf("dims arg = %v, want 1024", got)
	}
	if args[2] != "openai" || args[3] != "text-embedding-3-small" {
		t.Fatalf("provider/model args out of order: %v", args)
	}
}

// A provider that does not declare a width must keep the old behaviour rather than
// silently matching zero rows with "dims = 0".
func TestBuildEmbeddingCacheLookup_NoDimsFilterWhenUnset(t *testing.T) {
	for _, dims := range []int{0, -1} {
		t.Run(fmt.Sprintf("dims=%d", dims), func(t *testing.T) {
			query, args := buildEmbeddingCacheLookup([]string{"h1"}, "openai", "m", dims)

			if strings.Contains(query, "dims") {
				t.Fatalf("dims clause must be absent when the width is unknown:\n%s", query)
			}
			if got := len(args); got != 3 {
				t.Fatalf("arg count = %d, want 3", got)
			}
			assertPlaceholdersMatchArgs(t, query, args)
		})
	}
}

// Two different widths must fetch two different row sets. Note the SQL text is
// intentionally identical: the width travels as a bound arg ($N), not as literal SQL.
// Asserting on the query string here would encode the wrong model of how the filter works.
func TestBuildEmbeddingCacheLookup_DifferentWidthsFetchDifferentRows(t *testing.T) {
	q1024, a1024 := buildEmbeddingCacheLookup([]string{"h1"}, "openai", "m", 1024)
	q1536, a1536 := buildEmbeddingCacheLookup([]string{"h1"}, "openai", "m", 1536)

	for _, q := range []string{q1024, q1536} {
		if !strings.Contains(q, " AND dims = ") {
			t.Fatalf("dims filter absent from SQL:\n%s", q)
		}
	}
	if len(a1024) != len(a1536) {
		t.Fatalf("arg counts differ: %d vs %d", len(a1024), len(a1536))
	}
	if a1024[len(a1024)-1] == a1536[len(a1536)-1] {
		t.Fatalf("bound dims arg identical for different widths: %v vs %v", a1024, a1536)
	}
	if a1024[len(a1024)-1] != 1024 || a1536[len(a1536)-1] != 1536 {
		t.Fatalf("bound dims args wrong: %v / %v", a1024[len(a1024)-1], a1536[len(a1536)-1])
	}
}

func TestBuildEmbeddingCacheLookup_HashPlaceholdersAreSequential(t *testing.T) {
	hashes := []string{"a", "b", "c", "d"}
	query, args := buildEmbeddingCacheLookup(hashes, "p", "m", 512)

	assertPlaceholdersMatchArgs(t, query, args)
	for i, h := range hashes {
		if args[i] != h {
			t.Fatalf("arg %d = %v, want hash %q", i+1, args[i], h)
		}
	}
	// $1..$4 for hashes, $5 provider, $6 model, $7 dims
	if want := "hash IN ($1,$2,$3,$4)"; !strings.Contains(query, want) {
		t.Fatalf("hash placeholder list wrong; want %q in:\n%s", want, query)
	}
	if want := " AND dims = $7"; !strings.Contains(query, want) {
		t.Fatalf("dims placeholder should follow provider/model; want %q in:\n%s", want, query)
	}
}

func TestParseVector(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
		wantErr bool
	}{
		{"pgvector brackets", "[0.1,0.2,0.3]", 3, false},
		{"paren fallback", "(0.5,0.5)", 2, false},
		{"empty vector", "[]", 0, false},
		{"too short", "[", 0, true},
		{"non-numeric element", "[0.1,x]", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVector(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseVector(%q) err = %v, wantErr = %v", tc.in, err, tc.wantErr)
			}
			if err == nil && len(got) != tc.wantLen {
				t.Fatalf("parseVector(%q) len = %d, want %d", tc.in, len(got), tc.wantLen)
			}
		})
	}
}
