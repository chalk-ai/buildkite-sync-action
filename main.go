package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// PipelineFile represents a Buildkite pipeline YAML with an optional `on:` block.
//
// Example:
//
//	on:
//	  push:
//	    branches: ["main"]
//	  pull_request: {}
//
//	steps:
//	  - label: ":go: tests"
//	    command: go test ./...
type PipelineFile struct {
	On *TriggerConfig `yaml:"on"`
}

type TriggerConfig struct {
	Push *PushTrigger
	PR   *PRTrigger
	Tag  *TagTrigger
}

type PushTrigger struct {
	Branches []string `yaml:"branches"`
}

type PRTrigger struct {
	BranchFilter      string `yaml:"branch_filter"`
	ConditionalFilter string `yaml:"conditional_filter"`
}

type TagTrigger struct {
	BranchFilter      string `yaml:"branch_filter"`
	ConditionalFilter string `yaml:"conditional_filter"`
}

// UnmarshalYAML handles various forms of trigger config:
//   - `pull_request: {}` or `pull_request:` → empty PRTrigger
//   - `push: {}` or `push:` → empty PushTrigger
//   - `tag: {}` or `tag:` → empty TagTrigger
func (t *TriggerConfig) UnmarshalYAML(value *yaml.Node) error {
	for i := 0; i < len(value.Content)-1; i += 2 {
		key := value.Content[i].Value
		val := value.Content[i+1]

		switch key {
		case "push":
			t.Push = &PushTrigger{}
			if val.Kind == yaml.MappingNode {
				if err := val.Decode(t.Push); err != nil {
					return err
				}
			}
		case "pr", "pull_request":
			t.PR = &PRTrigger{}
			if val.Kind == yaml.MappingNode {
				if err := val.Decode(t.PR); err != nil {
					return err
				}
			}
		case "tag":
			t.Tag = &TagTrigger{}
			if val.Kind == yaml.MappingNode {
				if err := val.Decode(t.Tag); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// BuildkiteProviderSettings maps to the Buildkite API provider_settings for GitHub.
type BuildkiteProviderSettings struct {
	TriggerMode                    string `json:"trigger_mode"`
	BuildBranches                  bool   `json:"build_branches"`
	BuildPullRequests              bool   `json:"build_pull_requests"`
	BuildTags                      bool   `json:"build_tags"`
	PublishCommitStatus            bool   `json:"publish_commit_status"`
	PublishCommitStatusPerStep     bool   `json:"publish_commit_status_per_step"`
	FilterEnabled                  bool   `json:"filter_enabled"`
	FilterCondition                string `json:"filter_condition"`
	PullRequestBranchFilterEnabled bool   `json:"pull_request_branch_filter_enabled"`
	PullRequestBranchFilterConfig  string `json:"pull_request_branch_filter_configuration"`
	CancelDeletedBranchBuilds      bool   `json:"cancel_deleted_branch_builds"`
	SkipPRBuildsForExistingCommits bool   `json:"skip_pull_request_builds_for_existing_commits"`
}

type BuildkiteCreatePipelineReq struct {
	Name                string                     `json:"name"`
	Description         string                     `json:"description,omitempty"`
	Repository          string                     `json:"repository"`
	DefaultBranch       string                     `json:"default_branch"`
	Configuration       string                     `json:"configuration"`
	BranchConfiguration string                     `json:"branch_configuration,omitempty"`
	ClusterID           string                     `json:"cluster_id,omitempty"`
	TeamUUIDs           []string                   `json:"team_uuids,omitempty"`
	ProviderSettings    *BuildkiteProviderSettings `json:"provider_settings,omitempty"`
}

type BuildkiteUpdatePipelineReq struct {
	Description         string                     `json:"description,omitempty"`
	Configuration       string                     `json:"configuration,omitempty"`
	BranchConfiguration string                     `json:"branch_configuration,omitempty"`
	ProviderSettings    *BuildkiteProviderSettings `json:"provider_settings,omitempty"`
}

type BuildkitePipelineResp struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	WebURL   string `json:"web_url"`
	Provider struct {
		ID         string `json:"id"`
		WebhookURL string `json:"webhook_url"`
	} `json:"provider"`
}

type Config struct {
	BuildkiteOrg   string
	BuildkiteToken string
	GitHubOwner    string
	GitHubRepo     string
	RepoURL        string
	DefaultBranch  string
	ClusterID      string
	TeamUUID       string
	PipelinesDir   string
	PipelinePrefix string
	DryRun         bool
}

type pipelineEntry struct {
	File     *PipelineFile
	Filename string // original filename with extension, e.g. "pr.yml"
}

func main() {
	log.SetFlags(0)

	var cfg Config

	flag.StringVar(&cfg.PipelinesDir, "dir", ".buildkite", "Path to directory containing pipeline YAML files")
	flag.StringVar(&cfg.BuildkiteOrg, "org", envOrDefault("BUILDKITE_ORG", "chalk"), "Buildkite organization slug")
	flag.StringVar(&cfg.DefaultBranch, "default-branch", envOrDefault("DEFAULT_BRANCH", "main"), "Default branch for pipelines")
	flag.StringVar(&cfg.ClusterID, "cluster-id", os.Getenv("BUILDKITE_CLUSTER_ID"), "Buildkite cluster ID")
	flag.StringVar(&cfg.TeamUUID, "team-uuid", os.Getenv("BUILDKITE_TEAM_UUID"), "Buildkite team UUID to assign newly created pipelines to")
	flag.StringVar(&cfg.PipelinePrefix, "prefix", "", "Prefix for pipeline names (e.g. 'chalk-router-')")
	flag.BoolVar(&cfg.DryRun, "dry-run", false, "Print what would be done without making changes")

	var repo, workDir string
	flag.StringVar(&repo, "repo", "", "GitHub repository as owner/repo (e.g. chalk-ai/chalk-router)")
	flag.StringVar(&workDir, "work-dir", "", "Working directory (repo root); defaults to current directory")

	flag.Parse()

	if workDir != "" {
		if err := os.Chdir(workDir); err != nil {
			log.Fatalf("changing to work-dir %s: %v", workDir, err)
		}
	}

	cfg.BuildkiteToken = os.Getenv("BUILDKITE_API_TOKEN")

	if cfg.BuildkiteToken == "" {
		log.Fatal("BUILDKITE_API_TOKEN environment variable is required")
	}
	if cfg.ClusterID == "" {
		log.Fatal("-cluster-id flag or BUILDKITE_CLUSTER_ID environment variable is required")
	}

	if repo == "" {
		repo = os.Getenv("GITHUB_REPOSITORY")
	}
	if repo == "" {
		log.Fatal("--repo flag or GITHUB_REPOSITORY env var is required (format: owner/repo)")
	}
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) != 2 {
		log.Fatalf("--repo must be in owner/repo format, got %q", repo)
	}
	cfg.GitHubOwner = parts[0]
	cfg.GitHubRepo = parts[1]
	cfg.RepoURL = fmt.Sprintf("git@github.com:%s/%s.git", cfg.GitHubOwner, cfg.GitHubRepo)

	if cfg.PipelinePrefix == "" {
		cfg.PipelinePrefix = cfg.GitHubRepo + "-"
	}

	if err := run(context.Background(), cfg); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, cfg Config) error {
	pipelines, err := discoverPipelines(cfg.PipelinesDir)
	if err != nil {
		return fmt.Errorf("discovering pipelines: %w", err)
	}
	if len(pipelines) == 0 {
		log.Printf("No pipeline files found in %s", cfg.PipelinesDir)
		return nil
	}

	log.Printf("Found %d pipeline file(s) in %s", len(pipelines), cfg.PipelinesDir)
	log.Printf("Target: %s/%s (org: %s, prefix: %q)", cfg.GitHubOwner, cfg.GitHubRepo, cfg.BuildkiteOrg, cfg.PipelinePrefix)

	for _, entry := range pipelines {
		pf := entry.File
		filename := entry.Filename
		name := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")

		if pf.On == nil {
			log.Printf("  [skip] %s: no 'on' block", filename)
			continue
		}

		pipelineName := cfg.PipelinePrefix + name
		slug := toSlug(pipelineName)
		fileURL := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s/%s", cfg.GitHubOwner, cfg.GitHubRepo, cfg.DefaultBranch, cfg.PipelinesDir, filename)
		description := fmt.Sprintf("%s/%s %s: %s", cfg.GitHubOwner, cfg.GitHubRepo, filename, triggerNames(pf.On))
		bootstrap := bootstrapConfig(cfg.PipelinesDir, filename, fileURL)
		providerSettings := buildProviderSettings(pf.On)
		branchConfig := buildBranchConfiguration(pf.On)

		log.Printf("  [sync] %s -> pipeline %q (slug=%s)", filename, pipelineName, slug)
		if pf.On.Push == nil && pf.On.PR == nil && pf.On.Tag == nil {
			log.Printf("         no GitHub triggers — pipeline will only be triggered via API")
		} else {
			log.Printf("         push=%v pr=%v tag=%v", pf.On.Push != nil, pf.On.PR != nil, pf.On.Tag != nil)
		}

		if cfg.DryRun {
			log.Printf("    [dry-run] would create/update pipeline %q", pipelineName)
			log.Printf("    [dry-run] description: %s", description)
			log.Printf("    [dry-run] provider_settings: build_branches=%v build_pull_requests=%v build_tags=%v",
				providerSettings.BuildBranches, providerSettings.BuildPullRequests, providerSettings.BuildTags)
			if providerSettings.FilterEnabled {
				log.Printf("    [dry-run] filter_condition: %s", providerSettings.FilterCondition)
			}
			log.Printf("    [dry-run] bootstrap configuration:\n%s", bootstrap)
			continue
		}

		_, err := getBuildkitePipeline(ctx, cfg, slug)
		var pipeline BuildkitePipelineResp
		if err == nil {
			log.Printf("    Updating existing pipeline %q", slug)
			pipeline, err = updateBuildkitePipeline(ctx, cfg, slug, description, bootstrap, branchConfig, providerSettings)
			if err != nil {
				return fmt.Errorf("updating pipeline %s: %w", filename, err)
			}
		} else if errors.Is(err, errNotFound) {
			log.Printf("    Creating new pipeline %q", pipelineName)
			pipeline, err = createBuildkitePipeline(ctx, cfg, pipelineName, description, bootstrap, branchConfig, providerSettings)
			if err != nil {
				return fmt.Errorf("creating pipeline %s: %w", filename, err)
			}
		} else {
			return fmt.Errorf("looking up pipeline %s: %w", slug, err)
		}
		log.Printf("    Pipeline URL: %s", pipeline.WebURL)

		if pf.On.Push != nil || pf.On.PR != nil || pf.On.Tag != nil {
			log.Printf("    Registering GitHub webhook via Buildkite...")
			if err = createBuildkiteWebhook(ctx, cfg, pipeline.Slug); err != nil {
				log.Printf("    Warning: could not register webhook for %s: %v (webhook may already be registered)", slug, err)
			} else {
				log.Printf("    Webhook registered.")
			}
		} else {
			log.Printf("    Skipping webhook registration (no GitHub triggers).")
		}
	}

	log.Println("Done.")
	return nil
}

// discoverPipelines finds all .yml/.yaml files in dir and parses their `on:` block.
func discoverPipelines(dir string) ([]pipelineEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	var pipelines []pipelineEntry
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		ext := filepath.Ext(e.Name())
		if ext != ".yml" && ext != ".yaml" {
			continue
		}

		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", e.Name(), err)
		}

		var pf PipelineFile
		if err := yaml.Unmarshal(data, &pf); err != nil {
			log.Printf("Warning: could not parse %s: %v", e.Name(), err)
			continue
		}

		pipelines = append(pipelines, pipelineEntry{File: &pf, Filename: e.Name()})
	}
	return pipelines, nil
}

// bootstrapConfig returns a minimal pipeline configuration that uploads the real
// pipeline steps from the repo at build time.
//
// The upload command gracefully skips on PR branches when the pipeline file doesn't
// exist — this handles the case where a pipeline was created on another branch that
// the current PR branch hasn't merged yet. On non-PR builds (e.g. main) a missing
// file is still an error.
func bootstrapConfig(dir, filename, fileURL string) string {
	path := dir + "/" + filename
	cmd := fmt.Sprintf(
		`if [ -f %s ]; then buildkite-agent pipeline upload %s; elif [ "${BUILDKITE_PULL_REQUEST}" != "false" ]; then echo "Pipeline file %s not found on this PR branch, skipping."; else echo "Pipeline file %s not found!" && exit 1; fi`,
		path, path, path, path,
	)
	return fmt.Sprintf("steps:\n  - label: \":pipeline: Upload pipeline — %s\"\n    command: %q\n", fileURL, cmd)
}

func buildProviderSettings(on *TriggerConfig) *BuildkiteProviderSettings {
	ps := &BuildkiteProviderSettings{
		TriggerMode:                    "code",
		PublishCommitStatus:            true,
		CancelDeletedBranchBuilds:      true,
		SkipPRBuildsForExistingCommits: true,
	}

	if on.Push != nil {
		ps.BuildBranches = true
	}

	if on.PR != nil {
		ps.BuildPullRequests = true
		if on.PR.BranchFilter != "" {
			ps.PullRequestBranchFilterEnabled = true
			ps.PullRequestBranchFilterConfig = on.PR.BranchFilter
		}
		if on.PR.ConditionalFilter != "" {
			ps.FilterEnabled = true
			ps.FilterCondition = on.PR.ConditionalFilter
		}
	}

	if on.Tag != nil {
		ps.BuildTags = true
		if on.Tag.ConditionalFilter != "" {
			ps.FilterEnabled = true
			ps.FilterCondition = on.Tag.ConditionalFilter
		}
	}

	return ps
}

// buildBranchConfiguration returns the top-level branch_configuration glob for the pipeline.
// For push triggers this restricts which branches trigger builds (space-separated glob patterns).
// For tag triggers this restricts which tags trigger builds.
func buildBranchConfiguration(on *TriggerConfig) string {
	if on.Tag != nil {
		return on.Tag.BranchFilter
	}
	if on.Push != nil && len(on.Push.Branches) > 0 {
		return strings.Join(on.Push.Branches, " ")
	}
	return ""
}

func triggerNames(on *TriggerConfig) string {
	var names []string
	if on.Push != nil {
		names = append(names, "push")
	}
	if on.PR != nil {
		names = append(names, "pull_request")
	}
	if on.Tag != nil {
		names = append(names, "tag")
	}
	if len(names) == 0 {
		return "api"
	}
	return strings.Join(names, ", ")
}

func toSlug(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, "_", "-")
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

// errNotFound is returned by getBuildkitePipeline when the pipeline does not exist.
var errNotFound = errors.New("not found")

// --- Buildkite API ---

func createBuildkitePipeline(ctx context.Context, cfg Config, name, description, configuration, branchConfiguration string, ps *BuildkiteProviderSettings) (BuildkitePipelineResp, error) {
	payload := BuildkiteCreatePipelineReq{
		Name:                name,
		Description:         description,
		Repository:          cfg.RepoURL,
		DefaultBranch:       cfg.DefaultBranch,
		Configuration:       configuration,
		BranchConfiguration: branchConfiguration,
		ClusterID:           cfg.ClusterID,
		ProviderSettings:    ps,
	}
	if cfg.TeamUUID != "" {
		payload.TeamUUIDs = []string{cfg.TeamUUID}
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines", cfg.BuildkiteOrg)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BuildkitePipelineResp{}, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 201 {
		return BuildkitePipelineResp{}, fmt.Errorf("Buildkite create pipeline: %s: %s", resp.Status, respBody)
	}

	var pipeline BuildkitePipelineResp
	if err := json.Unmarshal(respBody, &pipeline); err != nil {
		return BuildkitePipelineResp{}, err
	}
	return pipeline, nil
}

func updateBuildkitePipeline(ctx context.Context, cfg Config, slug, description, configuration, branchConfiguration string, ps *BuildkiteProviderSettings) (BuildkitePipelineResp, error) {
	payload := BuildkiteUpdatePipelineReq{
		Description:         description,
		Configuration:       configuration,
		BranchConfiguration: branchConfiguration,
		ProviderSettings:    ps,
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s", cfg.BuildkiteOrg, slug)
	req, _ := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BuildkitePipelineResp{}, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return BuildkitePipelineResp{}, fmt.Errorf("Buildkite update pipeline: %s: %s", resp.Status, respBody)
	}

	var pipeline BuildkitePipelineResp
	if err := json.Unmarshal(respBody, &pipeline); err != nil {
		return BuildkitePipelineResp{}, err
	}
	return pipeline, nil
}

func createBuildkiteWebhook(ctx context.Context, cfg Config, slug string) error {
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s/webhook",
		cfg.BuildkiteOrg, slug)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		return fmt.Errorf("Buildkite create webhook: %s: %s", resp.Status, respBody)
	}
	return nil
}

func getBuildkitePipeline(ctx context.Context, cfg Config, slug string) (BuildkitePipelineResp, error) {
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s",
		cfg.BuildkiteOrg, slug)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BuildkitePipelineResp{}, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode == 404 {
		return BuildkitePipelineResp{}, errNotFound
	}
	if resp.StatusCode != 200 {
		return BuildkitePipelineResp{}, fmt.Errorf("Buildkite get pipeline: %s: %s", resp.Status, body)
	}

	var pipeline BuildkitePipelineResp
	if err := json.Unmarshal(body, &pipeline); err != nil {
		return BuildkitePipelineResp{}, err
	}
	return pipeline, nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
