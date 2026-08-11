// Prompt part helpers.
//
// Shared prompt-assembly utilities. The boop prompt builder
// (openrouter/prompt.mjs) and the experts' lens loader
// (experts.mjs readLensBody) both strip YAML frontmatter from
// the ConfigMap-mounted Markdown files; a future PR can extend
// the package with `pr_metadata_block`, `what_you_are_receiving`,
// and `diff_range` when a third call site needs them.
//
// RF-003 lifted the frontmatter strip out of two near-duplicate
// sites; the other "duplications" named in the audit are
// actually internal to the prompt builder (the no-tools vs
// tools-enabled block is the same template twice in one
// function), so the lift is a one-helper, two-call-site change.

/**
 * Strip a leading `---` YAML frontmatter block from a
 * Markdown body. The lens files and SKILL.md use a
 * `---` block for Hugo-style metadata; the model sees a
 * clean system-prompt-ish body when the block is removed.
 *
 * The strip is greedy enough to match across newlines
 * ([\s\S]*?) and anchored to the start of the string
 * (^--- ... ---\n*). A body with no frontmatter is
 * returned unchanged.
 */
export function stripFrontmatter(text) {
  if (!text) return "";
  return String(text).replace(/^---[\s\S]*?---\n*/, "");
}
