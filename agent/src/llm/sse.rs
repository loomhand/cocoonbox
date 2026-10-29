//! Incremental server-sent-events decoder.

/// One decoded SSE event.
#[derive(Debug, Default, PartialEq, Eq)]
pub struct Event {
    pub event: String,
    pub data: String,
}

/// Buffers raw bytes and yields complete events.
#[derive(Default)]
pub struct Decoder {
    buf: Vec<u8>,
}

impl Decoder {
    /// Appends a chunk; carriage returns are dropped so CRLF streams parse as LF.
    pub fn push(&mut self, chunk: &[u8]) {
        self.buf
            .extend(chunk.iter().copied().filter(|&b| b != b'\r'));
    }

    /// Returns the next complete event, if one is buffered.
    pub fn next_event(&mut self) -> Option<Event> {
        loop {
            let end = self.buf.windows(2).position(|w| w == b"\n\n")?;
            let event = parse(&String::from_utf8_lossy(&self.buf[..end]));
            self.buf.drain(..end + 2);
            if !event.data.is_empty() || !event.event.is_empty() {
                return Some(event);
            }
        }
    }

    /// Returns a trailing event that lacked the final blank line.
    pub fn finish(&mut self) -> Option<Event> {
        let raw = std::mem::take(&mut self.buf);
        let event = parse(&String::from_utf8_lossy(&raw));
        (!event.data.is_empty()).then_some(event)
    }
}

fn parse(block: &str) -> Event {
    let mut event = Event::default();
    for line in block.lines() {
        let (field, value) = line.split_once(':').unwrap_or((line, ""));
        let value = value.strip_prefix(' ').unwrap_or(value);
        match field {
            "event" => value.clone_into(&mut event.event),
            "data" => {
                if !event.data.is_empty() {
                    event.data.push('\n');
                }
                event.data.push_str(value);
            }
            _ => {}
        }
    }
    event
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn splits_events_across_chunks_and_crlf() {
        let mut d = Decoder::default();
        d.push(b"event: message_start\r\ndata: {\"a\"");
        assert_eq!(d.next_event(), None);
        d.push(b":1}\r\n\r\n: comment\n\ndata: x\ndata: y\n\n");
        assert_eq!(
            d.next_event(),
            Some(Event {
                event: "message_start".into(),
                data: "{\"a\":1}".into()
            })
        );
        assert_eq!(
            d.next_event(),
            Some(Event {
                event: String::new(),
                data: "x\ny".into()
            })
        );
        assert_eq!(d.next_event(), None);
        d.push(b"data: [DONE]");
        assert_eq!(d.finish().map(|e| e.data), Some("[DONE]".into()));
    }
}
