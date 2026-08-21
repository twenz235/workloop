package core

import "testing"

func TestConfigSetRunnerProvidersBuildsPool(t *testing.T) {
	s := testState(t)
	if _, err := s.ConfigSet("runner.providers", "codex,claude"); err != nil {
		t.Fatal(err)
	}
	if len(s.Config.Runner.Providers) != 2 {
		t.Fatalf("providers=%+v, want 2 entries", s.Config.Runner.Providers)
	}
	if s.Config.Runner.Providers[0].Provider != "codex" || s.Config.Runner.Providers[1].Provider != "claude" {
		t.Fatalf("providers=%+v, want codex then claude in the given order", s.Config.Runner.Providers)
	}
	for _, p := range s.Config.Runner.Providers {
		if p.ProviderPath == "" {
			t.Fatalf("provider %q resolved to an empty path", p.Provider)
		}
	}
}

func TestConfigSetRunnerProvidersPersistsAcrossReload(t *testing.T) {
	s := testState(t)
	if _, err := s.ConfigSet("runner.providers", "codex,claude"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := Open(s.Root)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Config.Runner.Providers) != 2 {
		t.Fatalf("reloaded providers=%+v, want 2 entries", reloaded.Config.Runner.Providers)
	}
}

func TestConfigSetRunnerProvidersRejectsDuplicate(t *testing.T) {
	s := testState(t)
	if _, err := s.ConfigSet("runner.providers", "codex,codex"); err == nil {
		t.Fatal("expected an error for a duplicate provider in the pool")
	}
}

func TestConfigSetRunnerProvidersRejectsUnknownProvider(t *testing.T) {
	s := testState(t)
	if _, err := s.ConfigSet("runner.providers", "codex,gpt5"); err == nil {
		t.Fatal("expected an error for an unknown provider name")
	}
}

func TestConfigSetRunnerProvidersRejectsSingleEntry(t *testing.T) {
	s := testState(t)
	if _, err := s.ConfigSet("runner.providers", "codex"); err == nil {
		t.Fatal("expected an error steering a single provider toward runner.provider instead")
	}
}

func TestConfigSetRunnerProvidersEmptyValueClearsPool(t *testing.T) {
	s := testState(t)
	if _, err := s.ConfigSet("runner.providers", "codex,claude"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfigSet("runner.providers", ""); err != nil {
		t.Fatal(err)
	}
	if len(s.Config.Runner.Providers) != 0 {
		t.Fatalf("providers=%+v, want an empty pool after clearing", s.Config.Runner.Providers)
	}
}
