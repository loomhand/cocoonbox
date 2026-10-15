//! `computer_act` on a described target: the plan executor's AT-SPI resolve, pressed by action, then a
//! unique OCR match of the target's visible text; neither finding it, or either failing, leaves the
//! grounding to the caller.

use std::path::Path;
use std::time::{Duration, Instant};

use serde::Deserialize;
use serde_json::{Value, json};
use tokio::process::Command;

use super::cua::{Cua, libreoffice};
use super::desk::Desk;
use super::rpc::Output;
use super::script::Scripts;
use super::timing;

const OCR_SCALE: usize = 2;
const OCR_TIMEOUT: Duration = Duration::from_secs(10);
const TESSERACT: &str = "tesseract";
const CROP: [f64; 2] = [320.0, 200.0];
const FIELD_WORDS: [&str; 5] = ["field", "input", "box", "entry", "textbox"];
const TEXT_ROLES: [&str; 3] = ["text", "label", "static"];
const MIN_PREFIX: usize = 3;

#[derive(Deserialize)]
struct Target {
    description: String,
    #[serde(default)]
    text: Option<String>,
}

#[derive(Deserialize)]
struct TargetArgs {
    action: String,
    target: Target,
    #[serde(default)]
    max_edge: Option<u32>,
}

/// One OCR word: its text folded to lowercase letters and digits, its line key and its box in screen pixels.
struct Word {
    norm: String,
    line: String,
    rect: [f64; 4],
}

/// Runs `computer_act` with `target`: `locate` only finds it, `click` also presses it; returns the
/// tier that found it, the point in the caller's screenshot pixels and a crop around it.
pub async fn act(
    cua: &Cua,
    desk: &Desk,
    scripts: &Scripts,
    workspace: &Path,
    args: Value,
) -> Result<Output, String> {
    let k = cua.screenshot_scale(&args);
    let a: TargetArgs =
        serde_json::from_value(args).map_err(|e| format!("invalid arguments: {e}"))?;
    let press = match a.action.as_str() {
        "click" => true,
        "locate" => false,
        other => return Err(format!("target takes action click or locate, not {other}")),
    };
    let start = Instant::now();
    let mut found = scripts
        .locate(workspace, &a.target.description, press)
        .await
        .unwrap_or_else(|e| json!({"found": false, "why": e}));
    timing::mark("a11y");
    let a11y_ms = start.elapsed().as_millis();
    let element = format!(
        "{} \"{}\"",
        found["role"].as_str().unwrap_or_default(),
        found["name"].as_str().unwrap_or_default()
    );
    let place = placed(&found, libreoffice);
    if found["found"] == true && (press || place.is_ok()) {
        let rect = place.ok();
        let point = rect.map(|[x, y, w, h]| [(x + w / 2.0) / k, (y + h / 2.0) / k]);
        let mut out = json!({
            "tier": "a11y",
            "how": found["how"],
            "element": element,
            "window": found["window"],
            "point": point,
            "scale": k,
            "pressed": press.then(|| found["via"].clone()),
            "ms": start.elapsed().as_millis(),
        });
        if let Err(why) = place {
            out["no_point"] = why.into();
        }
        if found["served"].is_array() {
            out["served"] = found["served"].take();
        }
        return Ok(finish(cua, &mut out, rect));
    }
    let mut out = match place.err().filter(|_| found["found"] == true) {
        Some(why) => json!({"a11y": {"element": element, "no_point": why, "ms": a11y_ms}}),
        None => json!({"a11y": {"why": found["why"], "ms": a11y_ms}}),
    };
    if found["served"].is_array() {
        out["a11y"]["served"] = found["served"].take();
    }
    let Some(text) = a.target.text.as_deref().filter(|t| !norm(t).is_empty()) else {
        return not_found(cua, out, a.max_edge, start).await;
    };
    let t0 = Instant::now();
    let words = match ocr(cua).await {
        Ok(words) => words,
        Err(e) => {
            out["ocr"] = json!({"error": e, "ms": t0.elapsed().as_millis()});
            return not_found(cua, out, a.max_edge, start).await;
        }
    };
    timing::mark("ocr");
    let hits = matches(&words, text);
    out["ocr"] = json!({"matches": hits.len(), "ms": t0.elapsed().as_millis()});
    let [rect] = hits.as_slice() else {
        return not_found(cua, out, a.max_edge, start).await;
    };
    let (cx, cy) = (rect[0] + rect[2] / 2.0, rect[1] + rect[3] / 2.0);
    if let Some(why) = label_of_field(desk, &a.target.description, text, cx, cy).await {
        out["ocr"]["refused"] = why.into();
        return not_found(cua, out, a.max_edge, start).await;
    }
    if press {
        cua.desk_input("click", json!({"x": cx.round(), "y": cy.round()}))
            .await?;
        cua.settled().await;
    }
    out = json!({
        "tier": "ocr",
        "text": text,
        "point": [cx / k, cy / k],
        "scale": k,
        "pressed": press,
        "ocr_ms": t0.elapsed().as_millis(),
        "a11y": out["a11y"],
        "ms": start.elapsed().as_millis(),
    });
    Ok(finish(cua, &mut out, Some(*rect)))
}

/// The box to point at for the found element, else why none: hidden, unbounded, or a LibreOffice control without a `shift`.
fn placed(found: &Value, office: impl Fn(i64) -> bool) -> Result<[f64; 4], &'static str> {
    if found["shown"] == false {
        return Err("it does not show, like an entry of a closed menu");
    }
    let [x, y, w, h] = rect_of(&found["bounds"]).ok_or("it has no bounds")?;
    let role = found["role"].as_str().unwrap_or_default();
    if role.starts_with("menu") || !found["pid"].as_i64().is_some_and(office) {
        return Ok([x, y, w, h]);
    }
    let dy = found["shift"]
        .as_f64()
        .ok_or("LibreOffice's extents can sit a menu bar's height above the drawn control")?;
    Ok([x, y + dy, w, h])
}

/// The result line and a full-resolution crop of [`CROP`] around `rect`, else no image.
fn finish(cua: &Cua, out: &mut Value, rect: Option<[f64; 4]>) -> Output {
    let image_b64 = rect.and_then(|[x, y, w, h]| {
        let (cx, cy) = (x + w / 2.0, y + h / 2.0);
        cua.crop([cx - CROP[0] / 2.0, cy - CROP[1] / 2.0, CROP[0], CROP[1]])
    });
    if let Some(b64) = &image_b64 {
        out["crop"] = json!({"w": CROP[0], "h": CROP[1], "bytes": b64.len() / 4 * 3});
    }
    Output {
        text: out.to_string(),
        is_error: false,
        image_b64,
        ..Output::default()
    }
}

/// No tier found the target: the reasons, and the whole screen so the caller grounds it.
async fn not_found(
    cua: &Cua,
    mut out: Value,
    max_edge: Option<u32>,
    start: Instant,
) -> Result<Output, String> {
    out["tier"] = "none".into();
    out["ms"] = json!(start.elapsed().as_millis());
    Ok(Output {
        text: format!(
            "{out}\nnot found by its accessibility name or a unique match of its text; give x,y from the screenshot"
        ),
        is_error: true,
        image_b64: cua.screenshot_once(max_edge).await?,
        ..Output::default()
    })
}

/// The screen's words by tesseract on a copy scaled up [`OCR_SCALE`] times, boxes in screen pixels.
async fn ocr(cua: &Cua) -> Result<Vec<Word>, String> {
    let (w, h, rgb) = cua.screen_rgb().ok_or("no screen capture over MIT-SHM")?;
    let (sw, sh, big) = upscale(w as usize, h as usize, &rgb, OCR_SCALE);
    let path = std::env::temp_dir().join(format!("cb-ocr-{}.png", uuid::Uuid::new_v4().simple()));
    let png = png_of(sw, sh, &big)?;
    tokio::fs::write(&path, png)
        .await
        .map_err(|e| format!("write {}: {e}", path.display()))?;
    let run = Command::new(TESSERACT)
        .arg(&path)
        .args(["-", "--psm", "11", "tsv"])
        .kill_on_drop(true)
        .output();
    let done = tokio::time::timeout(OCR_TIMEOUT, run).await;
    let _ = tokio::fs::remove_file(&path).await;
    let output = done
        .map_err(|_| format!("tesseract took over {} s", OCR_TIMEOUT.as_secs()))?
        .map_err(|e| format!("run {TESSERACT}: {e}"))?;
    if !output.status.success() {
        return Err(format!(
            "tesseract failed: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        ));
    }
    Ok(parse_tsv(
        &String::from_utf8_lossy(&output.stdout),
        OCR_SCALE as f64,
    ))
}

fn parse_tsv(tsv: &str, scale: f64) -> Vec<Word> {
    tsv.lines()
        .skip(1)
        .filter_map(|line| {
            let f: Vec<&str> = line.split('\t').collect();
            let (text, conf) = (f.get(11)?.trim(), f.get(10)?.parse::<f64>().ok()?);
            let n = norm(text);
            if n.is_empty() || conf < 0.0 {
                return None;
            }
            let num = |i: usize| f.get(i).and_then(|v| v.parse::<f64>().ok());
            Some(Word {
                norm: n,
                line: f[1..5].join("."),
                rect: [
                    num(6)? / scale,
                    num(7)? / scale,
                    num(8)? / scale,
                    num(9)? / scale,
                ],
            })
        })
        .collect()
}

/// The boxes of every run of consecutive words on one line that spells `label`; its last word may be cut off.
fn matches(words: &[Word], label: &str) -> Vec<[f64; 4]> {
    let want: Vec<String> = label
        .split_whitespace()
        .map(norm)
        .filter(|t| !t.is_empty())
        .collect();
    if want.is_empty() {
        return Vec::new();
    }
    (0..words.len().saturating_sub(want.len() - 1))
        .filter_map(|i| {
            let run = &words[i..i + want.len()];
            let line = run.iter().all(|w| w.line == run[0].line);
            let spelled = run.iter().zip(&want).enumerate().all(|(j, (w, t))| {
                w.norm == *t
                    || (j + 1 == want.len()
                        && w.norm.len().min(t.len()) >= MIN_PREFIX
                        && (t.starts_with(&w.norm) || w.norm.starts_with(t.as_str())))
            });
            (line && spelled).then(|| {
                let x0 = run.iter().map(|w| w.rect[0]).fold(f64::MAX, f64::min);
                let y0 = run.iter().map(|w| w.rect[1]).fold(f64::MAX, f64::min);
                let x1 = run
                    .iter()
                    .map(|w| w.rect[0] + w.rect[2])
                    .fold(f64::MIN, f64::max);
                let y1 = run
                    .iter()
                    .map(|w| w.rect[1] + w.rect[3])
                    .fold(f64::MIN, f64::max);
                [x0, y0, x1 - x0, y1 - y0]
            })
        })
        .collect()
}

/// Why a match of `text` at `(x, y)` is a field's label rather than the field the description names, if it
/// is: the description names a field in words outside the text, and the point holds plain text.
async fn label_of_field(
    desk: &Desk,
    description: &str,
    text: &str,
    x: f64,
    y: f64,
) -> Option<String> {
    let own: Vec<String> = text.split_whitespace().map(norm).collect();
    let d = description.to_lowercase();
    let words: Vec<&str> = d
        .split(|c: char| !c.is_alphanumeric())
        .filter(|t| !own.iter().any(|o| o == t))
        .collect();
    if !FIELD_WORDS.iter().any(|w| words.contains(w)) {
        return None;
    }
    let at = desk
        .at_point(x.round() as i32, y.round() as i32)
        .await
        .ok()?;
    let found = at.get("found").filter(|f| !f.is_null())?;
    let role = found["role"].as_str().unwrap_or_default();
    TEXT_ROLES.contains(&role).then(|| {
        format!(
            "the text is {role} \"{}\" beside a field, not the field",
            found["name"].as_str().unwrap_or_default()
        )
    })
}

fn rect_of(v: &Value) -> Option<[f64; 4]> {
    let b = v.as_array()?;
    let n = |i: usize| b.get(i).and_then(Value::as_f64);
    Some([n(0)?, n(1)?, n(2)?, n(3)?])
}

fn norm(s: &str) -> String {
    s.chars()
        .filter(char::is_ascii_alphanumeric)
        .map(|c| c.to_ascii_lowercase())
        .collect()
}

/// `rgb` of `w` x `h` scaled up `f` times, bilinear.
fn upscale(w: usize, h: usize, rgb: &[u8], f: usize) -> (u32, u32, Vec<u8>) {
    let (bw, bh) = (w * f, h * f);
    let mut out = vec![0u8; bw * bh * 3];
    for y in 0..bh {
        let sy = ((y as f64 + 0.5) / f as f64 - 0.5).max(0.0);
        let (y0, fy) = (sy.floor() as usize, sy.fract());
        let y1 = (y0 + 1).min(h - 1);
        for x in 0..bw {
            let sx = ((x as f64 + 0.5) / f as f64 - 0.5).max(0.0);
            let (x0, fx) = (sx.floor() as usize, sx.fract());
            let x1 = (x0 + 1).min(w - 1);
            for c in 0..3 {
                let p = |xx: usize, yy: usize| f64::from(rgb[(yy * w + xx) * 3 + c]);
                let top = p(x0, y0) * (1.0 - fx) + p(x1, y0) * fx;
                let bottom = p(x0, y1) * (1.0 - fx) + p(x1, y1) * fx;
                out[(y * bw + x) * 3 + c] = (top * (1.0 - fy) + bottom * fy).round() as u8;
            }
        }
    }
    (bw as u32, bh as u32, out)
}

fn png_of(w: u32, h: u32, rgb: &[u8]) -> Result<Vec<u8>, String> {
    let mut out = Vec::new();
    let mut enc = png::Encoder::new(&mut out, w, h);
    enc.set_color(png::ColorType::Rgb);
    enc.set_depth(png::BitDepth::Eight);
    enc.write_header()
        .and_then(|mut wr| wr.write_image_data(rgb))
        .map_err(|e| format!("encode PNG: {e}"))?;
    Ok(out)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn word(text: &str, line: &str, x: f64) -> Word {
        Word {
            norm: norm(text),
            line: line.to_owned(),
            rect: [x, 10.0, 20.0, 8.0],
        }
    }

    #[test]
    fn matches_a_label_on_one_line_and_a_cut_last_word() {
        let words = [
            word("Default", "1.1.1.1", 0.0),
            word("Paragraph", "1.1.1.1", 30.0),
            word("St", "1.1.1.1", 60.0),
            word("Save", "1.1.2.1", 0.0),
            word("Save", "1.1.3.1", 50.0),
        ];
        assert_eq!(matches(&words, "Default Paragraph").len(), 1);
        assert_eq!(matches(&words, "Save").len(), 2);
        assert!(matches(&words, "Paragraph Save").is_empty());
        assert_eq!(matches(&words, "Insert Image...").len(), 0);
    }

    #[test]
    fn a_short_label_needs_the_whole_word() {
        let words = [word("Okay", "1.1.1.1", 0.0), word("Google", "1.1.2.1", 0.0)];
        assert!(matches(&words, "OK").is_empty());
        assert!(matches(&words, "Go").is_empty());
        assert_eq!(matches(&words, "Okay").len(), 1);
        assert_eq!(matches(&words, "Goog").len(), 1);
    }

    #[test]
    fn a_libreoffice_control_outside_a_menu_is_placed_by_its_shift_else_has_no_point() {
        let office = |pid: i64| pid == 7;
        let at = |role: &str, pid: Option<i64>, shift: Value| json!({"role": role, "pid": pid, "shown": true, "bounds": [10, 20, 30, 30], "shift": shift});
        assert!(placed(&at("button", Some(7), Value::Null), office).is_err());
        assert_eq!(
            placed(&at("button", Some(7), json!(25)), office),
            Ok([10.0, 45.0, 30.0, 30.0])
        );
        assert_eq!(
            placed(&at("button", Some(7), json!(0)), office),
            Ok([10.0, 20.0, 30.0, 30.0])
        );
        assert_eq!(
            placed(&at("menuitem", Some(7), Value::Null), office),
            Ok([10.0, 20.0, 30.0, 30.0])
        );
        assert_eq!(
            placed(&at("button", Some(8), Value::Null), office),
            Ok([10.0, 20.0, 30.0, 30.0])
        );
    }

    #[test]
    fn a_hidden_or_unbounded_element_has_no_point() {
        let none = |_: i64| false;
        let closed = json!({"role": "menuitem", "shown": false, "bounds": [0, -400, 80, 20]});
        let unbounded = json!({"role": "button", "shown": true, "bounds": null});
        let drawn = json!({"role": "button", "shown": true, "bounds": [1, 2, 3, 4]});
        assert!(placed(&closed, none).is_err());
        assert!(placed(&unbounded, none).is_err());
        assert!(placed(&drawn, none).is_ok());
    }

    #[test]
    fn parse_tsv_keeps_words_and_scales_boxes_back() {
        let tsv = "level\tpage_num\tblock_num\tpar_num\tline_num\tword_num\tleft\ttop\twidth\theight\tconf\ttext\n\
                   5\t1\t2\t1\t1\t1\t100\t40\t60\t20\t91.5\tCancel\n\
                   4\t1\t2\t1\t1\t0\t100\t40\t60\t20\t-1\t\n";
        let words = parse_tsv(tsv, 2.0);
        assert_eq!(words.len(), 1);
        assert_eq!(words[0].norm, "cancel");
        assert_eq!(words[0].rect, [50.0, 20.0, 30.0, 10.0]);
    }

    #[test]
    fn upscale_doubles_the_size_and_keeps_flat_colour() {
        let rgb = vec![10u8; 4 * 3 * 3];
        let (w, h, big) = upscale(4, 3, &rgb, 2);
        assert_eq!((w, h), (8, 6));
        assert!(big.iter().all(|v| *v == 10));
    }
}
