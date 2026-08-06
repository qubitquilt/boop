# runner-config

Mounted read-only into the runner pod at `/home/opencode/.config/opencode`.

The whole tree is packed into a single `boop-runner-config` ConfigMap by the
`configMapGenerator` in `base/kustomization.yaml`. Kustomize walks this directory
recursively and uses each file's relative path as its key. Mounted as a volume,
the directory structure is preserved verbatim.

QUB-98 dropped `opencode.json` from the ConfigMap (and the opencode CLI
itself). The runner now reads the model name from `OPENROUTER_MODEL` and
calls the OpenRouter SDK in-process. Only the skill files mount here.

## Layout

| Path | Purpose |
|------|---------|
| `skills/boop/SKILL.md` | Boop orchestrator: persona, voice contract, output spec |
| `skills/boop/agents/review-*.md` | Seven review lenses the orchestrator walks |
| `skills/boop/resources/persona.md` | Curated pool of pug flourishes the narrator samples (one per review) |
| `skills/boop/resources/output-format.md` | The exact `=== SUMMARY === / === INLINE COMMENTS === / === END ===` block the narrator emits |
| `skills/boop/resources/lens-template.md` | Shared template every lens file is built from |

The runner reads `skills/boop/SKILL.md` and inlines the lens files and
the persona resource into the prompt as the multi-expert pipeline walks
each stage. Boop does one walkthrough LLM call, then N parallel
expert calls (one per lens), then one final narrator call. The
narrator synthesises the walkthrough and gathered findings into the
structured block the runner parses and posts to the PR.

## The seven lenses

| Lens | File | What it covers |
|------|------|----------------|
| Code quality | `skills/boop/agents/review-code-quality.md` | Complexity, coupling, cohesion, LOC |
| Design pattern | `skills/boop/agents/review-design-pattern.md` | Structural choices, practical alternatives |
| Error handling | `skills/boop/agents/review-error-handling.md` | Error paths, async safety, silent fallbacks |
| Readability | `skills/boop/agents/review-readability.md` | Naming, clarity, magic values, signatures |
| SOLID | `skills/boop/agents/review-solid-principles.md` | Coupling, extensibility, dependency structure |
| Test quality | `skills/boop/agents/review-test-quality.md` | Test assertions vs. name, fixtures, composition |
| Deep | `skills/boop/agents/review-deep.md` | End-to-end walkthrough, coupled invariants |

The orchestrator drives the multi-expert pipeline: a walkthrough call
produces shared context, then one expert call per lens runs in
parallel, then a gather/meta-review/narrate cycle produces the
structured block. The lenses are reference material each expert
reads as its system prompt; they are not sub-agent invocations
in the AI-agent sense (there is no per-agent runtime context).

## Editing flow

1. Edit any existing file in this directory, or drop a new lens into
   `skills/boop/agents/review-<name>.md`.
2. **If you added a new file**, add an entry to `base/kustomization.yaml` under
   `configMapGenerator.files` using the `key=path` form so the directory
   structure is preserved on mount:
   ```yaml
   files:
     - skills/<skill>/SKILL.md=runner-config/skills/<skill>/SKILL.md
     - skills/<skill>/agents/<agent>.md=runner-config/skills/<skill>/agents/<agent>.md
   ```
   Without the `key=path` form, kustomize flattens the basename and the file
   lands at the wrong path inside the pod.
3. Commit + push. Kustomize updates the ConfigMap, ArgoCD syncs, the next PR
   review uses the new config.

No image rebuild required.

## Limits

Hard cap of 1 MB total per ConfigMap (etcd limit). If you outgrow it, swap to
the git-sync init container pattern that `apps/dev-tools/openchamber` uses.
