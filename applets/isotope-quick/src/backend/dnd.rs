//! Do not disturb: `do_not_disturb` of com.system76.CosmicNotifications
//! (cosmic-notifications-config), the key the stock notifications applet's
//! toggle writes and cosmic-notifications reads.

use cosmic::cosmic_config::{Config, CosmicConfigEntry};
pub use cosmic_notifications_config::{ID, NotificationsConfig};

pub fn set_dnd(on: bool) {
    let res = Config::new(ID, NotificationsConfig::VERSION).and_then(|c| {
        let mut config = NotificationsConfig::get_entry(&c).unwrap_or_else(|(_, c)| c);
        config.set_do_not_disturb(&c, on).map(|_| ())
    });
    if let Err(err) = res {
        tracing::error!("cannot set do not disturb: {err}");
    }
}
