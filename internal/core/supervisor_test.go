package core

import (
	"strconv"
	"testing"
)

func TestSelectProviderFallsBackToLegacySingleProviderWhenPoolEmpty(t *testing.T) {
	s := &State{Config: Config{Runner: RunnerConfig{Provider: "claude", ProviderPath: "/bin/claude"}}}
	provider, providerPath := s.selectProvider("card-1", "dev", 1)
	if provider != "claude" || providerPath != "/bin/claude" {
		t.Fatalf("provider=%q providerPath=%q, want legacy claude", provider, providerPath)
	}
}

func TestSelectProviderIsDeterministicForTheSameAttempt(t *testing.T) {
	s := &State{Config: Config{Runner: RunnerConfig{Providers: []RunnerProviderEntry{
		{Provider: "codex", ProviderPath: "/bin/codex"},
		{Provider: "claude", ProviderPath: "/bin/claude"},
	}}}}
	p1, path1 := s.selectProvider("card-7", "qa", 3)
	p2, path2 := s.selectProvider("card-7", "qa", 3)
	if p1 != p2 || path1 != path2 {
		t.Fatalf("selectProvider is not deterministic: (%q,%q) vs (%q,%q)", p1, path1, p2, path2)
	}
}

func TestSelectProviderUsesEveryPoolMemberAcrossManyCards(t *testing.T) {
	s := &State{Config: Config{Runner: RunnerConfig{Providers: []RunnerProviderEntry{
		{Provider: "codex", ProviderPath: "/bin/codex"},
		{Provider: "claude", ProviderPath: "/bin/claude"},
	}}}}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		provider, _ := s.selectProvider("card-"+strconv.Itoa(i), "dev", 1)
		seen[provider] = true
	}
	if !seen["codex"] || !seen["claude"] {
		t.Fatalf("expected both pool providers to be selected at least once across 200 cards, got %v", seen)
	}
}

func TestSelectProviderOftenChangesProviderOnRetry(t *testing.T) {
	s := &State{Config: Config{Runner: RunnerConfig{Providers: []RunnerProviderEntry{
		{Provider: "codex", ProviderPath: "/bin/codex"},
		{Provider: "claude", ProviderPath: "/bin/claude"},
	}}}}
	switched := 0
	const trials = 50
	for i := 0; i < trials; i++ {
		id := "card-" + string(rune('a'+i%26))
		attempt1, _ := s.selectProvider(id, "dev", 1)
		attempt2, _ := s.selectProvider(id, "dev", 2)
		if attempt1 != attempt2 {
			switched++
		}
	}
	if switched == 0 {
		t.Fatalf("expected at least some retries (attempt+1) to land on a different provider, got 0 of %d", trials)
	}
}
