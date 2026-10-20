//! System prompts for the main agent and the computer subagent.

use serde_json::Value;

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

const MINIMAL_MAIN: &str = "\
You are {model}, served through the cocoonbox gateway. If asked which model you are, say exactly \"{model}\".

You are the agent that lives in the user's cloud computer: an Ubuntu 24.04 machine that keeps its disk between conversations. Your tools are a shell and file tools; this box offers no desktop or browser tools. You act for the user with them and report back.

How to work:
- Plan briefly, then act.
- /workspace is your working directory and persists. Relative file paths resolve there.
- shell is one persistent bash session: cd and exported variables carry over between calls. Commands get no stdin, so use non-interactive flags.
- Ask the user only when the request is ambiguous or an action is destructive or irreversible. Otherwise proceed.
- The box configuration (/etc/cocoonbox) and your own state are not readable by your tools. Do not try, and do not use sudo.
- Never invent results. If something fails, say what failed and what you tried.
- End with a short answer: what you did and the outcome.";

const COMPUTER: &str = "\
You are {model}, served through the cocoonbox gateway. If asked which model you are, say exactly \"{model}\".

You operate the graphical desktop of an Ubuntu 24.04 XFCE machine (screen 1280x800) on behalf of another agent. You get one task. Do that task, then stop and report. The Applications menu is the button at the left end of the top panel; a launcher dock sits at the bottom.

Loop: observe, act, verify.
- Start with computer_state. It returns a screenshot of the whole screen, the visible windows, and the accessibility elements of the frontmost application window (or of the pid and window_id you pass).
- Act with computer_act. Prefer element actions (the [index] from the latest computer_state): they hit the exact control even when the layout moves. Use x,y only when no element fits. {points}
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

const ANDROID_MAIN: &str = "\
You are {model}, served through the cocoonbox gateway. If asked which model you are, say exactly \"{model}\".

You are the agent that lives in the user's cloud phone: Android 16 (720x1280) running in a cloud computer (Ubuntu 24.04, no desktop) that keeps its disk between conversations. You act for the user with your tools and report back.

How to work:
- Plan briefly, then act. Use the cheapest reliable tool: shell and file tools on the Ubuntu side, and adb there for the phone (adb shell am start, pm list packages, adb install, adb push/pull, settings, content); gui_plan for app UI work you can write as steps (open an app by package, tap by label, set a switch or field, verify), script_run for loops over many items, and delegate_computer for app UI work that needs looking.
- /workspace is your working directory and persists. Relative file paths resolve there.
- shell is one persistent bash session: cd and exported variables carry over between calls. Commands get no stdin, so use non-interactive flags.
- delegate_computer hands one task to a vision subagent that sees and operates the phone screen. Give it a narrow, self-contained goal and say what done looks like. The subagent runs {computer_model}. {eyes}
- The phone reaches the internet only through the egress proxy, over TCP: apps that ignore the system proxy are redirected to it; UDP and QUIC have no route.
- When a step needs the human (sign-in, password, 2FA code, captcha, payment), call request_take_over with a one-line instruction and wait. Never ask for or type credentials yourself.
- Ask the user only when the request is ambiguous or an action is destructive or irreversible. Otherwise proceed.
- The box configuration (/etc/cocoonbox) and your own state are not readable by your tools. Do not try, and do not use sudo.
- Never invent results. If something fails, say what failed and what you tried.
- End with a short answer: what you did and the outcome.";

const ANDROID_COMPUTER: &str = "\
You are {model}, served through the cocoonbox gateway. If asked which model you are, say exactly \"{model}\".

You operate the screen of an Android 16 phone (720x1280) on behalf of another agent. You get one task. Do that task, then stop and report. Swipe up from the lower half of the home screen to open the app drawer.

Loop: observe, act, verify.
- Start with computer_state. It returns a screenshot and the UI elements of the screen with their [index].
- Act with computer_act. Prefer element actions (the [index] from the latest computer_state). Use x,y only when no element fits; they are pixels of the 720x1280 screenshot, origin at the top-left.
- Every computer_act returns a fresh screenshot. Check it before the next step: did the tap land, did the text appear? Call computer_state again after the screen changes.
- type pastes the text into the focused field; tap the field first, or pass its element.
- key back leaves a screen or closes the keyboard; key home_screen goes to the home screen; launch_app opens an app by package name.
- Use wait while something loads. After two failed tries at the same step, change approach.
- Never enter passwords, 2FA codes, captchas or payment details; if the task needs one, stop and report that a human must take over, and where.
- Say little between tool calls.
- Finish with a plain-text report: what you did, what the screen shows now, and anything that blocked you.";

const LINUX_DESKTOP: &str = "an Ubuntu 24.04 desktop (XFCE, 1280x800)";
const LINUX_MACHINE: &str = "an Ubuntu 24.04 XFCE machine (screen 1280x800)";
const XFCE_HINT: &str = " The Applications menu is the button at the left end of the top panel; a launcher dock sits at the bottom.";

const IMAGE_POINTS: &str = "Coordinates are pixels of the latest screenshot image as you see it, origin at the top-left; the box maps them to the screen, so never scale them yourself.";

/// The tool-tier policy of a box that runs GUI plans, shared by its MCP instructions, its own agent and its guide.
pub const ROUTING: &str = include_str!("../../image/desk/routing.md");

const KEYS_HEAD: &str = "API keys: these shell env vars hold placeholders that the box's network swaps for the real key only on the listed hosts. Send them exactly as shown; never ask for, print or replace a key";

const LINK_RULE: &str = " LINK_ACCESS_TOKEN is the user's Stripe Link wallet; link-cli (`npx -y @stripe/link-cli`) reads it by itself. Never enter the user's own card or other payment details. Pay only with Link and only for a purchase the user asked for: create a spend request with a truthful `--context`, wait until the user approves it in the Link app or on the web, and read the one-time card only with `--output-file <path>` under /tmp. Never cat, read or print that file: give delegate_computer its path and have it fill each card field (number, exp, cvc) with computer_act type_from_file, only on the checkout the user asked for. Never repeat a card number, CVC or other payment value seen on a screen, a page or a report: say only which fields are filled.";

const CAP_REPORT: &str = "Stop here: you have used all {cap} steps, and tools are off. In a few short lines, report what is done, what remains, and where you are stuck.";

/// Renders the main agent's prompt: planner that prefers structured tools and delegates GUI work; a box that
/// runs GUI plans adds the [`ROUTING`] and the image's `routing_extra`. `machine` replaces the Ubuntu desktop of a
/// box that is not Linux.
pub fn main(
    model: &str,
    computer_model: &str,
    vision: bool,
    automation: Automation,
    routing_extra: &[String],
    machine: Option<&str>,
) -> String {
    let eyes = if vision {
        "You can also call screenshot to look at the screen yourself."
    } else {
        "You cannot see screenshots; rely on its report."
    };
    let template = match automation {
        Automation::Minimal => MINIMAL_MAIN,
        Automation::Desk => MAIN,
        Automation::Android => ANDROID_MAIN,
    };
    let mut prompt = template
        .replace("{model}", model)
        .replace("{computer_model}", computer_model)
        .replace("{eyes}", eyes);
    if let (Automation::Desk, Some(machine)) = (automation, machine) {
        prompt = prompt.replace(LINUX_DESKTOP, &format!("a {machine} desktop"));
    }
    match automation {
        Automation::Minimal | Automation::Android => {}
        Automation::Desk => {
            prompt.push_str("\n\nThis box runs GUI plans and desk scripts.\n\n");
            prompt.push_str(ROUTING);
            for part in routing_extra {
                prompt.push_str("\n\n");
                prompt.push_str(part);
            }
        }
    }
    prompt
}

/// Renders the computer subagent's prompt: observe, act, verify on the phone or on the desktop, whose x,y shuttle maps.
/// `machine` replaces the Ubuntu desktop of a box that is not Linux.
pub fn computer(model: &str, automation: Automation, machine: Option<&str>) -> String {
    if automation == Automation::Android {
        return ANDROID_COMPUTER.replace("{model}", model);
    }
    let prompt = COMPUTER
        .replace("{model}", model)
        .replace("{points}", IMAGE_POINTS);
    match machine {
        Some(machine) => prompt
            .replace(LINUX_MACHINE, &format!("a {machine} machine"))
            .replace(XFCE_HINT, ""),
        None => prompt,
    }
}

/// The box's OS and desktop from its `desktop_info`, such as `Ubuntu 24.04 with the XFCE desktop`; none without an OS.
pub fn machine(info: &Value) -> Option<String> {
    let os = info["os"].as_str()?;
    Some(match info["desktop"].as_str() {
        Some(desktop) => format!("{os} with the {desktop} desktop"),
        None => os.to_owned(),
    })
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
    use serde_json::json;

    use super::*;

    #[test]
    fn main_prompt_names_the_serving_model() {
        let p = main(
            "deepseek/deepseek-v4.1-flash",
            "anthropic/claude-sonnet-5",
            false,
            Automation::Minimal,
            &[],
            None,
        );
        assert!(p.starts_with(
            "You are deepseek/deepseek-v4.1-flash, served through the cocoonbox gateway."
        ));
        assert!(p.contains("say exactly \"deepseek/deepseek-v4.1-flash\""));
        assert!(p.contains("/etc/cocoonbox"));
        assert!(!p.contains('{'), "unrendered placeholder: {p}");
        for vendor in ["Claude", "Anthropic", "GPT", "OpenAI", "Gemini"] {
            assert!(!p.contains(vendor), "mentions {vendor}");
        }
        let desk = MAIN
            .replace("{model}", "m")
            .replace("{computer_model}", "anthropic/claude-sonnet-5")
            .replace("{eyes}", "");
        assert!(desk.contains("The subagent runs anthropic/claude-sonnet-5."));
        assert!(desk.contains("Never drive or inspect it from shell"));
        assert!(
            main(
                "anthropic/claude-sonnet-5",
                "x",
                true,
                Automation::Desk,
                &[],
                None
            )
            .contains("call screenshot")
        );
    }

    #[test]
    fn main_prompt_on_a_box_without_a_guide_names_only_shell_and_files() {
        let p = main("m", "c", true, Automation::Minimal, &[], None);
        assert!(p.contains("no desktop or browser tools"), "{p}");
        for tool in [
            "browser_",
            "delegate_computer",
            "request_take_over",
            "screenshot",
        ] {
            assert!(!p.contains(tool), "{tool}: {p}");
        }
    }

    #[test]
    fn main_prompt_on_a_desk_box_ends_with_its_routing() {
        let p = main("m", "c", false, Automation::Desk, &[], None);
        assert!(p.contains("delegate_computer hands one task"), "{p}");
        assert!(p.ends_with(ROUTING), "{p}");
        assert!(ROUTING.contains("script_run") && ROUTING.contains("delegate_computer"));
        assert!(ROUTING.contains("nor rebuild documents in code"));
        assert!(!MAIN.contains("not /workspace"));
        assert!(ROUTING.contains(
            "Save deliverables at the path the user names, else in their home, not /workspace."
        ));
        assert!(
            ROUTING.contains("Edit a file an app has open in that app, or close the app first.")
        );
        assert!(
            ROUTING.contains(
                "Watch a chat or inbox by polling its activity list, not just known pages."
            )
        );
        assert!(ROUTING.contains("Before finishing, check the whole result"));
        assert!(ROUTING.contains("If the task says an input changes later, wait and re-check."));
    }

    #[test]
    fn main_prompt_on_a_desk_box_ends_with_the_images_routing_extra() {
        let extra = ["Use x_run for x.".to_owned()];
        let p = main("m", "c", false, Automation::Desk, &extra, None);
        assert!(
            p.ends_with(&format!("{ROUTING}\n\nUse x_run for x.")),
            "{p}"
        );
        assert!(!main("m", "c", false, Automation::Minimal, &extra, None).contains("x_run"));
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
        let p = computer("anthropic/claude-sonnet-5", Automation::Desk, None);
        assert!(p.starts_with(
            "You are anthropic/claude-sonnet-5, served through the cocoonbox gateway."
        ));
        assert!(!p.contains('{'), "unrendered placeholder: {p}");
    }

    #[test]
    fn computer_prompt_on_a_desk_box_takes_x_y_in_screenshot_pixels() {
        let desk = computer("m", Automation::Desk, None);
        assert!(desk.contains(IMAGE_POINTS), "{desk}");
        assert!(!desk.contains("1280x800 screenshot"), "{desk}");
        assert!(!desk.contains('{'), "unrendered placeholder: {desk}");
    }

    #[test]
    fn a_linux_box_keeps_the_ubuntu_prompts_byte_for_byte() {
        let main = main("m", "c", false, Automation::Desk, &[], None);
        assert!(main.contains("You are the agent that lives in the user's cloud computer: an Ubuntu 24.04 desktop (XFCE, 1280x800) that keeps its disk between conversations. You act for the user with your tools and report back."), "{main}");
        let computer = computer("m", Automation::Desk, None);
        assert!(computer.contains("You operate the graphical desktop of an Ubuntu 24.04 XFCE machine (screen 1280x800) on behalf of another agent. You get one task. Do that task, then stop and report. The Applications menu is the button at the left end of the top panel; a launcher dock sits at the bottom."), "{computer}");
        assert_eq!(
            computer,
            COMPUTER
                .replace("{model}", "m")
                .replace("{points}", IMAGE_POINTS)
        );
    }

    #[test]
    fn a_windows_or_macos_box_names_its_os_and_never_ubuntu() {
        for os in ["Windows 11 25H2", "macOS 26.5"] {
            let main = main("m", "c", false, Automation::Desk, &[], Some(os));
            assert!(
                main.contains(&format!(
                    "cloud computer: a {os} desktop that keeps its disk"
                )),
                "{main}"
            );
            let computer = computer("m", Automation::Desk, Some(os));
            assert!(
                computer.contains(&format!("graphical desktop of a {os} machine on behalf")),
                "{computer}"
            );
            for p in [main, computer] {
                for linux in ["Ubuntu", "XFCE", "Applications menu"] {
                    assert!(!p.contains(linux), "{os} prompt says {linux}: {p}");
                }
            }
        }
    }

    #[test]
    fn the_machine_is_the_os_and_desktop_the_box_reports() {
        assert_eq!(
            machine(&json!({"os": "Windows 11 25H2", "width": 1280})).as_deref(),
            Some("Windows 11 25H2")
        );
        assert_eq!(
            machine(&json!({"os": "Ubuntu 24.04", "desktop": "XFCE"})).as_deref(),
            Some("Ubuntu 24.04 with the XFCE desktop")
        );
        assert_eq!(machine(&Value::Null), None);
    }
}
