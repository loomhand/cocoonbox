//! Card values typed from a file: no text that a model, the transcript or a client gets keeps them.

use std::sync::{PoisonError, RwLock};

const NUMBER: &str = "[card number]";
const CVC: &str = "[card cvc]";
const CVC_LABELS: [&str; 5] = ["cvc", "cvv", "csc", "security code", "card code"];
const LABEL_REACH: usize = 48;
const MIN_PAN: usize = 12;

/// The registry of this process; it lives as long as the process and is never persisted.
pub static SECRETS: Secrets = Secrets::new();

/// Registered card values and their replacements.
pub struct Secrets {
    values: RwLock<Vec<Secret>>,
}

struct Secret {
    value: String,
    label: &'static str,
    /// Matches only as a whole number, and in tool output only near a CVC label, since three digits
    /// also occur there as coordinates.
    labelled: bool,
}

impl Secrets {
    /// An empty registry.
    pub const fn new() -> Self {
        Self {
            values: RwLock::new(Vec::new()),
        }
    }

    /// Registers card `field` typed as `value`: the number as typed, as plain digits and in groups
    /// split by spaces or dashes, and the cvc; other fields are not secret.
    pub fn register(&self, field: &str, value: &str) {
        let digits: String = value.chars().filter(char::is_ascii_digit).collect();
        let mut add = Vec::new();
        match field {
            "number" if digits.len() >= MIN_PAN => {
                let mut sizes = vec![4; digits.len() / 4];
                if !digits.len().is_multiple_of(4) {
                    sizes.push(digits.len() % 4);
                }
                let mut layouts = vec![sizes];
                if digits.len() == 15 {
                    layouts.push(vec![4, 6, 5]);
                }
                for sizes in layouts {
                    let groups = split(&digits, &sizes);
                    add.push(groups.join(" "));
                    add.push(groups.join("-"));
                }
                add.push(value.to_owned());
                add.push(digits);
            }
            "cvc" if (3..=4).contains(&digits.len()) => add.push(digits),
            _ => return,
        }
        let (label, labelled) = if field == "cvc" {
            (CVC, true)
        } else {
            (NUMBER, false)
        };
        let mut values = self.values.write().unwrap_or_else(PoisonError::into_inner);
        for value in add {
            if !values.iter().any(|s| s.value == value) {
                values.push(Secret {
                    value,
                    label,
                    labelled,
                });
            }
        }
    }

    /// Replaces every registered value in tool output `text`; with none registered it costs one lock read.
    pub fn scrub(&self, text: &mut String) {
        self.replace(text, false);
    }

    /// Like [`Secrets::scrub`] for text a model wrote, where a cvc goes even far from its label.
    pub fn scrub_prose(&self, text: &mut String) {
        self.replace(text, true);
    }

    fn replace(&self, text: &mut String, prose: bool) {
        let values = self.values.read().unwrap_or_else(PoisonError::into_inner);
        for s in values.iter() {
            if !text.contains(&s.value) {
                continue;
            }
            *text = if s.labelled {
                replace_whole(text, &s.value, s.label, prose)
            } else {
                text.replace(&s.value, s.label)
            };
        }
    }
}

impl Default for Secrets {
    fn default() -> Self {
        Self::new()
    }
}

fn split<'a>(digits: &'a str, sizes: &[usize]) -> Vec<&'a str> {
    let mut at = 0;
    sizes
        .iter()
        .map(|n| {
            let group = &digits[at..at + n];
            at += n;
            group
        })
        .collect()
}

/// Replaces `value` where it stands as a whole number, unless `anywhere`, only within reach of a CVC label.
fn replace_whole(text: &str, value: &str, label: &str, anywhere: bool) -> String {
    let digit = |c: char| c.is_ascii_digit();
    let labelled = |i: usize, end: usize| {
        let near = text[text.floor_char_boundary(i.saturating_sub(LABEL_REACH))
            ..text.ceil_char_boundary(end + LABEL_REACH)]
            .to_ascii_lowercase();
        CVC_LABELS.iter().any(|l| near.contains(l))
    };
    let mut out = String::with_capacity(text.len());
    let mut last = 0;
    for (i, _) in text.match_indices(value) {
        let end = i + value.len();
        if text[..i].ends_with(digit)
            || text[end..].starts_with(digit)
            || !anywhere && !labelled(i, end)
        {
            continue;
        }
        out.push_str(&text[last..i]);
        out.push_str(label);
        last = end;
    }
    out.push_str(&text[last..]);
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    const PAN: &str = "4392027523501133";
    const AMEX: &str = "371449635398431";

    fn scrubbed(s: &Secrets, text: &str) -> String {
        let mut text = text.to_owned();
        s.scrub(&mut text);
        text
    }

    #[test]
    fn an_empty_registry_changes_nothing() {
        let s = Secrets::new();
        assert_eq!(scrubbed(&s, PAN), PAN);
    }

    #[test]
    fn a_number_is_replaced_plain_and_in_groups_of_four() {
        let s = Secrets::new();
        s.register("number", PAN);
        for text in [
            format!("The Card number field is now filled with {PAN}."),
            "value=\"4392 0275 2350 1133\"".to_owned(),
            "card 4392-0275-2350-1133 ok".to_owned(),
            format!("x{PAN}9"),
        ] {
            let out = scrubbed(&s, &text);
            assert!(out.contains(NUMBER), "{out}");
            assert!(!out.contains("1133"), "{out}");
        }
        assert_eq!(
            scrubbed(&s, "a 4392 0275 b"),
            "a 4392 0275 b",
            "a partial number stays"
        );
    }

    #[test]
    fn a_fifteen_digit_number_is_also_replaced_in_four_six_five_groups() {
        let s = Secrets::new();
        s.register("number", AMEX);
        for text in [
            "3714 496353 98431",
            "3714-496353-98431",
            "3714 4963 5398 431",
        ] {
            assert_eq!(scrubbed(&s, text), NUMBER, "{text}");
        }
    }

    #[test]
    fn a_cvc_is_replaced_only_as_a_whole_number_near_its_label() {
        let s = Secrets::new();
        s.register("cvc", "739");
        assert_eq!(
            scrubbed(&s, "Filled the CVC field with 739."),
            format!("Filled the CVC field with {CVC}.")
        );
        assert_eq!(
            scrubbed(&s, "textbox \"Security code\" value=\"739\""),
            format!("textbox \"Security code\" value=\"{CVC}\"")
        );
        for kept in ["button \"OK\" center=(739, 412)", "CVC: see 17390 and 2739"] {
            assert_eq!(scrubbed(&s, kept), kept);
        }
        let mut report = "- **CVC** field: filled with a 3-digit value (shown on screen: 739); clicked at (17390, 739)".to_owned();
        s.scrub_prose(&mut report);
        assert_eq!(
            report,
            format!(
                "- **CVC** field: filled with a 3-digit value (shown on screen: {CVC}); clicked at (17390, {CVC})"
            )
        );
    }

    #[test]
    fn other_fields_are_not_registered() {
        let s = Secrets::new();
        s.register("exp", "07/31");
        s.register("billing_address.postal_code", "94107");
        s.register("number", "4242");
        assert!(s.values.read().unwrap().is_empty());
    }
}
