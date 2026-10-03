//! The hand-over template, its heading check, and the texts the box CLI receives.

use std::path::Path;

/// Largest hand-over or inbox message body.
pub const MAX_BODY: usize = 256 << 10;
/// The first line of every hand-over starts with this.
pub const TITLE: &str = "# Hand-over: ";
/// Headings every hand-over carries once, each as its own line.
pub const HEADINGS: [&str; 10] = [
    "## Goal and definition of done",
    "## User preferences and constraints",
    "## Current state",
    "## Decisions made",
    "## Files touched",
    "## Next steps",
    "## Commands",
    "## Environment facts",
    "## Gotchas learned",
    "## Open questions",
];
/// Starts a restarted session's first prompt.
pub const RESTARTED: &str = "You were restarted; continue from this hand-over and the repo.";

const TEMPLATE: &str = "# Hand-over: <one-line goal>

From: box   Agent: <claude|codex|cursor>   Repo: <path>   Branch: <branch> @ <short sha>

## Goal and definition of done
<what the user asked for, in their words where it matters; what \"done\" means>

## User preferences and constraints
<style, scope limits, things the user said not to do, deadlines; or none>

## Current state
<where the work stands in one paragraph: what works, what is broken, what is half-done>

## Decisions made
- <decision> — <why>; rejected: <alternative and why>

## Files touched
- `<path>` — <what changed and why; whether it is finished>

## Next steps
1. <the very next action, specific enough to start without asking>

## Commands
- Build: `<command>`   Test: `<command>`   Lint: `<command>`   Run: `<command>`
- Verify the current state: `<command>` → <what passing looks like>

## Environment facts
- Tools and versions: <go 1.26.x, rustc 1.9x, node 22.x, …; where each came from>
- Services: <local databases, ports, docker compose services; or none>
- Hosts: <APIs or internal hosts the task calls; or none>
- Secrets needed: <names only, never values; or none>
- Installed during this session: <packages and tools added; or none>

## Gotchas learned
- <non-obvious things that cost time: flaky tests, a wrong doc, a tool quirk>

## Open questions
- <what needs the user, and what to do meanwhile>
";

/// What a hand-over lacks: the title line, then each heading that is missing or repeated;
/// empty when it is valid.
pub fn missing(body: &str) -> Vec<&'static str> {
    let mut out = Vec::new();
    if !body.lines().next().is_some_and(|l| l.starts_with(TITLE)) {
        out.push(TITLE.trim_end());
    }
    for h in HEADINGS {
        if body.lines().filter(|l| l.trim_end() == h).count() != 1 {
            out.push(h);
        }
    }
    out
}

/// Refuses a hand-over that is too large or breaks the template, naming what it lacks.
pub fn check(body: &str) -> Result<(), String> {
    if body.len() > MAX_BODY {
        return Err(format!(
            "the hand-over is {} bytes; at most {MAX_BODY}",
            body.len()
        ));
    }
    let lacks = missing(body);
    if lacks.is_empty() {
        Ok(())
    } else {
        Err(format!(
            "the hand-over lacks (or repeats): {}; an empty section says none",
            lacks.join(", ")
        ))
    }
}

/// The box CLI's standing instructions, ahead of the hand-over in its first prompt.
pub fn preamble(dir: &Path, repo: &str, offload_dir: &Path, base: &str) -> String {
    let dir = dir.display();
    let handover = offload_dir.join(repo).join("handover.md");
    let handover = handover.display();
    format!(
        "You run in the user's cocoonbox, a Linux cloud desktop, in {dir}. The user is away and nobody \
answers prompts. You start fresh: the hand-over below, written by the agent that worked on this task \
before you, and the repo are all you know.

1. Read the hand-over, then run `cocoonbox-agent offload env`. It waits for the environment step and \
says which tools and versions are ready. Install what is still missing (mise first, apt with sudo \
second) and record every install.
2. HTTP_PROXY and HTTPS_PROXY are set in this box and can break tests that read them.
3. Before you blame your change for a failure, compare against the starting point {base} \
(`git stash`, or a worktree at {base}).
4. Run the hand-over's verify commands before new work, so you know the starting point.
5. Continue from \"Next steps\". Respect \"Decisions made\" and \"User preferences and constraints\" \
unless you find them wrong; then say why in your next hand-over. Commit as you go on the current \
branch. Never push.
6. After each milestone, rewrite your hand-over in {handover} with the template below \
(\"From: box\"; every heading, \"none\" for an empty section) and run \
`cocoonbox-agent offload post --progress < {handover}`.
7. If you are blocked on the user, run `cocoonbox-agent offload post --question` with the question \
on stdin, then work on what you can.
8. When a message starts with BACK, follow it.

The hand-over template:

{TEMPLATE}
--- hand-over ---
"
    )
}

/// The BACK instruction that leads a `send --back` message.
pub fn back(repo: &str, offload_dir: &Path) -> String {
    let handover = offload_dir.join(repo).join("handover.md");
    let handover = handover.display();
    format!(
        "BACK: the user is back. Finish or stop the current step and commit everything (the commit \
message says what is unfinished, if anything). Write your hand-over with the same template to \
{handover} (\"From: box\"; under \"Environment facts\" list what you installed), run \
`cocoonbox-agent offload post --back < {handover}`, and end your turn when it prints ready."
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> String {
        let mut s = String::from("# Hand-over: add --root\n\nFrom: Mac\n");
        for h in HEADINGS {
            s.push_str(&format!("\n{h}\nnone\n"));
        }
        s
    }

    #[test]
    fn a_complete_hand_over_passes() {
        assert!(missing(&valid()).is_empty());
        assert!(check(&valid()).is_ok());
        assert!(missing(&TEMPLATE.replace("<one-line goal>", "x")).is_empty());
    }

    #[test]
    fn missing_repeated_and_inline_headings_are_named() {
        let body = valid()
            .replace("## Commands\n", "")
            .replace(
                "## Gotchas learned\n",
                "## Gotchas learned\n## Gotchas learned\n",
            )
            .replace("## Next steps\n", "text ## Next steps\n");
        assert_eq!(
            missing(&body),
            ["## Next steps", "## Commands", "## Gotchas learned"]
        );
        let err = check(&body).unwrap_err();
        assert!(err.contains("## Commands"), "{err}");
    }

    #[test]
    fn the_title_must_be_the_first_line() {
        let body = format!("\n{}", valid());
        assert_eq!(missing(&body), ["# Hand-over:"]);
        assert_eq!(missing("# Hand-over:\n"), {
            let mut v = vec!["# Hand-over:"];
            v.extend(HEADINGS);
            v
        });
    }

    #[test]
    fn an_oversized_hand_over_is_refused() {
        let body = format!("{}{}", valid(), "x".repeat(MAX_BODY));
        assert!(check(&body).unwrap_err().contains("at most"));
    }
}
