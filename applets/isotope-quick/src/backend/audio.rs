//! Sound volume: the default output (sink) of PipeWire, through
//! cosmic-settings-daemon's audio service (cosmic-settings-audio-client over
//! varlink), the way the stock audio applet does it: the same events and
//! `set_sink_volume`. Volumes are percentages; 100 is shown full.

use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
};

use cosmic::iced::{Subscription, stream};
use cosmic_settings_audio_client::{self as client, CosmicAudioProxy};
use futures::{SinkExt, StreamExt, channel::mpsc::Sender};
use tokio::sync::mpsc::{UnboundedReceiver, UnboundedSender, unbounded_channel};

use crate::state::Msg;

/// The default output, from the daemon's event stream.
#[derive(Debug, Default)]
pub struct Sinks {
    default: Option<u32>,
    sinks: HashMap<u32, (u32, bool)>,
}

impl Sinks {
    /// Applies one event; returns what the popup shows when it changed it:
    /// the default output's volume (0 to 1, `None` without one) and mute.
    pub fn apply(&mut self, event: client::Event) -> bool {
        let before = self.shown();
        match event {
            client::Event::DefaultSink(id) => self.default = Some(id),
            client::Event::Node(id, node) if node.is_sink => {
                self.sinks.entry(id).or_insert((0, false));
            }
            client::Event::NodeVolume(id, volume, _) => {
                if let Some(s) = self.sinks.get_mut(&id) {
                    s.0 = volume;
                }
            }
            client::Event::NodeMute(id, mute) => {
                if let Some(s) = self.sinks.get_mut(&id) {
                    s.1 = mute;
                }
            }
            client::Event::RemoveNode(id) => {
                self.sinks.remove(&id);
            }
            _ => {}
        }
        before != self.shown()
    }

    pub fn shown(&self) -> (Option<f32>, bool) {
        match self.default.and_then(|id| self.sinks.get(&id)) {
            Some(&(volume, mute)) => (Some(volume as f32 / 100.0), mute),
            None => (None, false),
        }
    }

    pub fn default_sink(&self) -> Option<u32> {
        self.default.filter(|id| self.sinks.contains_key(id))
    }
}

#[derive(Debug, Clone)]
pub enum Request {
    Volume(f32),
    Mute(bool),
}

#[derive(Debug, Clone)]
pub enum Event {
    /// Where to send the person's changes.
    Sender(UnboundedSender<Request>),
    Changed(Msg),
}

/// A volume as the daemon's percentage.
pub fn percent(volume: f32) -> u32 {
    (volume.clamp(0.0, 1.0) * 100.0).round() as u32
}

/// Carries out the requests on a connection of its own. A slider sends a
/// burst of volumes; only the newest of a burst is sent.
async fn serve(mut rx: UnboundedReceiver<Request>, sink: Arc<Mutex<Option<u32>>>) {
    let mut conn = None;
    while let Some(mut req) = rx.recv().await {
        if matches!(req, Request::Volume(_)) {
            while let Ok(next) = rx.try_recv() {
                if matches!(next, Request::Volume(_)) {
                    req = next;
                } else {
                    carry(&mut conn, req, &sink).await;
                    req = next;
                    break;
                }
            }
        }
        carry(&mut conn, req, &sink).await;
    }
}

async fn carry(conn: &mut Option<client::Client>, req: Request, sink: &Mutex<Option<u32>>) {
    let sink = *sink.lock().unwrap();
    // A second try after the daemon restarted under an open connection.
    for _ in 0..2 {
        let c = match conn {
            Some(c) => c,
            None => match client::connect().await {
                Ok(c) => conn.insert(c),
                Err(err) => {
                    tracing::error!("cosmic-settings-daemon audio: {err}");
                    return;
                }
            },
        };
        let sent = match req {
            Request::Volume(v) => c.conn.set_sink_volume(percent(v)).await.map(drop),
            Request::Mute(mute) => match sink {
                Some(sink) => c.conn.set_node_mute(sink, mute).await.map(drop),
                None => return,
            },
        };
        match sent {
            Ok(()) => return,
            Err(err) => {
                tracing::warn!("cosmic-settings-daemon audio: {err}");
                *conn = None;
            }
        }
    }
}

pub fn subscription() -> Subscription<Event> {
    Subscription::run(|| {
        stream::channel(8, |mut tx: Sender<Event>| async move {
            let default_sink = Arc::new(Mutex::new(None));
            let (req_tx, req_rx) = unbounded_channel();
            tokio::spawn(serve(req_rx, default_sink.clone()));
            let _ = tx.send(Event::Sender(req_tx)).await;
            loop {
                let mut c = match client::connect().await {
                    Ok(c) => c,
                    Err(err) => {
                        tracing::warn!("cosmic-settings-daemon audio: {err}");
                        tokio::time::sleep(super::RETRY).await;
                        continue;
                    }
                };
                if let Ok(Ok(mut events)) = c.recv_events().await {
                    let mut sinks = Sinks::default();
                    while let Some(event) = events.next().await {
                        // Leave the stream, so the outer loop reconnects after RETRY.
                        let Ok(event) = event else { break };
                        if sinks.apply(event) {
                            *default_sink.lock().unwrap() = sinks.default_sink();
                            let (volume, mute) = sinks.shown();
                            let _ = tx.send(Event::Changed(Msg::VolumeChanged(volume, mute))).await;
                        }
                    }
                }
                // The daemon went away: no output until it is back.
                *default_sink.lock().unwrap() = None;
                let _ = tx.send(Event::Changed(Msg::VolumeChanged(None, false))).await;
                tokio::time::sleep(super::RETRY).await;
            }
        })
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn follows_the_default_sink() {
        let mut s = Sinks::default();
        assert_eq!(s.shown(), (None, false));
        assert!(!s.apply(client::Event::NodeVolume(7, 40, None)));
        s.sinks.insert(7, (0, false));
        s.sinks.insert(9, (0, false));
        s.apply(client::Event::NodeVolume(7, 40, None));
        s.apply(client::Event::NodeVolume(9, 150, None));
        assert!(s.apply(client::Event::DefaultSink(7)));
        assert_eq!(s.shown(), (Some(0.4), false));
        assert!(s.apply(client::Event::NodeMute(7, true)));
        assert_eq!(s.shown(), (Some(0.4), true));
        // Another output's change is not shown.
        assert!(!s.apply(client::Event::NodeMute(9, true)));
        assert!(s.apply(client::Event::DefaultSink(9)));
        assert_eq!(s.shown(), (Some(1.5), true));
        assert_eq!(s.default_sink(), Some(9));
        assert!(s.apply(client::Event::RemoveNode(9)));
        assert_eq!(s.shown(), (None, false));
        assert_eq!(s.default_sink(), None);
    }

    #[test]
    fn volume_is_a_percentage() {
        assert_eq!(percent(0.5), 50);
        assert_eq!(percent(1.3), 100);
        assert_eq!(percent(-1.0), 0);
    }
}
