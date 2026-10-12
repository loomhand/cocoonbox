//! Card values typed from a file: no text that a model, the transcript or a client gets keeps them.

use std::sync::{PoisonError, RwLock};

const NUMBER: &str = "[card number]";
const CVC: &str = "[card cvc]";
const CVC_LABELS: [&str; 5] = ["cvc", "cvv", "csc", "security code", "card code"];
const LABEL_REACH: usize = 48;
const MIN_PAN: usize = 12;
const MIN_FRAGMENT: usize = 8;
const MISREAD_SPAN: usize = 10;

/// The registry of this process; it lives as long as the process and is never persisted.
pub static SECRETS: Secrets = Secrets::new();

/// Registered card numbers as plain digits, and cvcs.
pub struct Secrets {
    values: RwLock<Registry>,
}

struct Registry {
    numbers: Vec<String>,
    cvcs: Vec<String>,
}

impl Secrets {
    /// An empty registry.
    pub const fn new() -> Self {
        Self {
            values: RwLock::new(Registry {
                numbers: Vec::new(),
                cvcs: Vec::new(),
            }),
        }
    }

    /// Registers card `field` typed as `value`: the number and the cvc by their digits; other fields
    /// are not secret.
    pub fn register(&self, field: &str, value: &str) {
        let digits: String = value.chars().filter(char::is_ascii_digit).collect();
        let mut values = self.values.write().unwrap_or_else(PoisonError::into_inner);
        let list = match field {
            "number" if digits.len() >= MIN_PAN => &mut values.numbers,
            "cvc" if (3..=4).contains(&digits.len()) => &mut values.cvcs,
            _ => return,
        };
        if !list.contains(&digits) {
            list.push(digits);
        }
    }

    /// Replaces every registered value in tool output `text`; with none registered it costs one lock read.
    pub fn scrub(&self, text: &mut String) {
        self.replace(text, false);
    }

    /// Like [`Secrets::scrub`] for text a model wrote, where a cvc goes even far from its label and a
    /// number also when misread.
    pub fn scrub_prose(&self, text: &mut String) {
        self.replace(text, true);
    }

    /// The next part of streamed model text `text`, of which the first `sent` bytes went out, scrubbed
    /// like [`Secrets::scrub_prose`]; a trailing number that more text could extend is held until `end`.
    /// With none registered it holds nothing back and scrubs nothing.
    pub fn stream(&self, text: &str, sent: &mut usize, end: bool) -> String {
        let values = self.values.read().unwrap_or_else(PoisonError::into_inner);
        let from = *sent;
        if values.numbers.is_empty() && values.cvcs.is_empty() {
            *sent = text.len();
            return text[from..].to_owned();
        }
        *sent = if end {
            text.len()
        } else {
            open_run(text).max(from)
        };
        let mut part = text[from..*sent].to_owned();
        values.replace(&mut part, true);
        part
    }

    fn replace(&self, text: &mut String, prose: bool) {
        self.values
            .read()
            .unwrap_or_else(PoisonError::into_inner)
            .replace(text, prose);
    }
}

impl Registry {
    fn replace(&self, text: &mut String, prose: bool) {
        if !self.numbers.is_empty()
            && let Some(out) = replace_numbers(text, &self.numbers, prose)
        {
            *text = out;
        }
        for cvc in &self.cvcs {
            if text.contains(cvc.as_str()) {
                *text = replace_whole(text, cvc, CVC, prose);
            }
        }
    }
}

impl Default for Secrets {
    fn default() -> Self {
        Self::new()
    }
}

/// Where the run of digits joined by single spaces, dashes or dots that ends `text`, or ends it but for one
/// such separator, starts; the length of `text` when none does.
fn open_run(text: &str) -> usize {
    let bytes = text.as_bytes();
    let separator = |i: usize| matches!(bytes[i], b' ' | b'-' | b'.');
    let mut i = bytes.len();
    if i >= 2 && separator(i - 1) && bytes[i - 2].is_ascii_digit() {
        i -= 1;
    }
    if i == 0 || !bytes[i - 1].is_ascii_digit() {
        return bytes.len();
    }
    loop {
        while i > 0 && bytes[i - 1].is_ascii_digit() {
            i -= 1;
        }
        if i >= 2 && separator(i - 1) && bytes[i - 2].is_ascii_digit() {
            i -= 1;
        } else {
            return i;
        }
    }
}

/// Replaces each run of digits joined by single spaces, dashes or dots whose digits hold a registered
/// number or, when `misread`, share a stretch of [`MISREAD_SPAN`] digits with one, are one edit from
/// one or are a stretch of one of at least [`MIN_FRAGMENT`] digits; `None` when none does.
fn replace_numbers(text: &str, numbers: &[String], misread: bool) -> Option<String> {
    let bytes = text.as_bytes();
    let mut out = String::new();
    let mut digits = String::new();
    let (mut last, mut i) = (0, 0);
    while i < bytes.len() {
        if !bytes[i].is_ascii_digit() {
            i += 1;
            continue;
        }
        let start = i;
        digits.clear();
        loop {
            while let Some(&b) = bytes.get(i).filter(|b| b.is_ascii_digit()) {
                digits.push(b.into());
                i += 1;
            }
            match bytes.get(i..i + 2) {
                Some([b' ' | b'-' | b'.', d]) if d.is_ascii_digit() => i += 1,
                _ => break,
            }
        }
        let hit = numbers.iter().any(|n| {
            if digits.len() < MIN_PAN {
                return misread && digits.len() >= MIN_FRAGMENT && n.contains(digits.as_str());
            }
            digits.contains(n.as_str())
                || misread
                    && ((0..=n.len() - MISREAD_SPAN)
                        .any(|at| digits.contains(&n[at..at + MISREAD_SPAN]))
                        || one_edit(digits.as_bytes(), n.as_bytes()))
        });
        if hit {
            out.push_str(&text[last..start]);
            out.push_str(NUMBER);
            last = i;
        }
    }
    (last > 0).then(|| {
        out.push_str(&text[last..]);
        out
    })
}

/// Whether `a` and `b` differ by at most one substituted, inserted or deleted digit or one swap of
/// adjacent digits.
fn one_edit(a: &[u8], b: &[u8]) -> bool {
    let (short, long) = if a.len() <= b.len() { (a, b) } else { (b, a) };
    if long.len() - short.len() > 1 {
        return false;
    }
    let at = short.iter().zip(long).take_while(|(x, y)| x == y).count();
    if short.len() < long.len() {
        return short[at..] == long[at + 1..];
    }
    at == short.len()
        || short[at + 1..] == long[at + 1..]
        || short.get(at + 1) == Some(&long[at])
            && long[at + 1] == short[at]
            && short[at + 2..] == long[at + 2..]
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
    const TEST_PAN: &str = "4242424242424242";

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
        let values = s.values.read().unwrap();
        assert!(values.numbers.is_empty() && values.cvcs.is_empty());
    }

    #[test]
    fn a_number_with_odd_separators_is_replaced() {
        let s = Secrets::new();
        s.register("number", TEST_PAN);
        for text in [
            "- Card number: 42424242424242 42 (16 digits shown)",
            "4242 4242 4242 4242",
            "4242-4242-4242-4242",
            "4242.4242.4242.4242",
            "4 2 4 2 4 2 4 2 4 2 4 2 4 2 4 2",
            "424 2424 24242 4242",
            "ref 994242424242424242",
        ] {
            let out = scrubbed(&s, text);
            assert!(out.contains(NUMBER) && !out.contains("4242"), "{out}");
        }
    }

    #[test]
    fn a_misread_number_is_replaced_only_in_model_text() {
        let s = Secrets::new();
        s.register("number", TEST_PAN);
        s.register("number", PAN);
        for text in [
            "Card number: 4242 4242 4243 4242",
            "Card number: 424242424242424",
            "Card number: 4392 0275 2350 1137",
            "Card number: 7392 0275 2350 1133",
        ] {
            let mut prose = text.to_owned();
            s.scrub_prose(&mut prose);
            assert_eq!(prose, format!("Card number: {NUMBER}"));
            assert_eq!(scrubbed(&s, text), text);
        }
    }

    fn replaced_only_in_model_text(near: &str) {
        let s = Secrets::new();
        s.register("number", PAN);
        let text = format!("Card number: {near}.");
        assert_eq!(whole_prose(&s, &text), format!("Card number: {NUMBER}."));
        assert_eq!(scrubbed(&s, &text), text);
    }

    #[test]
    fn a_number_with_one_digit_changed_is_replaced_only_in_model_text() {
        replaced_only_in_model_text("4392027513501133");
        replaced_only_in_model_text("4392 0275 1350 1133");
    }

    #[test]
    fn a_number_with_two_adjacent_digits_swapped_is_replaced_only_in_model_text() {
        replaced_only_in_model_text("4392027253501133");
    }

    #[test]
    fn a_number_with_one_digit_inserted_is_replaced_only_in_model_text() {
        replaced_only_in_model_text("43920275923501133");
    }

    #[test]
    fn a_number_with_one_digit_dropped_is_replaced_only_in_model_text() {
        replaced_only_in_model_text("439202753501133");
    }

    #[test]
    fn a_near_number_in_mixed_separators_never_goes_out() {
        let s = Secrets::new();
        s.register("number", PAN);
        let text = "gives 4392 0275-1350.1133 = 43 920 2751 35 01133. Done";
        let want = format!("gives {NUMBER} = {NUMBER}. Done");
        assert_eq!(whole_prose(&s, text), want);
        for at in 0..=text.len() {
            assert_eq!(
                streamed(&s, &[&text[..at], &text[at..]]),
                want,
                "split at {at}"
            );
        }
    }

    #[test]
    fn a_number_two_edits_away_stays() {
        let s = Secrets::new();
        s.register("number", PAN);
        for kept in [
            "4392027513501137",
            "439202751350113",
            "4392072513501133",
            "43920275193501133",
        ] {
            assert_eq!(whole_prose(&s, kept), kept);
        }
    }

    #[test]
    fn ordinary_numbers_stay_with_a_number_registered() {
        let s = Secrets::new();
        s.register("number", TEST_PAN);
        s.register("number", PAN);
        for kept in [
            "2026-10-12 19:15:29.123 started",
            "1791748802356 ms, at 2026-10-12T19:15:29Z",
            "clicked (424, 242) then (1242, 4242)",
            "button center=4242 424",
            "call +1 415-555-0100 or +86 138 0013 8000",
            "order 112-4567890-1234567 and #4242424",
            "version 1.2.42.4242, ip 10.203.0.1",
            "at 1791750880206 ms, 2026-10-12T01:24:07.4242424Z",
            "point (1242, 424), 37.7749295, -323.4392027",
            "call 415 555 0100, +44 20 7946 0958, 138-0013-8000",
            "order #4392027 and 2350113, ticket 424-2424, id 7523501",
        ] {
            let mut prose = kept.to_owned();
            s.scrub_prose(&mut prose);
            assert_eq!(prose, kept);
            assert_eq!(scrubbed(&s, kept), kept);
        }
    }

    fn streamed(s: &Secrets, deltas: &[&str]) -> String {
        let (mut text, mut sent, mut out) = (String::new(), 0, String::new());
        for d in deltas {
            text.push_str(d);
            out.push_str(&s.stream(&text, &mut sent, false));
        }
        out + &s.stream(&text, &mut sent, true)
    }

    fn whole_prose(s: &Secrets, text: &str) -> String {
        let mut text = text.to_owned();
        s.scrub_prose(&mut text);
        text
    }

    #[test]
    fn a_fragment_is_replaced_only_in_model_text() {
        let s = Secrets::new();
        s.register("number", PAN);
        s.register("number", TEST_PAN);
        for (text, want) in [
            ("id 43920275 and 23501133", "id {} and {}"),
            (
                "first half 4392 0275, then 2350",
                "first half {}, then 2350",
            ),
            ("it ends in 75235011", "it ends in {}"),
            ("digits 4392-0275-235", "digits {}"),
            ("digits 39202752350", "digits {}"),
            ("#424242424", "#{}"),
            ("4242 4242 42 then", "{} then"),
        ] {
            assert_eq!(whole_prose(&s, text), want.replace("{}", NUMBER));
            assert_eq!(scrubbed(&s, text), text);
        }
        for kept in [
            "4392027 and 3501133",
            "9392027523",
            "24242424 4",
            "4242 4243",
        ] {
            assert_eq!(whole_prose(&s, kept), kept);
        }
    }

    #[test]
    fn a_number_split_across_deltas_never_goes_out() {
        let s = Secrets::new();
        s.register("number", TEST_PAN);
        s.register("number", AMEX);
        s.register("cvc", "739");
        for form in [
            "4242 4242 4242 4242",
            "42424242424242 42",
            "4242-4242-4242-4242",
            "4242.4242.4242.4242",
            "4 2 4 2 4 2 4 2 4 2 4 2 4 2 4 2",
            "424 2424 24242 4242",
            "4242 4242 4243 4242",
            "3714 496353 98431",
            "4242 4242",
            "CVC 739",
        ] {
            let text = format!("Card: {form}. Done at 12:05");
            let want = whole_prose(&s, &text);
            assert!(!want.contains(form), "{want}");
            for at in 0..=text.len() {
                let out = streamed(&s, &[&text[..at], &text[at..]]);
                assert_eq!(out, want, "split at {at}");
            }
            let chars: Vec<String> = text.chars().map(String::from).collect();
            let chars: Vec<&str> = chars.iter().map(String::as_str).collect();
            assert_eq!(streamed(&s, &chars), want, "one char per delta");
        }
    }

    #[test]
    fn a_stream_holds_only_a_trailing_number() {
        let s = Secrets::new();
        s.register("number", TEST_PAN);
        let mut sent = 0;
        assert_eq!(
            s.stream("Clicked at step 12", &mut sent, false),
            "Clicked at step "
        );
        assert_eq!(s.stream("Clicked at step 12 -", &mut sent, false), "12 -");
        assert_eq!(
            s.stream("Clicked at step 12 - 4242 ", &mut sent, false),
            " "
        );
        assert_eq!(
            s.stream("Clicked at step 12 - 4242 x", &mut sent, false),
            "4242 x"
        );
        assert_eq!(
            s.stream("Clicked at step 12 - 4242 x 7", &mut sent, true),
            " 7"
        );
    }

    #[test]
    fn an_empty_registry_streams_every_delta_whole() {
        let s = Secrets::new();
        let mut sent = 0;
        assert_eq!(
            s.stream("Card: 4242 4242", &mut sent, false),
            "Card: 4242 4242"
        );
        assert_eq!(
            s.stream("Card: 4242 4242 4242 4242", &mut sent, false),
            " 4242 4242"
        );
    }

    #[test]
    fn deltas_scrubbed_one_by_one_leak_the_number() {
        let s = Secrets::new();
        s.register("number", TEST_PAN);
        let deltas = ["Card: 4242 42", "42 4242 4", "242."];
        let naive: String = deltas.iter().map(|d| whole_prose(&s, d)).collect();
        assert_eq!(naive, "Card: 4242 4242 4242 4242.");
        assert_eq!(streamed(&s, &deltas), format!("Card: {NUMBER}."));
    }
}
