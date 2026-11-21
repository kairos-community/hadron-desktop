//! Bluetooth on or off, and the connected device: BlueZ's D-Bus API on the
//! system bus (the adapters' `Powered`, the devices' `Connected` and
//! `Alias`), what bluer, which the stock Bluetooth applet uses, reads.

use std::collections::HashMap;

use cosmic::iced::{Subscription, stream};
use futures::{SinkExt, StreamExt, channel::mpsc::Sender};
use zbus::{
    MatchRule, MessageStream,
    fdo::{ManagedObjects, ObjectManagerProxy},
    names::InterfaceName,
    zvariant::{OwnedObjectPath, OwnedValue},
};

use crate::state::Msg;

const BLUEZ: &str = "org.bluez";
const ADAPTER: &str = "org.bluez.Adapter1";
const DEVICE: &str = "org.bluez.Device1";

/// What one adapter or device says.
#[derive(Debug, Clone, PartialEq)]
pub enum Object {
    Adapter { powered: bool },
    Device { connected: bool, alias: String },
}

/// Bluetooth is on when an adapter is powered; it shows the first connected
/// device, in object path order so it does not jump between two.
pub fn summarize(objects: &[(String, Object)]) -> (bool, Option<String>) {
    let mut objects: Vec<_> = objects.iter().collect();
    objects.sort_by(|a, b| a.0.cmp(&b.0));
    let on = objects
        .iter()
        .any(|(_, o)| matches!(o, Object::Adapter { powered: true }));
    let device = objects.iter().find_map(|(_, o)| match o {
        Object::Device { connected: true, alias } => Some(alias.clone()),
        _ => None,
    });
    (on, device.filter(|_| on))
}

fn get<T: TryFrom<OwnedValue>>(props: &HashMap<String, OwnedValue>, key: &str) -> Option<T> {
    props.get(key).and_then(|v| v.try_clone().ok()).and_then(|v| T::try_from(v).ok())
}

fn objects(managed: ManagedObjects) -> Vec<(String, Object)> {
    let mut out = Vec::new();
    for (path, ifaces) in managed {
        for (iface, props) in ifaces {
            let props: HashMap<String, OwnedValue> = props.into_iter().collect();
            let object = match iface.as_str() {
                ADAPTER => Object::Adapter { powered: get(&props, "Powered").unwrap_or(false) },
                DEVICE => Object::Device {
                    connected: get(&props, "Connected").unwrap_or(false),
                    alias: get(&props, "Alias").unwrap_or_default(),
                },
                _ => continue,
            };
            out.push((path.to_string(), object));
        }
    }
    out
}

async fn adapters(conn: &zbus::Connection) -> zbus::Result<Vec<OwnedObjectPath>> {
    let om = ObjectManagerProxy::builder(conn).destination(BLUEZ)?.path("/")?.build().await?;
    Ok(om
        .get_managed_objects()
        .await?
        .into_iter()
        .filter(|(_, ifaces)| ifaces.keys().any(|i| i.as_str() == ADAPTER))
        .map(|(path, _)| path)
        .collect())
}

async fn watch(tx: &mut Sender<Msg>) -> zbus::Result<()> {
    let conn = zbus::Connection::system().await?;
    let om = ObjectManagerProxy::builder(&conn).destination(BLUEZ)?.path("/")?.build().await?;
    // Any property of any BlueZ object, and objects coming and going.
    let props = MatchRule::builder()
        .msg_type(zbus::message::Type::Signal)
        .sender(BLUEZ)?
        .interface("org.freedesktop.DBus.Properties")?
        .member("PropertiesChanged")?
        .build();
    let mut changes = futures::stream_select!(
        MessageStream::for_match_rule(props, &conn, Some(16)).await?.map(|_| ()),
        om.receive_interfaces_added().await?.map(|_| ()),
        om.receive_interfaces_removed().await?.map(|_| ()),
    );
    let mut last = None;
    loop {
        let now = summarize(&objects(om.get_managed_objects().await?));
        if last.as_ref() != Some(&now) {
            let _ = tx.send(Msg::BtChanged(now.0, now.1.clone())).await;
            last = Some(now);
        }
        if changes.next().await.is_none() {
            return Ok(());
        }
    }
}

pub fn subscription() -> Subscription<Msg> {
    Subscription::run(|| {
        stream::channel(4, |mut tx: Sender<Msg>| async move {
            loop {
                if let Err(err) = watch(&mut tx).await {
                    tracing::warn!("BlueZ: {err}");
                }
                tokio::time::sleep(super::RETRY).await;
            }
        })
    })
}

/// Powers every adapter on or off. When BlueZ refuses (rfkill), what to
/// show instead: the state read back, or, when that fails too, the toggle
/// undone. The watcher does not report it, since for it nothing changed.
pub async fn set_bt(on: bool) -> Option<Msg> {
    let conn = zbus::Connection::system().await;
    let res = async {
        let conn = conn.as_ref().map_err(Clone::clone)?;
        for path in adapters(conn).await? {
            let props = zbus::fdo::PropertiesProxy::builder(conn)
                .destination(BLUEZ)?
                .path(path)?
                .build()
                .await?;
            props
                .set(InterfaceName::from_static_str_unchecked(ADAPTER), "Powered", on.into())
                .await?;
        }
        zbus::Result::Ok(())
    };
    let err = res.await.err()?;
    tracing::error!("cannot turn Bluetooth {}: {err}", if on { "on" } else { "off" });
    let read = async {
        let conn = conn.as_ref().map_err(Clone::clone)?;
        let om = ObjectManagerProxy::builder(conn).destination(BLUEZ)?.path("/")?.build().await?;
        zbus::Result::Ok(summarize(&objects(om.get_managed_objects().await?)))
    };
    Some(match read.await {
        Ok((powered, device)) => Msg::BtChanged(powered, device),
        Err(_) => Msg::BtSetFailed(on),
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    fn dev(path: &str, connected: bool, alias: &str) -> (String, Object) {
        (path.into(), Object::Device { connected, alias: alias.into() })
    }

    #[test]
    fn a_powered_adapter_is_on_and_shows_the_connected_device() {
        let adapter = |powered| ("/org/bluez/hci0".to_string(), Object::Adapter { powered });
        assert_eq!(summarize(&[]), (false, None));
        assert_eq!(summarize(&[adapter(false)]), (false, None));
        assert_eq!(
            summarize(&[
                dev("/org/bluez/hci0/dev_B", true, "Speaker"),
                adapter(true),
                dev("/org/bluez/hci0/dev_A", true, "Headphones"),
                dev("/org/bluez/hci0/dev_0", false, "Mouse"),
            ]),
            (true, Some("Headphones".into()))
        );
        assert_eq!(summarize(&[adapter(true), dev("/org/bluez/hci0/dev_0", false, "Mouse")]), (true, None));
    }
}
