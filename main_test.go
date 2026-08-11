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
	cfg := bootstrapConfig(".buildkite", "metaplanner-matrix.yml", "https://example/blob", 0, "", "")
	step := parseBootstrap(t, cfg)

	if _, ok := step["concurrency"]; ok {
		t.Errorf("expected no concurrency when group is empty:\n%s", cfg)
	}
	if _, ok := step["concurrency_group"]; ok {
		t.Errorf("expected no concurrency_group when group is empty:\n%s", cfg)
	}
	if _, ok := step["agents"]; ok {
		t.Errorf("expected no agents tag when uploadQueue is empty:\n%s", cfg)
	}
	if cmd, _ := step["command"].(string); !strings.Contains(cmd, "buildkite-agent pipeline upload .buildkite/metaplanner-matrix.yml") {
		t.Errorf("upload command missing or wrong:\n%s", cfg)
	}
}

func TestBootstrapConfigGated(t *testing.T) {
	cfg := bootstrapConfig(".buildkite", "metaplanner-matrix.yml", "https://example/blob", 1, "metaplanner-matrix/chalk-q-staging", "")
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
	cfg := bootstrapConfig(".buildkite", "p.yml", "https://example/blob", 0, "grp", "")
	step := parseBootstrap(t, cfg)

	if got := step["concurrency"]; got != 1 {
		t.Errorf("concurrency = %v, want default 1:\n%s", got, cfg)
	}
}

// TestBootstrapConfigUploadQueue verifies the upload step is pinned to the given
// agent queue when uploadQueue is set.
func TestBootstrapConfigUploadQueue(t *testing.T) {
	cfg := bootstrapConfig(".buildkite", "metaplanner-matrix.yml", "https://example/blob", 0, "", "small-job-queue")
	step := parseBootstrap(t, cfg)

	agents, ok := step["agents"].(map[string]any)
	if !ok {
		t.Fatalf("expected agents tag to be set:\n%s", cfg)
	}
	if got := agents["queue"]; got != "small-job-queue" {
		t.Errorf("agents.queue = %v, want small-job-queue:\n%s", got, cfg)
	}
}

func TestScheduleKeyAbsent(t *testing.T) {
	t.Parallel()
	var pf PipelineFile
	if err := yaml.Unmarshal([]byte("on: {}\n"), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pf.On.Schedule != nil {
		t.Errorf("Schedule = non-nil, want nil when key is absent")
	}
}

func TestScheduleKeyPresentButEmpty(t *testing.T) {
	t.Parallel()
	var pf PipelineFile
	if err := yaml.Unmarshal([]byte("on:\n  schedule: []\n"), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pf.On.Schedule == nil {
		t.Fatal("Schedule is nil, want non-nil when key is present")
	}
	if len(*pf.On.Schedule) != 0 {
		t.Errorf("len(Schedule) = %d, want 0", len(*pf.On.Schedule))
	}
}

func TestNextLinkURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		header string
		want   string
	}{
		{
			header: `<https://api.buildkite.com/v2/pipelines?page=2&per_page=100>; rel="next", <https://api.buildkite.com/v2/pipelines?page=5&per_page=100>; rel="last"`,
			want:   "https://api.buildkite.com/v2/pipelines?page=2&per_page=100",
		},
		{
			header: `<https://api.buildkite.com/v2/pipelines?page=5&per_page=100>; rel="last"`,
			want:   "",
		},
		{header: "", want: ""},
	}
	for _, c := range cases {
		if got := nextLinkURL(c.header); got != c.want {
			t.Errorf("nextLinkURL(%q) = %q, want %q", c.header, got, c.want)
		}
	}
}

func TestToScheduleReqDefaults(t *testing.T) {
	t.Parallel()
	s := &ScheduleEntry{Label: "nightly", Cron: "0 2 * * *"}
	req := toScheduleReq(s, "main")

	if req.Branch != "main" {
		t.Errorf("Branch = %q, want %q", req.Branch, "main")
	}
	if !req.Enabled {
		t.Errorf("Enabled = false, want true by default")
	}
	if req.Cronline != "0 2 * * *" {
		t.Errorf("Cronline = %q, want %q", req.Cronline, "0 2 * * *")
	}
}

func TestToScheduleReqExplicitValues(t *testing.T) {
	t.Parallel()
	disabled := false
	s := &ScheduleEntry{
		Label:   "weekly",
		Cron:    "0 9 * * 1",
		Branch:  "dev",
		Message: "Weekly run",
		Env:     map[string]string{"FOO": "bar"},
		Enabled: &disabled,
	}
	req := toScheduleReq(s, "main")

	if req.Branch != "dev" {
		t.Errorf("Branch = %q, want %q", req.Branch, "dev")
	}
	if req.Enabled {
		t.Errorf("Enabled = true, want false")
	}
	if req.Message != "Weekly run" {
		t.Errorf("Message = %q, want %q", req.Message, "Weekly run")
	}
	if req.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want %q", req.Env["FOO"], "bar")
	}
}

func TestScheduleYAMLParsing(t *testing.T) {
	t.Parallel()
	input := `
on:
  schedule:
    - label: "Nightly build"
      cron: "0 2 * * *"
      branch: main
      message: "Nightly run"
      env:
        FOO: bar
`
	var pf PipelineFile
	if err := yaml.Unmarshal([]byte(input), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pf.On == nil {
		t.Fatal("On is nil")
	}
	if pf.On.Schedule == nil {
		t.Fatal("Schedule is nil, want non-nil")
	}
	if len(*pf.On.Schedule) != 1 {
		t.Fatalf("len(Schedule) = %d, want 1", len(*pf.On.Schedule))
	}
	s := (*pf.On.Schedule)[0]
	if s.Label != "Nightly build" {
		t.Errorf("Label = %q, want %q", s.Label, "Nightly build")
	}
	if s.Cron != "0 2 * * *" {
		t.Errorf("Cron = %q, want %q", s.Cron, "0 2 * * *")
	}
	if s.Branch != "main" {
		t.Errorf("Branch = %q, want %q", s.Branch, "main")
	}
	if s.Message != "Nightly run" {
		t.Errorf("Message = %q, want %q", s.Message, "Nightly run")
	}
	if s.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want %q", s.Env["FOO"], "bar")
	}
}

func TestScheduleYAMLWithOtherTriggers(t *testing.T) {
	t.Parallel()
	input := `
on:
  push:
    branches: [main]
  schedule:
    - label: "Nightly"
      cron: "0 2 * * *"
    - label: "Weekly"
      cron: "0 9 * * 1"
`
	var pf PipelineFile
	if err := yaml.Unmarshal([]byte(input), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pf.On.Push == nil {
		t.Fatal("Push is nil")
	}
	if pf.On.Schedule == nil {
		t.Fatal("Schedule is nil, want non-nil")
	}
	if len(*pf.On.Schedule) != 2 {
		t.Fatalf("len(Schedule) = %d, want 2", len(*pf.On.Schedule))
	}
	if (*pf.On.Schedule)[0].Label != "Nightly" {
		t.Errorf("Schedule[0].Label = %q, want %q", (*pf.On.Schedule)[0].Label, "Nightly")
	}
	if (*pf.On.Schedule)[1].Label != "Weekly" {
		t.Errorf("Schedule[1].Label = %q, want %q", (*pf.On.Schedule)[1].Label, "Weekly")
	}
}

func TestScheduleYAMLOptionalFields(t *testing.T) {
	t.Parallel()
	input := `
on:
  schedule:
    - label: "Minimal"
      cron: "0 * * * *"
`
	var pf PipelineFile
	if err := yaml.Unmarshal([]byte(input), &pf); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if pf.On.Schedule == nil {
		t.Fatal("Schedule is nil, want non-nil")
	}
	s := (*pf.On.Schedule)[0]
	if s.Branch != "" {
		t.Errorf("Branch = %q, want empty", s.Branch)
	}
	if s.Enabled != nil {
		t.Errorf("Enabled = %v, want nil", *s.Enabled)
	}
	if s.Message != "" {
		t.Errorf("Message = %q, want empty", s.Message)
	}
	if len(s.Env) != 0 {
		t.Errorf("Env = %v, want empty", s.Env)
	}
}
