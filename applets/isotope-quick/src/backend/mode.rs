//! Dark style: `is_dark` of com.system76.CosmicTheme.Mode (libcosmic's
//! `ThemeMode`), the key COSMIC Settings' Appearance page writes
//! (`ThemeManager::dark_mode`: `set_is_dark`, nothing else). Every libcosmic
//! app and cosmic-comp follow it.

use cosmic::cosmic_config::CosmicConfigEntry;
pub use cosmic::cosmic_theme::{THEME_MODE_ID as ID, ThemeMode};

pub fn set_dark(dark: bool) {
    let res = ThemeMode::config().and_then(|c| {
        let mut mode = ThemeMode::get_entry(&c).unwrap_or_else(|(_, m)| m);
        mode.set_is_dark(&c, dark).map(|_| ())
    });
    if let Err(err) = res {
        tracing::error!("cannot set the dark style: {err}");
    }
}
