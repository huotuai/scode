package cli

import "strings"

// Prompt commands are built-in slash commands that expand to canned
// agent workflows (/commit, /commit-push-pr). They are NOT App.Command
// builtins: the routers send them down the prompt path, and App.Run
// expands them at the prompt boundary (the $name discipline), so
// the agent executes the workflow with its bash tool under the usual
// sandbox and permission gates.

// promptCommandNames is ordered longest-first so "/commit-push-pr" is
// never matched as "/commit".
var promptCommandNames = []string{"/commit-push-pr", "/commit", "/init"}

var promptCommandBodies = map[string]string{
	"/commit": `Create a git commit for the current changes.

1. Inspect the working tree: git status, git diff, and git diff --staged, plus git log --oneline -10 to learn this repo's commit message style.
2. Stage the changes that form one logical commit (git add). Never stage secrets, credentials, or files you cannot explain; leave unrelated or risky changes unstaged and say which you left.
3. Write the commit message in the repo's own style — imperative subject line, body only when it adds information. Never mention AI tooling and never add Co-Authored-By trailers.
4. Commit in one call, using a HEREDOC for the message:
   git commit -m "$(cat <<'EOF'
   <subject>

   <optional body>
   EOF
   )"
5. Show the result with git log -1 --stat, then stop. Do not push.

If the working tree is clean, say so and stop.`,

	"/commit-push-pr": `Commit the current changes, push the branch, and open a pull request.

First create the commit exactly like the /commit workflow: inspect git status, git diff, and git log for style, stage one logical change, and write the message in the repo's style with no AI attribution. Then:

1. Push the current branch with git push -u origin HEAD, creating the remote branch if needed. Never force-push.
2. Open a pull request with gh pr create: --title is the imperative summary of the change; --body has a "## Summary" section and a "## Test plan" section describing how the change was verified. Check gh pr list --limit 5 first if it is cheap, to match the house style.
3. Print the PR URL once created. If gh is missing or not authenticated, print the prepared title and body plus the compare URL (derive it from git remote get-url origin and the branch name) so the PR can be opened manually.

Stop after the PR step. Do not merge and do not delete branches.`,

	"/init": `Create or update the AGENTS.md file for this repository (Codex's /init).

1. Explore the project: list the top-level files and directories, then read the key ones — README, build manifests (go.mod, package.json, Cargo.toml, pyproject.toml, …), CI configs, and a small representative sample of source files. Read only what you need to describe the project accurately.
2. Write AGENTS.md at the repository root with exactly this structure:

# AGENTS.md

## Project overview
One short paragraph: what the project is, its primary language(s), key frameworks.

## Building and running
The exact commands to build and run the project, taken from the manifests and CI config — never guessed.

## Testing
How to run the test suite, and how to run a single test.

## Linting / formatting
The formatting and lint commands the repo actually uses (gofmt, ruff, eslint, …). If none exist, say so plainly.

## Code style
The conventions a contributor must follow, as observed in the code: naming, error handling, comment language, import ordering, anything consistent.

## Architecture
The directory layout with one line per significant package or module, and how the pieces fit together.

## Notes
Anything unusual a coding agent must know: git workflow, generated code, platform quirks, commands that must never run.

3. If AGENTS.md already exists, read it first: preserve the user's hand-written sections and intent, refresh only facts you can verify from the repository.
4. Every section stays terse and factual; commands must be copy-pasteable. Never invent anything you did not observe.
5. Finish by printing the file path and a one-line summary. AGENTS.md is picked up as the session's project context on the next start.`,
}

// IsPromptCommand reports whether line invokes a built-in prompt
// command, optionally with trailing instructions ("/commit focus the
// tests"). Longest name wins, and the name must end the line or be
// followed by a space, so "/committing" matches nothing.
func IsPromptCommand(line string) bool {
	_, _, ok := matchPromptCommand(line)
	return ok
}

// matchPromptCommand resolves line against the prompt commands and
// splits off the trailing instructions ("" when none).
func matchPromptCommand(line string) (name, rest string, ok bool) {
	for _, name := range promptCommandNames {
		if line == name {
			return name, "", true
		}
		if tail, found := strings.CutPrefix(line, name+" "); found {
			return name, strings.TrimSpace(tail), true
		}
	}
	return "", "", false
}

// expandPromptCommand expands "/commit [instructions]" into the canned
// workflow prompt; trailing instructions ride along as extra guidance.
func expandPromptCommand(text string) (string, bool) {
	name, rest, ok := matchPromptCommand(text)
	if !ok {
		return text, false
	}
	if rest == "" {
		return promptCommandBodies[name], true
	}
	return promptCommandBodies[name] + "\n\nAdditional instructions from the user: " + rest, true
}

// PromptCommandInfos lists the prompt commands for the TUI completion
// palette (display metadata only; execution goes through the prompt
// path, not App.Command).
func PromptCommandInfos() []CommandInfo {
	out := make([]CommandInfo, 0, len(promptCommandNames))
	for _, name := range promptCommandNames {
		desc := "create a git commit"
		switch name {
		case "/commit-push-pr":
			desc = "commit, push, and open a PR"
		case "/init":
			desc = "generate AGENTS.md for this repo (Codex-style)"
		}
		out = append(out, CommandInfo{Name: name, Hint: "[instructions]", Desc: desc})
	}
	return out
}
