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
	"sync"

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
	On     *TriggerConfig `yaml:"on"`
	Builds *BuildsConfig  `yaml:"builds"`
}

// BuildsConfig holds pipeline-level build behavior overrides.
type BuildsConfig struct {
	SkipIntermediate   *bool  `yaml:"skip_intermediate"`
	CancelIntermediate *bool  `yaml:"cancel_intermediate"`
	BranchFilter       string `yaml:"branch_filter"`
	// Concurrency and ConcurrencyGroup gate the generated upload step. When set,
	// the upload step (the build's first job) waits on the concurrency group
	// instead of starting immediately, so the build stays scheduled and remains
	// skippable by skip_intermediate while an earlier build holds the lock.
	Concurrency      int    `yaml:"concurrency"`
	ConcurrencyGroup string `yaml:"concurrency_group"`
}

type TriggerConfig struct {
	Push     *PushTrigger
	PR       *PRTrigger
	Tag      *TagTrigger
	Schedule []*ScheduleEntry
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

type ScheduleEntry struct {
	Label   string            `yaml:"label"`
	Cron    string            `yaml:"cron"`
	Branch  string            `yaml:"branch"`
	Message string            `yaml:"message"`
	Env     map[string]string `yaml:"env"`
	Enabled *bool             `yaml:"enabled"`
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
		case "schedule":
			if val.Kind == yaml.SequenceNode {
				if err := val.Decode(&t.Schedule); err != nil {
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
	Name                            string                     `json:"name"`
	Description                     string                     `json:"description,omitempty"`
	Repository                      string                     `json:"repository"`
	DefaultBranch                   string                     `json:"default_branch"`
	Configuration                   string                     `json:"configuration"`
	BranchConfiguration             string                     `json:"branch_configuration,omitempty"`
	ClusterID                       string                     `json:"cluster_id,omitempty"`
	TeamUUIDs                       []string                   `json:"team_uuids,omitempty"`
	ProviderSettings                *BuildkiteProviderSettings `json:"provider_settings,omitempty"`
	SkipQueuedBranchBuilds          bool                       `json:"skip_queued_branch_builds"`
	SkipQueuedBranchBuildsFilter    string                     `json:"skip_queued_branch_builds_filter,omitempty"`
	CancelRunningBranchBuilds       bool                       `json:"cancel_running_branch_builds"`
	CancelRunningBranchBuildsFilter string                     `json:"cancel_running_branch_builds_filter,omitempty"`
}

type BuildkiteUpdatePipelineReq struct {
	Description                     string                     `json:"description,omitempty"`
	Configuration                   string                     `json:"configuration,omitempty"`
	BranchConfiguration             string                     `json:"branch_configuration,omitempty"`
	ProviderSettings                *BuildkiteProviderSettings `json:"provider_settings,omitempty"`
	SkipQueuedBranchBuilds          bool                       `json:"skip_queued_branch_builds"`
	SkipQueuedBranchBuildsFilter    string                     `json:"skip_queued_branch_builds_filter,omitempty"`
	CancelRunningBranchBuilds       bool                       `json:"cancel_running_branch_builds"`
	CancelRunningBranchBuildsFilter string                     `json:"cancel_running_branch_builds_filter,omitempty"`
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

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, entry := range pipelines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := syncPipeline(ctx, cfg, entry); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	log.Println("Done.")
	return nil
}

func syncPipeline(ctx context.Context, cfg Config, entry pipelineEntry) error {
	pf := entry.File
	filename := entry.Filename
	name := strings.TrimSuffix(strings.TrimSuffix(filename, ".yaml"), ".yml")

	if pf.On == nil {
		log.Printf("[%s] skip: no 'on' block", filename)
		return nil
	}

	pipelineName := cfg.PipelinePrefix + name
	slug := toSlug(pipelineName)
	logger := log.New(log.Writer(), fmt.Sprintf("[%s] ", slug), 0)

	fileURL := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s/%s", cfg.GitHubOwner, cfg.GitHubRepo, cfg.DefaultBranch, cfg.PipelinesDir, filename)
	description := fmt.Sprintf("%s/%s %s: %s", cfg.GitHubOwner, cfg.GitHubRepo, filename, triggerNames(pf.On))
	var uploadConcurrency int
	var uploadConcurrencyGroup string
	if pf.Builds != nil {
		uploadConcurrency = pf.Builds.Concurrency
		uploadConcurrencyGroup = pf.Builds.ConcurrencyGroup
	}
	bootstrap := bootstrapConfig(cfg.PipelinesDir, filename, fileURL, uploadConcurrency, uploadConcurrencyGroup)
	pipelineCfg := buildPipelineConfig(pf)
	branchConfig := buildBranchConfiguration(pf.On)

	if pf.On.Push == nil && pf.On.PR == nil && pf.On.Tag == nil {
		if len(pf.On.Schedule) > 0 {
			logger.Printf("no GitHub triggers — schedule only (%d)", len(pf.On.Schedule))
		} else {
			logger.Printf("no GitHub triggers — pipeline will only be triggered via API")
		}
	} else {
		logger.Printf("push=%v pr=%v tag=%v schedules=%d", pf.On.Push != nil, pf.On.PR != nil, pf.On.Tag != nil, len(pf.On.Schedule))
	}

	if cfg.DryRun {
		logger.Printf("[dry-run] would create/update pipeline %q", pipelineName)
		logger.Printf("[dry-run] description: %s", description)
		logger.Printf("[dry-run] provider_settings: build_branches=%v build_pull_requests=%v build_tags=%v",
			pipelineCfg.providerSettings.BuildBranches, pipelineCfg.providerSettings.BuildPullRequests, pipelineCfg.providerSettings.BuildTags)
		if pipelineCfg.providerSettings.FilterEnabled {
			logger.Printf("[dry-run] filter_condition: %s", pipelineCfg.providerSettings.FilterCondition)
		}
		logger.Printf("[dry-run] skip_queued_branch_builds=%v (filter=%q) cancel_running_branch_builds=%v (filter=%q)",
			pipelineCfg.skipQueuedBuilds, pipelineCfg.skipQueuedBuildsFilter,
			pipelineCfg.cancelRunningBuilds, pipelineCfg.cancelRunningBuildsFilter)
		logger.Printf("[dry-run] bootstrap configuration:\n%s", bootstrap)
		for _, s := range pf.On.Schedule {
			req := toScheduleReq(s, cfg.DefaultBranch)
			logger.Printf("[dry-run] schedule %q: %s on %s (enabled=%v)", req.Label, req.Cronline, req.Branch, req.Enabled)
		}
		return nil
	}

	_, err := getBuildkitePipeline(ctx, cfg, slug)
	var pipeline BuildkitePipelineResp
	if err == nil {
		logger.Printf("updating existing pipeline")
		pipeline, err = updateBuildkitePipeline(ctx, cfg, slug, description, bootstrap, branchConfig, pipelineCfg)
		if err != nil {
			return fmt.Errorf("updating pipeline %s: %w", filename, err)
		}
	} else if errors.Is(err, errNotFound) {
		logger.Printf("creating new pipeline %q", pipelineName)
		pipeline, err = createBuildkitePipeline(ctx, cfg, pipelineName, description, bootstrap, branchConfig, pipelineCfg)
		if err != nil {
			return fmt.Errorf("creating pipeline %s: %w", filename, err)
		}
	} else {
		return fmt.Errorf("looking up pipeline %s: %w", slug, err)
	}
	logger.Printf("URL: %s", pipeline.WebURL)

	if err := syncSchedules(ctx, cfg, logger, pipeline.Slug, pf.On.Schedule, cfg.DefaultBranch); err != nil {
		return fmt.Errorf("syncing schedules for %s: %w", filename, err)
	}

	if pf.On.Push != nil || pf.On.PR != nil || pf.On.Tag != nil {
		logger.Printf("registering GitHub webhook...")
		if err = createBuildkiteWebhook(ctx, cfg, pipeline.Slug); err != nil {
			logger.Printf("warning: could not register webhook: %v (may already be registered)", err)
		} else {
			logger.Printf("webhook registered")
		}
	} else {
		logger.Printf("skipping webhook registration (no GitHub triggers)")
	}
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
func bootstrapConfig(dir, filename, fileURL string, concurrency int, concurrencyGroup string) string {
	path := dir + "/" + filename
	cmd := fmt.Sprintf(
		`if [ -f %s ]; then buildkite-agent pipeline upload %s; elif [ "${BUILDKITE_PULL_REQUEST}" != "false" ]; then echo "Pipeline file %s not found on this PR branch, skipping."; else echo "Pipeline file %s not found!" && exit 1; fi`,
		path, path, path, path,
	)
	var b strings.Builder
	fmt.Fprintf(&b, "steps:\n  - label: \":pipeline: Upload pipeline — %s\"\n    command: %q\n", fileURL, cmd)
	// Gate the first job on the concurrency group so a newer build's upload stays
	// queued (scheduled, not running) behind the active build. An un-gated upload
	// starts immediately, flips the build to running, and defeats skip_intermediate.
	if concurrencyGroup != "" {
		if concurrency < 1 {
			concurrency = 1
		}
		fmt.Fprintf(&b, "    concurrency: %d\n    concurrency_group: %q\n", concurrency, concurrencyGroup)
	}
	return b.String()
}

const defaultIntermediateBuildsBranchFilter = "!main !dev"

type pipelineConfig struct {
	providerSettings          *BuildkiteProviderSettings
	skipQueuedBuilds          bool
	skipQueuedBuildsFilter    string
	cancelRunningBuilds       bool
	cancelRunningBuildsFilter string
}

func buildPipelineConfig(pf *PipelineFile) pipelineConfig {
	on := pf.On
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

	// Enable skip/cancel of intermediate builds by default, excluding protected branches.
	skipQueued := true
	cancelRunning := true
	branchFilter := defaultIntermediateBuildsBranchFilter
	if pf.Builds != nil {
		if pf.Builds.SkipIntermediate != nil {
			skipQueued = *pf.Builds.SkipIntermediate
		}
		if pf.Builds.CancelIntermediate != nil {
			cancelRunning = *pf.Builds.CancelIntermediate
		}
		if pf.Builds.BranchFilter != "" {
			branchFilter = pf.Builds.BranchFilter
		}
	}

	pc := pipelineConfig{
		providerSettings:    ps,
		skipQueuedBuilds:    skipQueued,
		cancelRunningBuilds: cancelRunning,
	}
	if skipQueued {
		pc.skipQueuedBuildsFilter = branchFilter
	}
	if cancelRunning {
		pc.cancelRunningBuildsFilter = branchFilter
	}
	return pc
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
	if len(on.Schedule) > 0 {
		names = append(names, fmt.Sprintf("schedule(%d)", len(on.Schedule)))
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

func createBuildkitePipeline(ctx context.Context, cfg Config, name, description, configuration, branchConfiguration string, pc pipelineConfig) (BuildkitePipelineResp, error) {
	payload := BuildkiteCreatePipelineReq{
		Name:                            name,
		Description:                     description,
		Repository:                      cfg.RepoURL,
		DefaultBranch:                   cfg.DefaultBranch,
		Configuration:                   configuration,
		BranchConfiguration:             branchConfiguration,
		ClusterID:                       cfg.ClusterID,
		ProviderSettings:                pc.providerSettings,
		SkipQueuedBranchBuilds:          pc.skipQueuedBuilds,
		SkipQueuedBranchBuildsFilter:    pc.skipQueuedBuildsFilter,
		CancelRunningBranchBuilds:       pc.cancelRunningBuilds,
		CancelRunningBranchBuildsFilter: pc.cancelRunningBuildsFilter,
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

func updateBuildkitePipeline(ctx context.Context, cfg Config, slug, description, configuration, branchConfiguration string, pc pipelineConfig) (BuildkitePipelineResp, error) {
	payload := BuildkiteUpdatePipelineReq{
		Description:                     description,
		Configuration:                   configuration,
		BranchConfiguration:             branchConfiguration,
		ProviderSettings:                pc.providerSettings,
		SkipQueuedBranchBuilds:          pc.skipQueuedBuilds,
		SkipQueuedBranchBuildsFilter:    pc.skipQueuedBuildsFilter,
		CancelRunningBranchBuilds:       pc.cancelRunningBuilds,
		CancelRunningBranchBuildsFilter: pc.cancelRunningBuildsFilter,
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

// --- Buildkite Schedules API ---

type BuildkiteScheduleReq struct {
	Label   string            `json:"label"`
	Cronline string           `json:"cronline"`
	Branch  string            `json:"branch,omitempty"`
	Message string            `json:"message,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Enabled bool              `json:"enabled"`
}

type BuildkiteScheduleResp struct {
	ID      string            `json:"id"`
	Label   string            `json:"label"`
	Cronline string           `json:"cronline"`
	Branch  string            `json:"branch"`
	Message string            `json:"message"`
	Env     map[string]string `json:"env"`
	Enabled bool              `json:"enabled"`
}

func toScheduleReq(s *ScheduleEntry, defaultBranch string) BuildkiteScheduleReq {
	enabled := true
	if s.Enabled != nil {
		enabled = *s.Enabled
	}
	branch := s.Branch
	if branch == "" {
		branch = defaultBranch
	}
	return BuildkiteScheduleReq{
		Label:   s.Label,
		Cronline: s.Cron,
		Branch:  branch,
		Message: s.Message,
		Env:     s.Env,
		Enabled: enabled,
	}
}

func syncSchedules(ctx context.Context, cfg Config, logger *log.Logger, slug string, schedules []*ScheduleEntry, defaultBranch string) error {
	existing, err := listBuildkiteSchedules(ctx, cfg, slug)
	if err != nil {
		return fmt.Errorf("listing schedules: %w", err)
	}

	existingByLabel := make(map[string]BuildkiteScheduleResp, len(existing))
	for _, s := range existing {
		existingByLabel[s.Label] = s
	}

	seen := make(map[string]bool, len(schedules))
	for _, s := range schedules {
		seen[s.Label] = true
		req := toScheduleReq(s, defaultBranch)
		if ex, ok := existingByLabel[s.Label]; ok {
			logger.Printf("updating schedule %q", s.Label)
			if _, err := updateBuildkiteSchedule(ctx, cfg, slug, ex.ID, req); err != nil {
				return fmt.Errorf("updating schedule %q: %w", s.Label, err)
			}
		} else {
			logger.Printf("creating schedule %q", s.Label)
			if _, err := createBuildkiteSchedule(ctx, cfg, slug, req); err != nil {
				return fmt.Errorf("creating schedule %q: %w", s.Label, err)
			}
		}
	}

	for _, ex := range existing {
		if !seen[ex.Label] {
			logger.Printf("deleting schedule %q", ex.Label)
			if err := deleteBuildkiteSchedule(ctx, cfg, slug, ex.ID); err != nil {
				return fmt.Errorf("deleting schedule %q: %w", ex.Label, err)
			}
		}
	}

	return nil
}

func listBuildkiteSchedules(ctx context.Context, cfg Config, slug string) ([]BuildkiteScheduleResp, error) {
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s/schedules",
		cfg.BuildkiteOrg, slug)
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Buildkite list schedules: %s: %s", resp.Status, body)
	}

	var schedules []BuildkiteScheduleResp
	if err := json.Unmarshal(body, &schedules); err != nil {
		return nil, err
	}
	return schedules, nil
}

func createBuildkiteSchedule(ctx context.Context, cfg Config, slug string, s BuildkiteScheduleReq) (BuildkiteScheduleResp, error) {
	body, _ := json.Marshal(s)
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s/schedules",
		cfg.BuildkiteOrg, slug)
	req, _ := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BuildkiteScheduleResp{}, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 201 {
		return BuildkiteScheduleResp{}, fmt.Errorf("Buildkite create schedule: %s: %s", resp.Status, respBody)
	}

	var schedule BuildkiteScheduleResp
	if err := json.Unmarshal(respBody, &schedule); err != nil {
		return BuildkiteScheduleResp{}, err
	}
	return schedule, nil
}

func updateBuildkiteSchedule(ctx context.Context, cfg Config, slug, id string, s BuildkiteScheduleReq) (BuildkiteScheduleResp, error) {
	body, _ := json.Marshal(s)
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s/schedules/%s",
		cfg.BuildkiteOrg, slug, id)
	req, _ := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return BuildkiteScheduleResp{}, err
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		return BuildkiteScheduleResp{}, fmt.Errorf("Buildkite update schedule: %s: %s", resp.Status, respBody)
	}

	var schedule BuildkiteScheduleResp
	if err := json.Unmarshal(respBody, &schedule); err != nil {
		return BuildkiteScheduleResp{}, err
	}
	return schedule, nil
}

func deleteBuildkiteSchedule(ctx context.Context, cfg Config, slug, id string) error {
	url := fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s/schedules/%s",
		cfg.BuildkiteOrg, slug, id)
	req, _ := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	req.Header.Set("Authorization", "Bearer "+cfg.BuildkiteToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 204 {
		return fmt.Errorf("Buildkite delete schedule: %s: %s", resp.Status, body)
	}
	return nil
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
