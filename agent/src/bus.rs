//! In-memory event bus with monotonically increasing offsets and a bounded replay ring.

use std::collections::VecDeque;
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use prost::Message;
use tokio::sync::broadcast;

use crate::pb::{Event, event};

const RING_EVENTS: usize = 4096;
const RING_BYTES: usize = 32 << 20;
const LIVE_CAPACITY: usize = 1024;

/// Fan-out of agent events; offsets start at the boot time in microseconds so
/// they keep growing across restarts and stale offsets fall out of range.
pub struct Bus {
    ring: Mutex<Ring>,
    live: broadcast::Sender<Arc<Event>>,
}

/// Replayed events plus the live receiver that continues after them.
pub struct Subscription {
    pub replay: Vec<Arc<Event>>,
    pub live: broadcast::Receiver<Arc<Event>>,
}

struct Ring {
    next: u64,
    events: VecDeque<Arc<Event>>,
    bytes: usize,
}

impl Bus {
    /// Creates an empty bus.
    pub fn new() -> Self {
        let base = SystemTime::now()
            .duration_since(UNIX_EPOCH)
            .map_or(1, |d| u64::try_from(d.as_micros()).unwrap_or(1));
        Self::with_base(base)
    }

    /// Creates an empty bus whose first offset is `base`.
    pub fn with_base(base: u64) -> Self {
        Self {
            ring: Mutex::new(Ring {
                next: base.max(1),
                events: VecDeque::new(),
                bytes: 0,
            }),
            live: broadcast::channel(LIVE_CAPACITY).0,
        }
    }

    /// Assigns the next offset to `body` and delivers it to every subscriber.
    pub fn publish(&self, body: event::Body) -> u64 {
        let Ok(mut ring) = self.ring.lock() else {
            return 0;
        };
        let offset = ring.next;
        ring.next += 1;
        let event = Arc::new(Event {
            offset,
            body: Some(body),
        });
        ring.bytes += event.encoded_len();
        ring.events.push_back(Arc::clone(&event));
        while ring.events.len() > RING_EVENTS || ring.bytes > RING_BYTES {
            let Some(old) = ring.events.pop_front() else {
                break;
            };
            ring.bytes -= old.encoded_len();
        }
        // sent under the ring lock so live order equals offset order
        let _ = self.live.send(event);
        offset
    }

    /// Subscribes after `from_offset`; 0 means live events only.
    pub fn subscribe(&self, from_offset: u64) -> Result<Subscription, String> {
        let ring = self
            .ring
            .lock()
            .map_err(|_| "event ring poisoned".to_owned())?;
        let live = self.live.subscribe();
        if from_offset == 0 {
            return Ok(Subscription {
                replay: Vec::new(),
                live,
            });
        }
        let first = ring.events.front().map_or(ring.next, |e| e.offset);
        if from_offset + 1 < first || from_offset >= ring.next {
            return Err(format!(
                "offset {from_offset} is outside the replay window [{}, {}); resync with Transcript",
                first.saturating_sub(1),
                ring.next
            ));
        }
        let replay = ring
            .events
            .iter()
            .filter(|e| e.offset > from_offset)
            .cloned()
            .collect();
        Ok(Subscription { replay, live })
    }
}

impl Default for Bus {
    fn default() -> Self {
        Self::new()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::pb::ComputerAction;

    fn action(n: i32) -> event::Body {
        event::Body::ComputerAction(ComputerAction {
            x: n,
            ..Default::default()
        })
    }

    #[tokio::test]
    async fn replay_then_live_without_gaps() {
        let bus = Bus::with_base(100);
        let offsets: Vec<u64> = (0..3).map(|i| bus.publish(action(i))).collect();
        assert_eq!(offsets, [100, 101, 102]);
        let mut sub = bus.subscribe(100).unwrap();
        let replayed: Vec<u64> = sub.replay.iter().map(|e| e.offset).collect();
        assert_eq!(replayed, [101, 102]);
        bus.publish(action(3));
        assert_eq!(sub.live.recv().await.unwrap().offset, 103);
    }

    #[test]
    fn zero_offset_is_live_only_and_latest_offset_replays_nothing() {
        let bus = Bus::with_base(10);
        bus.publish(action(0));
        assert!(bus.subscribe(0).unwrap().replay.is_empty());
        assert!(bus.subscribe(10).unwrap().replay.is_empty());
    }

    #[test]
    fn evicted_or_foreign_offsets_are_rejected() {
        let bus = Bus::with_base(1000);
        for i in 0..(RING_EVENTS as i32 + 10) {
            bus.publish(action(i));
        }
        assert!(bus.subscribe(1000).is_err());
        assert!(bus.subscribe(5).is_err());
        assert!(bus.subscribe(u64::MAX - 1).is_err());
        assert!(bus.subscribe(1009).is_ok());
    }
}
