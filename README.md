# buildkite-sync-action

A reusable composite GitHub Action that keeps Buildkite pipelines in sync with `.buildkite/` YAML files in your repo. When changes are merged to `main`, the action reconciles pipeline state automatically — no manual Buildkite UI interaction required after initial setup.

## How it works

Each `.buildkite/*.yml` file contains an `on:` trigger block (similar to GitHub Actions) plus the real pipeline steps. The action:

1. Parses the `on:` block to determine provider settings (which events trigger builds)
2. Creates or updates the Buildkite pipeline with a [dynamic pipeline](https://buildkite.com/docs/pipelines/configure/defining-steps#step-defaults-pipeline-dot-yml-file) bootstrap step — Buildkite stores only a minimal upload command; the agent reads the real steps from the repo at build time
3. Calls Buildkite's webhook API to ensure the GitHub webhook is registered

This means changes to pipeline steps take effect on the next build automatically, with no re-run of this action needed.

## Usage

### Workflow file

```yaml
name: Sync Buildkite Pipelines

on:
  push:
    branches: [main]
    paths:
      - '.buildkite/**'
  workflow_dispatch:

jobs:
  sync:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: chalk-ai/buildkite-sync-action@v1
        with:
          cluster-id: ${{ vars.BUILDKITE_CLUSTER_ID }}
        env:
          BUILDKITE_API_TOKEN: ${{ secrets.BUILDKITE_API_TOKEN }}
```

### Pipeline files

Each file in `.buildkite/` must include an `on:` block declaring what triggers it.

**Pull request builds:**
```yaml
# .buildkite/pr.yml
on:
  pull_request: {}
  # Optional filters:
  # branch_filter: "main"         # only PRs targeting this branch (glob)
  # conditional_filter: 'build.branch != "main"'  # Buildkite condition expression

steps:
  - label: ":go: Tests"
    command: go test ./...
```

**Tag builds:**
```yaml
# .buildkite/release.yml
on:
  tag:
    branch_filter: "v*"                              # only tags matching this glob
    conditional_filter: 'build.tag =~ /^v\d+\.\d+(\.\d+)?$/'  # Buildkite condition expression

steps:
  - label: ":rocket: Release"
    command: goreleaser release --clean
```

**Branch push builds:**
```yaml
# .buildkite/deploy.yml
on:
  push:
    branches: [main, dev]   # only these branches trigger builds

steps:
  - label: ":ship: Deploy"
    command: ./scripts/deploy.sh
```

**API-only / managed pipeline (no automatic triggers):**
```yaml
# .buildkite/adhoc.yml
on: {}

steps:
  - label: ":wrench: Ad-hoc job"
    command: ./scripts/adhoc.sh
```

Using `on: {}` puts the pipeline under sync action management (creates/updates it) but configures no GitHub triggers — the pipeline will only run when triggered manually or via the Buildkite API.

**Scheduled builds:**
```yaml
# .buildkite/nightly.yml
on:
  schedule:
    - label: "Nightly"
      cron: "0 2 * * *"
      branch: main             # default: pipeline's default branch
      message: "Nightly run"  # optional build message
      env:                     # optional environment variables
        NIGHTLY: "true"

steps:
  - label: ":night_with_stars: Nightly job"
    command: ./scripts/nightly.sh
```

Schedules can be combined with other triggers. The `label` field is required and acts as the stable identity for idempotent syncs — renaming a label deletes the old schedule and creates a new one. Schedules not present in the YAML are deleted from Buildkite on the next sync.

Files without an `on:` block are skipped entirely and not managed by the action.

### Trigger options

| Trigger | Field | Description |
|---------|-------|-------------|
| `pull_request` | `branch_filter` | Glob pattern for target branch (e.g. `main`) |
| `pull_request` | `conditional_filter` | Buildkite [condition expression](https://buildkite.com/docs/pipelines/configure/conditionals) |
| `tag` | `branch_filter` | Glob pattern for tag name (e.g. `v*`) — sets `branch_configuration` on the pipeline |
| `tag` | `conditional_filter` | Buildkite condition expression (e.g. `build.tag =~ /^v\d+\.\d+/`) |
| `push` | `branches` | List of branches that trigger builds |
| `schedule` | `label` | Required. Stable identifier used for idempotent sync |
| `schedule` | `cron` | Required. Cron expression (e.g. `"0 2 * * *"`) |
| `schedule` | `branch` | Branch to build (default: pipeline's default branch) |
| `schedule` | `message` | Build message (optional) |
| `schedule` | `env` | Environment variables as key/value pairs (optional) |
| `schedule` | `enabled` | Whether the schedule is active (default: `true`) |

### Build behavior

An optional top-level `builds:` block controls intermediate build handling. By default, queued and running builds are cancelled/skipped when a new commit is pushed, except on `main` and `dev`.

```yaml
# .buildkite/pr.yml
on:
  pull_request: {}

builds:
  skip_intermediate: true        # default: true — skip queued builds on new push
  cancel_intermediate: true      # default: true — cancel running builds on new push
  branch_filter: "!main !dev"    # default: "!main !dev" — branches excluded from above
```

To disable this behaviour for a pipeline:

```yaml
builds:
  skip_intermediate: false
  cancel_intermediate: false
```

#### Gating the upload step

`skip_intermediate` only skips builds that are still in the `scheduled`/`creating` state. The generated upload step is the build's first job, so an agent normally starts it immediately and the build flips to `running` — at which point it can no longer be skipped. To keep newer builds skippable, gate the upload step on a concurrency group (typically the same group used by the real work step the upload publishes):

```yaml
builds:
  skip_intermediate: true
  cancel_intermediate: false       # let the running build finish
  branch_filter: "dev"
  concurrency: 1                   # default: 1 when concurrency_group is set
  concurrency_group: "my-pipeline/serialized"
```

With this, a newer build's upload waits in the concurrency queue behind the active build, so the build stays `scheduled` and `skip_intermediate` keeps only the most recent queued build. Combined with `cancel_intermediate: false`, the oldest running build finishes while at most one newer build waits.

### Pipeline naming

Pipelines are named `{repo-name}-{filename-without-ext}`. For example, `.buildkite/pr.yml` in `chalk-ai/chalk-router` becomes `chalk-router-pr`.

## Inputs

| Input | Required | Default | Description |
|-------|----------|---------|-------------|
| `cluster-id` | yes | — | Buildkite cluster ID |
| `org` | no | `chalk` | Buildkite organization slug |
| `dir` | no | `.buildkite` | Directory containing pipeline YAML files |
| `default-branch` | no | `main` | Default branch for created pipelines |
| `team-uuid` | no | — | Buildkite team UUID to assign newly created pipelines to |
| `dry-run` | no | `false` | Print planned actions without making changes |
| `upload-queue` | no | — | Agent queue to pin the generated "Upload pipeline" bootstrap step to (e.g. your smallest queue). Leave unset to keep the pipeline's default queue |

## Secrets and variables

Pass secrets via `env:` in the calling workflow (standard pattern for composite actions):

| Name | Type | Description |
|------|------|-------------|
| `BUILDKITE_API_TOKEN` | Secret | Buildkite API token with `read_pipelines` and `write_pipelines` scopes |
| `BUILDKITE_CLUSTER_ID` | Variable | Not sensitive — safe to store as a repo variable |
| `BUILDKITE_TEAM_UUID` | Variable | Not sensitive — safe to store as a repo variable |

## One-time setup per repo

1. **Secret** `BUILDKITE_API_TOKEN` — add in GitHub repo settings → Secrets
2. **Variable** `BUILDKITE_CLUSTER_ID` — add in GitHub repo settings → Variables
3. **Variable** `BUILDKITE_TEAM_UUID` *(if your org uses Teams)* — add in GitHub repo settings → Variables
4. Add `.buildkite/*.yml` files with `on:` blocks
5. Add the workflow file above

After that, merging changes to `.buildkite/` is all that's needed to keep Buildkite in sync.

## Dry run

To preview what the action would do without making any API calls:

```shell
BUILDKITE_API_TOKEN=<token> \
  go run main.go -dry-run -repo chalk-ai/my-repo -cluster-id <id>
```

## Development

```shell
make build       # build ./buildkite-sync-action
make pre-commit  # fmt, vet, fix, and test — run before committing
make release     # force-move the v1 tag to HEAD and push
```

See `make help` for the full list with a local chalk-private dry-run example.
