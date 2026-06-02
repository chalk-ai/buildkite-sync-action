package main

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// parseBootstrap asserts the bootstrap config is valid YAML with a single step
// and returns that step as a map.
func parseBootstrap(t *testing.T, cfg string) map[string]any {
	t.Helper()
	var parsed struct {
		Steps []map[string]any `yaml:"steps"`
	}
	if err := yaml.Unmarshal([]byte(cfg), &parsed); err != nil {
		t.Fatalf("bootstrap config is not valid YAML: %v\n%s", err, cfg)
	}
	if len(parsed.Steps) != 1 {
		t.Fatalf("expected exactly one step, got %d:\n%s", len(parsed.Steps), cfg)
	}
	return parsed.Steps[0]
}

func TestBootstrapConfigUngated(t *testing.T) {
	cfg := bootstrapConfig(".buildkite", "metaplanner-matrix.yml", "https://example/blob", 0, "")
	step := parseBootstrap(t, cfg)

	if _, ok := step["concurrency"]; ok {
		t.Errorf("expected no concurrency when group is empty:\n%s", cfg)
	}
	if _, ok := step["concurrency_group"]; ok {
		t.Errorf("expected no concurrency_group when group is empty:\n%s", cfg)
	}
	if cmd, _ := step["command"].(string); !strings.Contains(cmd, "buildkite-agent pipeline upload .buildkite/metaplanner-matrix.yml") {
		t.Errorf("upload command missing or wrong:\n%s", cfg)
	}
}

func TestBootstrapConfigGated(t *testing.T) {
	cfg := bootstrapConfig(".buildkite", "metaplanner-matrix.yml", "https://example/blob", 1, "metaplanner-matrix/chalk-q-staging")
	step := parseBootstrap(t, cfg)

	if got := step["concurrency"]; got != 1 {
		t.Errorf("concurrency = %v, want 1:\n%s", got, cfg)
	}
	if got := step["concurrency_group"]; got != "metaplanner-matrix/chalk-q-staging" {
		t.Errorf("concurrency_group = %v, want metaplanner-matrix/chalk-q-staging:\n%s", got, cfg)
	}
}

// TestBootstrapConfigGroupDefaultsConcurrency verifies a group with an unset or
// non-positive concurrency defaults to 1 rather than emitting an invalid 0.
func TestBootstrapConfigGroupDefaultsConcurrency(t *testing.T) {
	cfg := bootstrapConfig(".buildkite", "p.yml", "https://example/blob", 0, "grp")
	step := parseBootstrap(t, cfg)

	if got := step["concurrency"]; got != 1 {
		t.Errorf("concurrency = %v, want default 1:\n%s", got, cfg)
	}
}
