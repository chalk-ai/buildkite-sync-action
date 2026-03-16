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

Files without an `on:` block are skipped.

### Trigger options

| Trigger | Field | Description |
|---------|-------|-------------|
| `pull_request` | `branch_filter` | Glob pattern for target branch (e.g. `main`) |
| `pull_request` | `conditional_filter` | Buildkite [condition expression](https://buildkite.com/docs/pipelines/configure/conditionals) |
| `tag` | `branch_filter` | Glob pattern for tag name (e.g. `v*`) — sets `branch_configuration` on the pipeline |
| `tag` | `conditional_filter` | Buildkite condition expression (e.g. `build.tag =~ /^v\d+\.\d+/`) |
| `push` | `branches` | List of branches that trigger builds |

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
