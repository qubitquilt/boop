# runner-config

Mounted read-only into the runner pod at `/home/opencode/.config/opencode`.

The whole tree is packed into a single `boop-runner-config` ConfigMap by the
`configMapGenerator` in `base/kustomization.yaml`. Kustomize walks this directory
recursively and uses each file's relative path as its key. Mounted as a volume,
the directory structure is preserved verbatim.

## What to drop in

| Path | Purpose |
|------|---------|
| `opencode.json` | OpenCode config (model, provider, mcp servers, etc.) |
| `AGENTS.md` | Agent-level system prompt (optional) |
| `skills/<skill-name>/SKILL.md` | One folder per skill |

OpenCode discovers skills from `~/.config/opencode/skills/<name>/SKILL.md`, so the
layout above is exactly what the runner expects.

## Editing flow

1. Edit any existing file in this directory, or drop a new skill into
   `skills/<name>/SKILL.md`.
2. **If you added a new file**, add an entry to `base/kustomization.yaml` under
   `configMapGenerator.files` using the `key=path` form so the directory
   structure is preserved on mount:
   ```yaml
   files:
     - opencode.json=runner-config/opencode.json
     - skills/<name>/SKILL.md=runner-config/skills/<name>/SKILL.md
   ```
   Without the `key=path` form, kustomize flattens the basename and the file
   lands at the wrong path inside the pod.
3. Commit + push. Kustomize updates the ConfigMap, ArgoCD syncs, the next PR
   review uses the new config.

No image rebuild required.

## Limits

Hard cap of 1 MB total per ConfigMap (etcd limit). If you outgrow it, swap to
the git-sync init container pattern that `apps/dev-tools/openchamber` uses.
