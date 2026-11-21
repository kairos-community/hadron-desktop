//! Wi-Fi on or off, the connected network, and whether a wired connection
//! is up: NetworkManager's D-Bus API on the system bus (what nmrs, which
//! the stock network applet uses, calls underneath).

use cosmic::iced::{Subscription, stream};
use futures::{SinkExt, StreamExt, channel::mpsc::Sender};
use zbus::{MatchRule, MessageStream, zvariant::OwnedObjectPath};

use crate::state::Msg;

#[zbus::proxy(
    interface = "org.freedesktop.NetworkManager",
    default_service = "org.freedesktop.NetworkManager",
    default_path = "/org/freedesktop/NetworkManager"
)]
trait NetworkManager {
    #[zbus(property)]
    fn wireless_enabled(&self) -> zbus::Result<bool>;
    #[zbus(property)]
    fn set_wireless_enabled(&self, value: bool) -> zbus::Result<()>;
    #[zbus(property)]
    fn active_connections(&self) -> zbus::Result<Vec<OwnedObjectPath>>;
    /// NetworkManager's overall state; it changes as a connection comes up
    /// or goes down, which the list of active connections alone does not show.
    #[zbus(property)]
    fn state(&self) -> zbus::Result<u32>;
}

#[zbus::proxy(
    interface = "org.freedesktop.NetworkManager.Connection.Active",
    default_service = "org.freedesktop.NetworkManager"
)]
trait ActiveConnection {
    #[zbus(property)]
    fn id(&self) -> zbus::Result<String>;
    #[zbus(property, name = "Type")]
    fn kind(&self) -> zbus::Result<String>;
    #[zbus(property)]
    fn state(&self) -> zbus::Result<u32>;
}

const NM: &str = "org.freedesktop.NetworkManager";
const ACTIVE: &str = "org.freedesktop.NetworkManager.Connection.Active";

/// NM_ACTIVE_CONNECTION_STATE_ACTIVATED.
const ACTIVATED: u32 = 2;

/// One active connection: its type, state and name.
#[derive(Debug, Clone, PartialEq)]
pub struct Active {
    pub kind: String,
    pub state: u32,
    pub id: String,
}

/// The connected Wi-Fi network's name, and whether a wired connection is up.
pub fn summarize(active: &[Active]) -> (Option<String>, bool) {
    let up = |kind: &str| {
        active
            .iter()
            .find(|a| a.kind == kind && a.state == ACTIVATED)
    };
    (
        up("802-11-wireless").map(|a| a.id.clone()),
        up("802-3-ethernet").is_some(),
    )
}

async fn read(conn: &zbus::Connection, nm: &NetworkManagerProxy<'_>) -> zbus::Result<(bool, Vec<Active>)> {
    let enabled = nm.wireless_enabled().await?;
    let mut active = Vec::new();
    for path in nm.active_connections().await? {
        let Ok(a) = ActiveConnectionProxy::builder(conn).path(path)?.build().await else {
            continue;
        };
        // A connection can go away between the list and the read.
        if let (Ok(kind), Ok(state), Ok(id)) = (a.kind().await, a.state().await, a.id().await) {
            active.push(Active { kind, state, id });
        }
    }
    Ok((enabled, active))
}

async fn watch(tx: &mut Sender<Msg>) -> zbus::Result<()> {
    let conn = zbus::Connection::system().await?;
    let nm = NetworkManagerProxy::new(&conn).await?;
    // An active connection's own changes too: a Wi-Fi network that comes up
    // while a wired one is already up changes neither the list nor the
    // global state, only that connection's State.
    let active = MatchRule::builder()
        .msg_type(zbus::message::Type::Signal)
        .sender(NM)?
        .interface("org.freedesktop.DBus.Properties")?
        .member("PropertiesChanged")?
        .arg(0, ACTIVE)?
        .build();
    let mut changes = futures::stream_select!(
        nm.receive_wireless_enabled_changed().await.map(|_| ()),
        nm.receive_active_connections_changed().await.map(|_| ()),
        nm.receive_state_changed().await.map(|_| ()),
        MessageStream::for_match_rule(active, &conn, Some(16)).await?.map(|_| ()),
    );
    let mut last = None;
    loop {
        let now = current(&conn, &nm).await?;
        if last.as_ref() != Some(&now) {
            let _ = tx.send(Msg::WifiChanged(now.0, now.1.clone())).await;
            let _ = tx.send(Msg::WiredChanged(now.2)).await;
            last = Some(now);
        }
        if changes.next().await.is_none() {
            return Ok(());
        }
    }
}

/// Wi-Fi on, the network's name, a wired connection up.
async fn current(
    conn: &zbus::Connection,
    nm: &NetworkManagerProxy<'_>,
) -> zbus::Result<(bool, Option<String>, bool)> {
    let (enabled, active) = read(conn, nm).await?;
    let (name, wired) = summarize(&active);
    Ok((enabled, name, wired))
}

pub fn subscription() -> Subscription<Msg> {
    Subscription::run(|| {
        stream::channel(4, |mut tx: Sender<Msg>| async move {
            loop {
                if let Err(err) = watch(&mut tx).await {
                    tracing::warn!("NetworkManager: {err}");
                }
                tokio::time::sleep(super::RETRY).await;
            }
        })
    })
}

/// Turns Wi-Fi on or off (polkit lets the active local session do it).
/// When NetworkManager refuses (rfkill, polkit), what to show instead: the
/// state read back, or, when that fails too, the toggle undone. The watcher
/// does not report it, since for it nothing changed.
pub async fn set_wifi(on: bool) -> Option<Msg> {
    let conn = async {
        let conn = zbus::Connection::system().await?;
        let nm = NetworkManagerProxy::new(&conn).await?;
        zbus::Result::Ok((conn, nm))
    }
    .await;
    let err = match &conn {
        Ok((_, nm)) => nm.set_wireless_enabled(on).await.err()?,
        Err(err) => err.clone(),
    };
    tracing::error!("cannot turn Wi-Fi {}: {err}", if on { "on" } else { "off" });
    let read = match &conn {
        Ok((conn, nm)) => current(conn, nm).await.ok(),
        Err(_) => None,
    };
    Some(match read {
        Some((enabled, name, _)) => Msg::WifiChanged(enabled, name),
        None => Msg::WifiSetFailed(on),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn a(kind: &str, state: u32, id: &str) -> Active {
        Active { kind: kind.into(), state, id: id.into() }
    }

    #[test]
    fn the_activated_wifi_names_the_network() {
        assert_eq!(summarize(&[]), (None, false));
        assert_eq!(
            summarize(&[a("802-3-ethernet", 2, "Wired"), a("802-11-wireless", 2, "home-5G")]),
            (Some("home-5G".into()), true)
        );
        // Still connecting: no name yet.
        assert_eq!(summarize(&[a("802-11-wireless", 1, "home-5G")]), (None, false));
        // A VPN or a bridge is neither.
        assert_eq!(summarize(&[a("vpn", 2, "work"), a("bridge", 2, "br0")]), (None, false));
    }
}
