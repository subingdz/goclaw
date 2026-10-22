package skills

import (
	"context"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/nextlevelbuilder/goclaw/internal/config"
)

// Registries is the enabled set of community skill sources, built once at startup.
//
// It is deliberately a value with no live reload: a new outbound source is an operator decision
// made in config, and hot-swapping registries while an install is in flight buys nothing.
type Registries struct {
	ordered []Registry
	byName  map[string]Registry
}

// SearchOutcome is a fan-out result. A source that fails is named rather than hidden, because
// "the hub is down" and "the hub has nothing by that name" lead to different actions.
type SearchOutcome struct {
	Summaries []RegistrySummary
	Failures  map[string]string
}

// BuildRegistries constructs the enabled sources. A source that is configured but invalid is
// reported and skipped rather than silently dropped: an operator who typed a bad URL deserves
// a log line, not a search box that returns nothing.
func BuildRegistries(cfg *config.Config) *Registries {
	r := &Registries{byName: make(map[string]Registry, 2)}
	if cfg == nil {
		return r
	}
	src := cfg.Skills.Registries
	if src == nil {
		return r
	}

	if src.ClawHub.IsEnabled() {
		base := src.ClawHub.BaseURL
		if base == "" {
			base = DefaultClawHubBase
		}
		reg, err := NewClawHubRegistry(ClawHubConfig{
			BaseURL: base,
			Token:   src.ClawHub.Token,
			Timeout: registryTimeout(src.ClawHub.TimeoutMs),
		})
		if err != nil {
			slog.Warn("skills.registry: clawhub not enabled", "error", err)
		} else {
			r.add(reg)
			slog.Info("skills.registry: clawhub enabled", "base_url", base)
		}
	}

	if src.GitHub.IsEnabled() {
		reg, err := NewGitHubRegistry(GitHubConfig{
			APIBase: src.GitHub.BaseURL,
			Token:   src.GitHub.Token,
			Timeout: registryTimeout(src.GitHub.TimeoutMs),
		})
		if err != nil {
			slog.Warn("skills.registry: github not enabled", "error", err)
		} else {
			r.add(reg)
			slog.Info("skills.registry: github enabled", "api_base", reg.api)
		}
	}

	return r
}

// NewRegistries assembles the set from sources that are already built. BuildRegistries reads
// config; this is the seam for tests and for any caller that constructs sources itself.
func NewRegistries(regs ...Registry) *Registries {
	out := &Registries{byName: make(map[string]Registry, len(regs))}
	for _, reg := range regs {
		out.add(reg)
	}
	return out
}

func registryTimeout(ms int) time.Duration {
	if ms <= 0 {
		return 0 // the constructors apply their own default
	}
	return time.Duration(ms) * time.Millisecond
}

func (r *Registries) add(reg Registry) {
	if _, dup := r.byName[reg.Name()]; dup {
		return
	}
	r.byName[reg.Name()] = reg
	r.ordered = append(r.ordered, reg)
}

// Enabled reports whether any source may be contacted.
func (r *Registries) Enabled() bool { return r != nil && len(r.ordered) > 0 }

// Names lists the enabled sources in config order.
func (r *Registries) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.ordered))
	for _, reg := range r.ordered {
		out = append(out, reg.Name())
	}
	return out
}

// RegistrySource is one enabled source as the UI needs to see it: whether asking it a keyword
// question is meaningful at all.
type RegistrySource struct {
	Name       string `json:"name"`
	Searchable bool   `json:"searchable"`
}

// Sources lists the enabled sources in config order.
func (r *Registries) Sources() []RegistrySource {
	if r == nil {
		return nil
	}
	out := make([]RegistrySource, 0, len(r.ordered))
	for _, reg := range r.ordered {
		_, searchable := reg.(Searcher)
		out = append(out, RegistrySource{Name: reg.Name(), Searchable: searchable})
	}
	return out
}

// Get returns one enabled source.
func (r *Registries) Get(name string) (Registry, bool) {
	if r == nil {
		return nil, false
	}
	reg, ok := r.byName[name]
	return reg, ok
}

// SearchAll asks every searchable source and merges the results by the sources' own scores.
// Scores from different registries are not comparable in any strict sense, so they are only
// used to keep the most relevant matches near the top rather than to rank across sources.
func (r *Registries) SearchAll(ctx context.Context, query, source string, limit int) (SearchOutcome, error) {
	outcome := SearchOutcome{Failures: map[string]string{}}
	if r == nil || len(r.ordered) == 0 {
		return outcome, ErrNoRegistries
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	type result struct {
		source string
		items  []RegistrySummary
		err    error
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		collect []result
	)
	for _, reg := range r.ordered {
		if source != "" && reg.Name() != source {
			continue
		}
		searcher, ok := reg.(Searcher)
		if !ok {
			if source == reg.Name() {
				// Asking a non-searchable source directly is a caller error, not a partial
				// success worth swallowing.
				return outcome, ErrRegistryNotSearchable
			}
			continue
		}
		wg.Add(1)
		go func(name string, s Searcher) {
			defer wg.Done()
			items, err := s.Search(ctx, query, limit)
			mu.Lock()
			collect = append(collect, result{source: name, items: items, err: err})
			mu.Unlock()
		}(reg.Name(), searcher)
	}
	wg.Wait()

	if len(collect) == 0 {
		return outcome, ErrRegistryNotSearchable
	}

	for _, res := range collect {
		if res.err != nil {
			outcome.Failures[res.source] = res.err.Error()
			slog.Warn("skills.registry: search failed", "source", res.source, "error", res.err)
			continue
		}
		outcome.Summaries = append(outcome.Summaries, res.items...)
	}
	sort.SliceStable(outcome.Summaries, func(i, j int) bool {
		return outcome.Summaries[i].Score > outcome.Summaries[j].Score
	})
	if len(outcome.Summaries) > limit {
		outcome.Summaries = outcome.Summaries[:limit]
	}
	if len(outcome.Summaries) == 0 && len(outcome.Failures) > 0 {
		return outcome, ErrAllRegistriesFailed
	}
	return outcome, nil
}
