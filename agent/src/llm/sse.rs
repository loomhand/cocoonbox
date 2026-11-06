//! Incremental server-sent-events decoder.

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

    /// Returns the data of the next complete event, if one is buffered.
    pub fn next_event(&mut self) -> Option<String> {
        loop {
            let end = self.buf.windows(2).position(|w| w == b"\n\n")?;
            let data = parse(&String::from_utf8_lossy(&self.buf[..end]));
            self.buf.drain(..end + 2);
            if !data.is_empty() {
                return Some(data);
            }
        }
    }

    /// Returns the data of a trailing event that lacked the final blank line.
    pub fn finish(&mut self) -> Option<String> {
        let raw = std::mem::take(&mut self.buf);
        let data = parse(&String::from_utf8_lossy(&raw));
        (!data.is_empty()).then_some(data)
    }
}

fn parse(block: &str) -> String {
    let mut data = String::new();
    for line in block.lines() {
        let (field, value) = line.split_once(':').unwrap_or((line, ""));
        if field == "data" {
            if !data.is_empty() {
                data.push('\n');
            }
            data.push_str(value.strip_prefix(' ').unwrap_or(value));
        }
    }
    data
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
        assert_eq!(d.next_event(), Some("{\"a\":1}".into()));
        assert_eq!(d.next_event(), Some("x\ny".into()));
        assert_eq!(d.next_event(), None);
        d.push(b"data: [DONE]");
        assert_eq!(d.finish(), Some("[DONE]".into()));
    }
}
