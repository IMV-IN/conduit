package config

import "testing"

func TestLoadExample(t *testing.T) {
	cfg, err := Load("../../conduit.example.yaml")
	if err != nil {
		t.Fatalf("example config must load: %v", err)
	}
	for _, name := range []string{"default", "baseline-best", "baseline-cheap", "round-robin"} {
		if cfg.PolicyByName(name) == nil {
			t.Fatalf("example config missing policy %q", name)
		}
	}
	if cfg.ModelByID("mock/premium") == nil {
		t.Fatal("example config missing mock/premium")
	}
}

func TestValidateRejectsEmpty(t *testing.T) {
	var c Config
	if err := c.Validate(); err == nil {
		t.Fatal("empty config should not validate")
	}
}
