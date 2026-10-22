package skills

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// searchableSource is a fake registry that answers keyword queries.
type searchableSource struct {
	name     string
	results  []RegistrySummary
	err      error
	gotQuery string
	gotLimit int
}

func (f *searchableSource) Name() string { return f.name }

func (f *searchableSource) Fetch(context.Context, string, string) (*RegistryBundle, error) {
	return nil, errors.New("searchableSource: fetch not used in these tests")
}

func (f *searchableSource) Search(_ context.Context, query string, limit int) ([]RegistrySummary, error) {
	f.gotQuery, f.gotLimit = query, limit
	return f.results, f.err
}

// fetchOnlySource models a Git host: install-by-name, no keyword search.
type fetchOnlySource struct{ name string }

func (f *fetchOnlySource) Name() string { return f.name }

func (f *fetchOnlySource) Fetch(context.Context, string, string) (*RegistryBundle, error) {
	return nil, errors.New("fetchOnlySource: fetch not used in these tests")
}

func enabled(v bool) *bool { return &v }

func registryConfig(clawHub, gitHub *SkillRegistryFixture) *config.Config {
	cfg := &config.Config{}
	cfg.Skills.Registries = &config.SkillRegistriesConfig{ClawHub: clawHub, GitHub: gitHub}
	return cfg
}

// SkillRegistryFixture keeps the test call sites readable.
type SkillRegistryFixture = config.SkillRegistryConfig

func TestBuildRegistriesDefaultsToNothing(t *testing.T) {
	for name, cfg := range map[string]*config.Config{
		"no block":      {Skills: config.SkillsConfig{}},
		"nil config":    nil,
		"explicit off":  registryConfig(&SkillRegistryFixture{Enabled: enabled(false)}, nil),
		"unset enabled": registryConfig(&SkillRegistryFixture{}, &SkillRegistryFixture{}),
	} {
		t.Run(name, func(t *testing.T) {
			regs := BuildRegistries(cfg)
			if regs.Enabled() {
				t.Fatalf("%s: Enabled() = true, want false (nothing may be contacted)", name)
			}
			if len(regs.Names()) != 0 || len(regs.Sources()) != 0 {
				t.Fatalf("%s: sources = %v", name, regs.Names())
			}
		})
	}
}

func TestBuildRegistriesKeepsTransportRules(t *testing.T) {
	tests := []struct {
		name    string
		clawHub *SkillRegistryFixture
		wantOn  bool
	}{
		{"https default", &SkillRegistryFixture{Enabled: enabled(true), BaseURL: DefaultClawHubBase}, true},
		{"plain http to a public host is refused", &SkillRegistryFixture{Enabled: enabled(true), BaseURL: "http://clawhub.example"}, false},
		{"loopback http is allowed", &SkillRegistryFixture{Enabled: enabled(true), BaseURL: "http://127.0.0.1:8080"}, true},
		{"not a url", &SkillRegistryFixture{Enabled: enabled(true), BaseURL: "://nope"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			regs := BuildRegistries(registryConfig(tt.clawHub, nil))
			if regs.Enabled() != tt.wantOn {
				t.Fatalf("Enabled() = %v, want %v", regs.Enabled(), tt.wantOn)
			}
		})
	}
}

func TestBuildRegistriesReportsSearchability(t *testing.T) {
	regs := BuildRegistries(registryConfig(
		&SkillRegistryFixture{Enabled: enabled(true)},
		&SkillRegistryFixture{Enabled: enabled(true)},
	))
	got := regs.Sources()
	if len(got) != 2 {
		t.Fatalf("sources = %+v, want clawhub and github", got)
	}
	if got[0].Name != "clawhub" || !got[0].Searchable {
		t.Fatalf("clawhub = %+v, want searchable", got[0])
	}
	if got[1].Name != "github" || got[1].Searchable {
		t.Fatalf("github = %+v, a git host has no keyword search", got[1])
	}
	if _, ok := regs.Get("github"); !ok {
		t.Fatal("Get(github) must resolve")
	}
	if _, ok := regs.Get("nope"); ok {
		t.Fatal("Get must refuse an unknown source")
	}
}

func TestSearchAllFansOutAndKeepsFailuresSeparate(t *testing.T) {
	up := &searchableSource{name: "up", results: []RegistrySummary{
		{Slug: "pdf", Score: 5, Source: "up"},
	}}
	broken := &searchableSource{name: "broken", err: errors.New("http 500")}
	offline := &fetchOnlySource{name: "git"}

	regs := &Registries{byName: map[string]Registry{}}
	regs.add(up)
	regs.add(broken)
	regs.add(offline)

	out, err := regs.SearchAll(context.Background(), "pdf", "", 10)
	if err != nil {
		t.Fatalf("SearchAll() error = %v", err)
	}
	if len(out.Summaries) != 1 || out.Summaries[0].Slug != "pdf" {
		t.Fatalf("summaries = %+v", out.Summaries)
	}
	// "the source is down" and "the source has nothing" must not look the same to the operator.
	if reason, ok := out.Failures["broken"]; !ok || !strings.Contains(reason, "http 500") {
		t.Fatalf("failures = %v", out.Failures)
	}
	if _, ok := out.Failures["git"]; ok {
		t.Fatalf("a source with no search is not a failure: %v", out.Failures)
	}
	if up.gotQuery != "pdf" || up.gotLimit != 10 {
		t.Fatalf("source saw query=%q limit=%d", up.gotQuery, up.gotLimit)
	}
}

func TestSearchAllSortsByScoreAndTruncates(t *testing.T) {
	a := &searchableSource{name: "a", results: []RegistrySummary{{Slug: "low", Score: 1}, {Slug: "high", Score: 9}}}
	b := &searchableSource{name: "b", results: []RegistrySummary{{Slug: "mid", Score: 5}}}
	regs := &Registries{byName: map[string]Registry{}}
	regs.add(a)
	regs.add(b)

	out, err := regs.SearchAll(context.Background(), "q", "", 2)
	if err != nil {
		t.Fatalf("SearchAll() error = %v", err)
	}
	var slugs []string
	for _, s := range out.Summaries {
		slugs = append(slugs, s.Slug)
	}
	if !slices.Equal(slugs, []string{"high", "mid"}) {
		t.Fatalf("order = %v, want the two best", slugs)
	}
}

func TestSearchAllErrors(t *testing.T) {
	t.Run("no sources", func(t *testing.T) {
		if _, err := (&Registries{byName: map[string]Registry{}}).SearchAll(context.Background(), "q", "", 5); !errors.Is(err, ErrNoRegistries) {
			t.Fatalf("error = %v, want ErrNoRegistries", err)
		}
		var nilRegs *Registries
		if _, err := nilRegs.SearchAll(context.Background(), "q", "", 5); !errors.Is(err, ErrNoRegistries) {
			t.Fatalf("nil receiver error = %v", err)
		}
	})

	t.Run("asking a fetch-only source directly", func(t *testing.T) {
		regs := &Registries{byName: map[string]Registry{}}
		regs.add(&fetchOnlySource{name: "git"})
		if _, err := regs.SearchAll(context.Background(), "q", "git", 5); !errors.Is(err, ErrRegistryNotSearchable) {
			t.Fatalf("error = %v, want ErrRegistryNotSearchable", err)
		}
	})

	t.Run("every source failed", func(t *testing.T) {
		f := &searchableSource{name: "f", err: errors.New("dns")}
		regs := &Registries{byName: map[string]Registry{}}
		regs.add(f)
		out, err := regs.SearchAll(context.Background(), "q", "", 5)
		if !errors.Is(err, ErrAllRegistriesFailed) {
			t.Fatalf("error = %v, want ErrAllRegistriesFailed", err)
		}
		if out.Failures["f"] == "" {
			t.Fatalf("the failing source must be named: %v", out.Failures)
		}
	})

	t.Run("a source that returns nothing is not an error", func(t *testing.T) {
		f := &searchableSource{name: "empty"}
		regs := &Registries{byName: map[string]Registry{}}
		regs.add(f)
		if _, err := regs.SearchAll(context.Background(), "q", "", 5); err != nil {
			t.Fatalf("SearchAll() error = %v, want an empty result set", err)
		}
	})
}

func TestSearchAllSourceFilterOnlyAsksOneSource(t *testing.T) {
	want := &searchableSource{name: "b", results: []RegistrySummary{{Slug: "only-b"}}}
	other := &searchableSource{name: "a", results: []RegistrySummary{{Slug: "only-a"}}}
	regs := &Registries{byName: map[string]Registry{}}
	regs.add(other)
	regs.add(want)

	out, err := regs.SearchAll(context.Background(), "q", "b", 10)
	if err != nil {
		t.Fatalf("SearchAll() error = %v", err)
	}
	if other.gotQuery != "" {
		t.Fatal("filtering by source must not ask the other registry")
	}
	if len(out.Summaries) != 1 || out.Summaries[0].Slug != "only-b" {
		t.Fatalf("summaries = %+v", out.Summaries)
	}
}

func TestRegistriesAddIgnoresDuplicateNames(t *testing.T) {
	regs := &Registries{byName: map[string]Registry{}}
	regs.add(&fetchOnlySource{name: "git"})
	regs.add(&fetchOnlySource{name: "git"})
	if len(regs.Names()) != 1 {
		t.Fatalf("names = %v, want one entry per source name", regs.Names())
	}
}
