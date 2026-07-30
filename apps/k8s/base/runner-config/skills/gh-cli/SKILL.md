---
name: gh-cli
description: "Use when working with GitHub repositories, pull requests, issues, Actions, releases, or search. Covers the gh CLI for everyday GitHub operations — PRs, issues, repos, CI, API calls, secrets, and agent skills."
---

# GitHub CLI (`gh`)

Authenticated CLI for GitHub. JSON output via `--json` or `--jq`, machine-readable, pipeable.

## Auth

```bash
gh auth login                              # browser-based OAuth
gh auth login -h github.com -w <token>     # PAT-based
gh auth logout
gh auth status                             # check active account + host
gh auth switch                             # switch accounts
gh auth token                              # print the token gh uses
gh auth setup-git                          # configure git with gh auth
```

## Pull requests

```bash
# List / status
gh pr list
gh pr list --state merged --limit 20
gh pr list --author @me --label bug
gh pr status                               # relevant, created, needs review

# View / checkout
gh pr view 123
gh pr view 123 --json title,body,files
gh pr view 123 --web
gh pr checkout 123
gh co 123                                  # shortcut alias

# Create
gh pr create --title "Fix" --body "Closes #42"
gh pr create --fill                        # use commit messages
gh pr create --draft --label wip

# Review / comment
gh pr review 123 --approve
gh pr review 123 --comment "LGTM but fix the typo"
gh pr review 123 --request-changes
gh pr comment 123 --body "Done"

# Merge
gh pr merge 123 --merge                    # merge commit
gh pr merge 123 --squash                   # squash
gh pr merge 123 --rebase                   # rebase
gh pr merge 123 --auto                     # enable auto-merge

# Diff / checks
gh pr diff 123
gh pr diff 123 --name-only
gh pr checks 123                           # CI status
gh pr checks 123 --watch                   # wait for CI

# Other
gh pr close 123
gh pr reopen 123
gh pr edit 123 --add-label "needs-review"
gh pr ready 123                            # mark ready for review
gh pr lock 123                             # lock conversation
gh pr unlock 123
gh pr update-branch 123
```

## Issues

```bash
gh issue list
gh issue list --label bug --state closed
gh issue list --assignee @me --limit 10
gh issue status                            # assigned, created, mentioned
gh issue view 42
gh issue view 42 --json title,labels --web
gh issue create --title "Bug" --body "Steps..."
gh issue create --label enhancement --assignee @me
gh issue close 42
gh issue reopen 42
gh issue comment 42 --body "Fixed in #99"
gh issue edit 42 --add-label blocked
gh issue pin 42
gh issue unpin 42
gh issue transfer 42 owner/repo
gh issue lock 42
gh issue unlock 42
```

## Repos

```bash
gh repo view                               # current repo
gh repo view owner/repo --web
gh repo list                               # your repos
gh repo list owner --limit 50
gh repo create my-app --public --clone
gh repo create my-org/my-app --private --push --source .
gh repo clone owner/repo
gh repo fork owner/repo --clone
gh repo archive owner/repo
gh repo rename new-name
gh repo delete owner/repo
gh repo edit --description "New desc" --homepage https://...
gh repo sync                               # sync local fork
gh repo set-default owner/repo             # set default for pwd
```

## Releases

```bash
gh release list -R owner/repo
gh release view v1.0.0
gh release view v1.0.0 --json name,body,assets --web
gh release create v1.0.0 --title "v1.0.0" --notes "Release notes"
gh release create v1.0.0 --generate-notes --latest
gh release create v1.0.0 --notes-file CHANGELOG.md --target main
gh release upload v1.0.0 ./dist/*.tar.gz
gh release download v1.0.0 --dir ./downloads
gh release delete v1.0.0
```

## Actions

```bash
# Workflows
gh workflow list                           # all workflows
gh workflow view <id>                      # workflow details
gh workflow run <id> -f env=prod           # trigger workflow_dispatch
gh workflow enable <id>
gh workflow disable <id>

# Runs
gh run list                                # recent runs
gh run list --workflow=ci.yml --branch main
gh run view <id>
gh run watch <id>                          # watch until complete
gh run rerun <id>
gh run cancel <id>
gh run download <id>                       # download artifacts

# Caches
gh cache list
gh cache delete <key>
```

## API (for everything else)

```bash
# REST
gh api /repos/{owner}/{repo}/issues
gh api /repos/{owner}/{repo}/releases/latest --jq .tag_name
gh api repos/{owner}/{repo}/issues/1/comments -f body="Nice"
gh api -X PATCH /repos/{owner}/{repo} -f name=new-name
gh api --paginate /repos/{owner}/{repo}/issues > all.json

# GraphQL
gh api graphql -f query='query { viewer { login } }'
gh api graphql -F owner='{owner}' -F repo='{repo}' -f query='
  query($owner: String!, $repo: String!) {
    repository(owner: $owner, name: $repo) { stargazerCount }
  }'

# Headers + caching
gh api -H "Accept: application/vnd.github.v3.raw+json" /repos/{owner}/{repo}/readme
gh api --cache 3600s /repos/{owner}/{repo}  # cache for 1hr
```

## Search

```bash
gh search repos "terraform" --stars=1000
gh search issues "bug" --repo owner/repo --state open
gh search prs "WIP" --author @me
gh search commits "fix security" --repo owner/repo
gh search code "TODO" --repo owner/repo
```

## Secrets & variables

```bash
# Secrets
gh secret list
gh secret set DEPLOY_KEY --body "$(cat key.pem)"
gh secret set DEPLOY_KEY --body "value" --env production
gh secret delete DEPLOY_KEY

# Variables
gh variable list
gh variable set MY_VAR --body "hello"
gh variable get MY_VAR
gh variable delete MY_VAR
```

## Skills

```bash
gh skill list                              # installed skills
gh skill search terraform                  # search skill catalog
gh skill install owner/repo <name>         # install a skill
gh skill preview owner/repo <name>         # preview before install
gh skill update --all                      # update all skills
gh skill publish --dry-run                 # validate for publishing
```

## Config

```bash
gh config get git_protocol                 # ssh or https
gh config set git_protocol ssh
gh config set editor code
gh completion                              # shell completion
```

## Common flags

- `-R, --repo owner/repo` — target a different repo
- `--json fields...` — JSON output (e.g. `--json title,body`)
- `--jq query` — filter JSON with jq (e.g. `--jq '.[].title'`)
- `--template '{{.title}}'` — Go template output
- `--web` — open in browser
- `--limit N` — max results (default varies)

## Gotchas

- `gh pr create --fill` uses commit messages as title/body — good for quick PRs
- `--repo owner/repo` flag works on every command — use from any directory
- `gh api` defaults to GET, switches to POST when you add `-f` fields
- Use `gh api --paginate` for large result sets; `--slurp` to merge pages
- `gh auth status` tells you if and where you're authenticated
- `gh run watch` is great for CI debugging without leaving the terminal
- `gh secret set` reads from stdin or `--body` — use `--body "$(cat file)"` for file contents
- Environment: `GH_TOKEN`, `GITHUB_TOKEN`, `GH_HOST` for scripting