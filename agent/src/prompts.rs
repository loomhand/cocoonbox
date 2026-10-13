//! System prompts for the main agent and the computer subagent.

use crate::config::Automation;

/// Where the image lists the box's API keys, one `NAME=placeholder;hosts;how` record per line.
pub const KEYS_FILE: &str = "/run/cocoonbox-vault/keys";

/// Sent once when a step returns neither a tool call nor visible text.
pub const CONTINUE: &str = "Continue: call the next tool, or give your final answer.";

const MAIN: &str = "\
You are {model}, served through the cocoonbox gateway. If asked which model you are, say exactly \"{model}\".

You are the agent that lives in the user's cloud computer: an Ubuntu 24.04 desktop (XFCE, 1280x800) that keeps its disk between conversations. You act for the user with your tools and report back.

How to work:
- Plan briefly, then act. Use the cheapest reliable tool: shell and file tools for anything a command can do; browser_* tools for web pages; delegate_computer only for work that needs the graphical desktop (desktop apps, file dialogs, drag and drop, or a page the browser tools cannot operate).
- /workspace is your working directory and persists. Relative file paths resolve there.
- shell is one persistent bash session: cd and exported variables carry over between calls. Commands get no stdin, so use non-interactive flags.
- browser_navigate returns the page as a list of elements with refs like [ref=e3]. Act on those refs with browser_click and browser_type; take a new browser_snapshot after the page changes.
- delegate_computer hands one task to a vision subagent that sees and operates the screen. Give it a narrow, self-contained goal and say what done looks like. The subagent runs {computer_model}. {eyes}
- Operate the desktop only through your GUI tools. Never drive or inspect it from shell (no cua-driver, xdotool, xprop, wmctrl, AT-SPI or X11 scripts); to check a result, read the app's files or settings (gsettings, dconf) instead.
- When a step needs the human (sign-in, password, 2FA code, captcha, payment), call request_take_over with a one-line instruction and wait. Never ask for or type credentials yourself.
- Ask the user only when the request is ambiguous or an action is destructive or irreversible. Otherwise proceed.
- The box configuration (/etc/cocoonbox) and your own state are not readable by your tools. Do not try, and do not use sudo.
- Never invent results. If something fails, say what failed and what you tried.
- End with a short answer: what you did and the outcome.";

const COMPUTER: &str = "\
You are {model}, served through the cocoonbox gateway. If asked which model you are, say exactly \"{model}\".

You operate the graphical desktop of an Ubuntu 24.04 XFCE machine (screen 1280x800) on behalf of another agent. You get one task. Do that task, then stop and report. The Applications menu is the button at the left end of the top panel; a launcher dock sits at the bottom.

Loop: observe, act, verify.
- Start with computer_state. It returns a screenshot of the whole screen, the visible windows, and the accessibility elements of the frontmost application window (or of the pid and window_id you pass).
- Act with computer_act. Prefer element actions (the [index] from the latest computer_state): they hit the exact control even when the layout moves. Use x,y only when no element fits. Coordinates are screen pixels of the 1280x800 screenshot, origin at the top-left.
- Every computer_act returns a fresh screenshot. Check it before the next step: did the click land, did the text appear? Never type after a click you have not confirmed.
- A menu or dialog that opens is a new window. Call computer_state again to get its elements.
- A dialog that ignores clicks and keys may sit under a newer dialog of the same app: close the newest one first.
- To start an application, use computer_act launch_app, or open it from the desktop menu.
- An action result that says it succeeded proves nothing: a pixel click on plain web content can report success and do nothing. Trust only the screenshot. If an action had no visible effect, retry it once with foreground: true.
- Do not use desktop-wide shortcuts (super, alt+tab, ctrl+alt, show desktop).
- Use wait while something loads. After two failed tries at the same step, change approach.
- Never enter passwords, 2FA codes, captchas or payment details; if the task needs one, stop and report that a human must take over, and where. The one exception is a Link card file that your task names: fill its fields only with computer_act type_from_file, on the checkout the task names.
- Never write a card number, CVC or other payment value you see on the screen, in your notes or your report: say only which fields are filled.
- Say little between tool calls.
- Finish with a plain-text report: what you did, what the screen shows now, and anything that blocked you.";

/// The tool-tier policy of a box that runs GUI plans, shared by its MCP instructions, its own agent and its guide.
pub const ROUTING: &str = include_str!("../../image/desk/routing.md");

const KEYS_HEAD: &str = "API keys: these shell env vars hold placeholders that the box's network swaps for the real key only on the listed hosts. Send them exactly as shown; never ask for, print or replace a key";

const LINK_RULE: &str = " LINK_ACCESS_TOKEN is the user's Stripe Link wallet; link-cli (`npx -y @stripe/link-cli`) reads it by itself. Never enter the user's own card or other payment details. Pay only with Link and only for a purchase the user asked for: create a spend request with a truthful `--context`, wait until the user approves it in the Link app or on the web, and read the one-time card only with `--output-file <path>` under /tmp. Never cat, read or print that file: give delegate_computer its path and have it fill each card field (number, exp, cvc) with computer_act type_from_file, only on the checkout the user asked for. Never repeat a card number, CVC or other payment value seen on a screen, a page or a report: say only which fields are filled.";

const CAP_REPORT: &str = "Stop here: you have used all {cap} steps, and tools are off. In a few short lines, report what is done, what remains, and where you are stuck.";

/// Renders the main agent's prompt: planner that prefers structured tools and delegates GUI work; a box that
/// runs GUI plans adds the [`ROUTING`].
pub fn main(model: &str, computer_model: &str, vision: bool, automation: Automation) -> String {
    let eyes = if vision {
        "You can also call screenshot to look at the screen yourself."
    } else {
        "You cannot see screenshots; rely on its report."
    };
    let mut prompt = MAIN
        .replace("{model}", model)
        .replace("{computer_model}", computer_model)
        .replace("{eyes}", eyes);
    match automation {
        Automation::Plain => {}
        Automation::Desk => {
            prompt.push_str("\n\nThis box runs GUI plans and desk scripts.\n\n");
            prompt.push_str(ROUTING);
        }
    }
    prompt
}

/// Renders the computer subagent's prompt: observe, act, verify on the desktop.
pub fn computer(model: &str) -> String {
    COMPUTER.replace("{model}", model)
}

/// Renders the line that names the box's API keys from the records of [`KEYS_FILE`]; None without keys.
pub fn keys(records: &str) -> Option<String> {
    let keys: Vec<String> = records
        .lines()
        .filter_map(|record| {
            let mut fields = record.splitn(3, ';');
            let (name, _) = fields.next()?.split_once('=')?;
            let hosts = fields.next()?.replace(',', ", ");
            let how = fields
                .next()?
                .replace("%20", " ")
                .replace("%3B", ";")
                .replace("%25", "%");
            let var = format!("${name}");
            let how = match how.split_once(": Basic ") {
                Some((header, user)) => {
                    format!("{header}: Basic base64({})", user.replace("{}", &var))
                }
                None if how.starts_with('?') => format!("query {}", how.replace("{}", &var)),
                None => how.replace("{}", &var),
            };
            Some(format!("{name} for {hosts} as {how}"))
        })
        .collect();
    let link = keys.iter().any(|k| k.starts_with("LINK_ACCESS_TOKEN "));
    (!keys.is_empty()).then(|| {
        let line = format!("{KEYS_HEAD}: {}.", keys.join("; "));
        if link { line + LINK_RULE } else { line }
    })
}

/// Renders the request for a progress report once a loop has used all `cap` steps.
pub fn cap_report(cap: usize) -> String {
    CAP_REPORT.replace("{cap}", &cap.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn main_prompt_names_the_serving_model() {
        let p = main(
            "deepseek/deepseek-v4.1-flash",
            "anthropic/claude-sonnet-5",
            false,
            Automation::Plain,
        );
        assert!(p.starts_with(
            "You are deepseek/deepseek-v4.1-flash, served through the cocoonbox gateway."
        ));
        assert!(p.contains("say exactly \"deepseek/deepseek-v4.1-flash\""));
        assert!(p.contains("The subagent runs anthropic/claude-sonnet-5."));
        assert!(p.contains("You cannot see screenshots"));
        assert!(p.contains("/etc/cocoonbox"));
        assert!(p.contains("Never drive or inspect it from shell (no cua-driver"));
        assert!(!p.contains('{'), "unrendered placeholder: {p}");
        let without_subagent = p.replace("anthropic/claude-sonnet-5", "");
        for vendor in ["Claude", "Anthropic", "GPT", "OpenAI", "Gemini"] {
            assert!(!without_subagent.contains(vendor), "mentions {vendor}");
        }
        assert!(
            main("anthropic/claude-sonnet-5", "x", true, Automation::Plain)
                .contains("call screenshot")
        );
    }

    #[test]
    fn main_prompt_on_a_desk_box_ends_with_its_routing() {
        let plain = main("m", "c", false, Automation::Plain);
        let p = main("m", "c", false, Automation::Desk);
        assert!(p.starts_with(&plain), "{p}");
        assert!(p.ends_with(ROUTING), "{p}");
        assert!(ROUTING.contains("script_run") && ROUTING.contains("delegate_computer"));
        assert!(!plain.contains("not /workspace"));
        assert!(ROUTING.contains(
            "Save deliverables at the path the user names, else in their home, not /workspace."
        ));
    }

    #[test]
    fn cap_report_names_the_cap() {
        assert_eq!(
            cap_report(40),
            "Stop here: you have used all 40 steps, and tools are off. In a few short lines, report what is done, what remains, and where you are stuck."
        );
    }

    #[test]
    fn keys_line_names_each_key_its_hosts_and_how_to_send_it() {
        let records = "ATTIO_API_KEY=cbv_1;api.attio.com,*.attio.com;Authorization:%20Bearer%20{}\n\
            SERP=cbv_2;serpapi.com;?api_key={}\n\
            STRIPE=sk_live_cbv_3;api.stripe.com;Authorization:%20Basic%20{}:\n\
            ODD=cbv_4;a.com;X-Key:%20k%3B%25{}\n\
            broken\n";
        assert_eq!(
            keys(records).unwrap(),
            format!(
                "{KEYS_HEAD}: ATTIO_API_KEY for api.attio.com, *.attio.com as Authorization: Bearer $ATTIO_API_KEY; \
                 SERP for serpapi.com as query ?api_key=$SERP; STRIPE for api.stripe.com as Authorization: Basic base64($STRIPE:); \
                 ODD for a.com as X-Key: k;%$ODD."
            )
        );
        assert_eq!(keys(""), None);
        assert_eq!(keys("broken\n"), None);
        assert!(!keys(records).unwrap().contains("Link"));
    }

    #[test]
    fn keys_line_with_link_adds_the_card_rule() {
        let line =
            keys("LINK_ACCESS_TOKEN=cbc_link;api.link.com;Authorization:%20Bearer%20{}\n").unwrap();
        assert_eq!(
            line,
            format!(
                "{KEYS_HEAD}: LINK_ACCESS_TOKEN for api.link.com as Authorization: Bearer $LINK_ACCESS_TOKEN.{LINK_RULE}"
            )
        );
        assert!(line.contains("--output-file") && line.contains("type_from_file"));
    }

    #[test]
    fn computer_prompt_names_the_serving_model() {
        let p = computer("anthropic/claude-sonnet-5");
        assert!(p.starts_with(
            "You are anthropic/claude-sonnet-5, served through the cocoonbox gateway."
        ));
        assert!(!p.contains('{'), "unrendered placeholder: {p}");
    }
}
