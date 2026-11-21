//! What the quick settings show, and what a click asks the system to do.
//!
//! A pure reducer: `update` changes the [`State`] and returns the
//! [`Effect`]s the backends carry out. Two kinds of [`Msg`]:
//!
//! - reports from the backends (`*Changed`): the system's state changed, so
//!   the popup follows; they never return an effect, so a backend's own
//!   change cannot echo back to it;
//! - the person's actions (`*Toggled`, `Brightness`, `Volume`,
//!   `OpenDetail`): the popup changes at once and returns what to do;
//! - `*SetFailed`: a backend could not carry out a toggle and could not read
//!   the system back either, so the toggle goes back to what it was.
//!
//! A report of the brightness or the volume that arrives while the person
//! is moving that slider ([`HOLD`] after their last move) carries a value
//! from before the move and would make the handle jump back: it is dropped.

use std::time::{Duration, Instant};

/// How long a slider ignores the system's reports after the person moved it.
pub const HOLD: Duration = Duration::from_millis(150);

/// A setting that is on or off, with what it shows when on (the network
/// name, the connected device).
#[derive(Debug, Clone, Default, PartialEq)]
pub struct Toggle<T> {
    pub on: bool,
    pub value: T,
}

#[derive(Debug, Clone, Default, PartialEq)]
pub struct State {
    /// Wi-Fi on or off, and the connected network's name.
    pub wifi: Toggle<Option<String>>,
    /// Bluetooth on or off, and the connected device's name.
    pub bt: Toggle<Option<String>>,
    /// A wired connection is up (the panel icon only).
    pub wired: bool,
    /// The keyboard layout's name, e.g. "English (US)".
    pub layout: String,
    pub dark: bool,
    pub dnd: bool,
    /// The display brightness, 0 to 1; `None` when there is no backlight,
    /// which hides the slider.
    pub brightness: Option<f32>,
    /// The default output's volume, 0 to 1; `None` when there is none.
    pub volume: Option<f32>,
    pub muted: bool,
    /// When the person last moved the brightness or the volume slider.
    pub brightness_moved: Option<Instant>,
    pub volume_moved: Option<Instant>,
}

/// What a row, or a button of the bottom row, opens.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Detail {
    Wifi,
    Bluetooth,
    Keyboard,
    Settings,
    LogOut,
    Suspend,
    Restart,
    ShutDown,
}

impl Detail {
    /// The command that opens it: COSMIC Settings' page for a row, what the
    /// stock power applet (and isotope-menu) runs for a power button.
    pub fn command(self) -> Vec<String> {
        let argv: &[&str] = match self {
            Detail::Wifi => &["cosmic-settings", "wireless"],
            Detail::Bluetooth => &["cosmic-settings", "bluetooth"],
            Detail::Keyboard => &["cosmic-settings", "keyboard"],
            Detail::Settings => &["cosmic-settings"],
            // cosmic-osd asks for confirmation with a countdown.
            Detail::LogOut => &["cosmic-osd", "log-out"],
            Detail::Restart => &["cosmic-osd", "restart"],
            Detail::ShutDown => &["cosmic-osd", "shutdown"],
            // The stock applet calls logind's Suspend; this is the same call.
            Detail::Suspend => &["systemctl", "suspend"],
        };
        argv.iter().map(|s| s.to_string()).collect()
    }
}

#[derive(Debug, Clone, PartialEq)]
pub enum Msg {
    // Reports from the backends.
    WifiChanged(bool, Option<String>),
    BtChanged(bool, Option<String>),
    WiredChanged(bool),
    LayoutChanged(String),
    DarkChanged(bool),
    DndChanged(bool),
    BrightnessChanged(Option<f32>),
    /// The default output's volume (`None`: no output) and mute.
    VolumeChanged(Option<f32>, bool),
    // The person's actions.
    WifiToggled,
    BtToggled,
    /// Setting Wi-Fi to this failed, and so did reading it back.
    WifiSetFailed(bool),
    /// Setting Bluetooth to this failed, and so did reading it back.
    BtSetFailed(bool),
    DarkToggled,
    DndToggled,
    Brightness(f32),
    Volume(f32),
    MuteToggled,
    OpenDetail(Detail),
}

#[derive(Debug, Clone, PartialEq)]
pub enum Effect {
    SetWifi(bool),
    SetBt(bool),
    SetDark(bool),
    SetDnd(bool),
    SetBrightness(f32),
    SetVolume(f32),
    SetMute(bool),
    Spawn(Vec<String>),
}

/// A toggle as reported: the value is shown only while it is on.
fn toggle(on: bool, value: Option<String>) -> Toggle<Option<String>> {
    Toggle { on, value: value.filter(|_| on) }
}

fn unit(v: f32) -> f32 {
    if v.is_nan() { 0.0 } else { v.clamp(0.0, 1.0) }
}

pub fn update(s: &mut State, m: Msg) -> Vec<Effect> {
    update_at(s, m, Instant::now())
}

fn held(moved: Option<Instant>, now: Instant) -> bool {
    moved.is_some_and(|t| now.saturating_duration_since(t) < HOLD)
}

/// [`update`] at a given time, which the sliders' hold depends on.
pub fn update_at(s: &mut State, m: Msg, now: Instant) -> Vec<Effect> {
    match m {
        Msg::WifiChanged(on, name) => s.wifi = toggle(on, name),
        Msg::BtChanged(on, name) => s.bt = toggle(on, name),
        Msg::WiredChanged(up) => s.wired = up,
        Msg::LayoutChanged(layout) => s.layout = layout,
        Msg::DarkChanged(dark) => s.dark = dark,
        Msg::DndChanged(dnd) => s.dnd = dnd,
        // A slider that goes away (no backlight, no output) is never held.
        Msg::BrightnessChanged(b) => {
            if b.is_none() || !held(s.brightness_moved, now) {
                s.brightness = b.map(unit);
            }
        }
        Msg::VolumeChanged(v, muted) => {
            if v.is_none() || !held(s.volume_moved, now) {
                s.volume = v.map(unit);
                s.muted = muted;
            }
        }
        Msg::WifiSetFailed(tried) => {
            if s.wifi.on == tried {
                s.wifi = toggle(!tried, None);
            }
        }
        Msg::BtSetFailed(tried) => {
            if s.bt.on == tried {
                s.bt = toggle(!tried, None);
            }
        }
        Msg::WifiToggled => {
            s.wifi = toggle(!s.wifi.on, None);
            return vec![Effect::SetWifi(s.wifi.on)];
        }
        Msg::BtToggled => {
            s.bt = toggle(!s.bt.on, None);
            return vec![Effect::SetBt(s.bt.on)];
        }
        Msg::DarkToggled => {
            s.dark = !s.dark;
            return vec![Effect::SetDark(s.dark)];
        }
        Msg::DndToggled => {
            s.dnd = !s.dnd;
            return vec![Effect::SetDnd(s.dnd)];
        }
        Msg::Brightness(b) => {
            if s.brightness.is_some() {
                let b = unit(b);
                s.brightness = Some(b);
                s.brightness_moved = Some(now);
                return vec![Effect::SetBrightness(b)];
            }
        }
        Msg::Volume(v) => {
            if s.volume.is_some() {
                let v = unit(v);
                s.volume = Some(v);
                s.volume_moved = Some(now);
                s.muted = false;
                return vec![Effect::SetVolume(v)];
            }
        }
        Msg::MuteToggled => {
            if s.volume.is_some() {
                s.muted = !s.muted;
                return vec![Effect::SetMute(s.muted)];
            }
        }
        Msg::OpenDetail(d) => return vec![Effect::Spawn(d.command())],
    }
    Vec::new()
}

/// The panel button's network icon, chosen as the stock network applet
/// chooses it: a wired connection first, then Wi-Fi (no signal strength
/// here), else disconnected.
pub fn net_icon(s: &State) -> &'static str {
    if s.wired {
        "network-wired-symbolic"
    } else if s.wifi.value.is_some() {
        "network-wireless-symbolic"
    } else if s.wifi.on {
        "network-wireless-disconnected-symbolic"
    } else {
        "network-wired-disconnected-symbolic"
    }
}

/// The panel button's volume icon, with the stock audio applet's steps.
pub fn volume_icon(s: &State) -> &'static str {
    match s.volume {
        None => "audio-volume-muted-symbolic",
        Some(v) if s.muted || v <= 0.0 => "audio-volume-muted-symbolic",
        Some(v) if v < 0.33 => "audio-volume-low-symbolic",
        Some(v) if v < 0.66 => "audio-volume-medium-symbolic",
        Some(_) => "audio-volume-high-symbolic",
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn panel_icons_follow_the_state() {
        let mut s = State::default();
        assert_eq!(net_icon(&s), "network-wired-disconnected-symbolic");
        update(&mut s, Msg::WifiChanged(true, None));
        assert_eq!(net_icon(&s), "network-wireless-disconnected-symbolic");
        update(&mut s, Msg::WifiChanged(true, Some("home-5G".into())));
        assert_eq!(net_icon(&s), "network-wireless-symbolic");
        update(&mut s, Msg::WiredChanged(true));
        assert_eq!(net_icon(&s), "network-wired-symbolic");

        assert_eq!(volume_icon(&s), "audio-volume-muted-symbolic");
        update(&mut s, Msg::VolumeChanged(Some(0.2), false));
        assert_eq!(volume_icon(&s), "audio-volume-low-symbolic");
        update(&mut s, Msg::VolumeChanged(Some(0.5), false));
        assert_eq!(volume_icon(&s), "audio-volume-medium-symbolic");
        update(&mut s, Msg::VolumeChanged(Some(1.0), false));
        assert_eq!(volume_icon(&s), "audio-volume-high-symbolic");
        update(&mut s, Msg::VolumeChanged(Some(1.0), true));
        assert_eq!(volume_icon(&s), "audio-volume-muted-symbolic");
    }

    #[test]
    fn dark_toggle_flips_and_emits_set_dark() {
        let mut s = State::default();
        assert_eq!(update(&mut s, Msg::DarkToggled), vec![Effect::SetDark(true)]);
        assert!(s.dark);
        assert_eq!(update(&mut s, Msg::DarkToggled), vec![Effect::SetDark(false)]);
        assert!(!s.dark);
        // A change made elsewhere (COSMIC Settings) is followed, not echoed.
        assert_eq!(update(&mut s, Msg::DarkChanged(true)), vec![]);
        assert!(s.dark);
    }

    #[test]
    fn dnd_toggle_emits_set_dnd() {
        let mut s = State::default();
        assert_eq!(update(&mut s, Msg::DndToggled), vec![Effect::SetDnd(true)]);
        assert!(s.dnd);
        assert_eq!(update(&mut s, Msg::DndChanged(false)), vec![]);
        assert!(!s.dnd);
    }

    #[test]
    fn volume_clamps_to_0_1() {
        let mut s = State::default();
        update(&mut s, Msg::VolumeChanged(Some(0.4), true));
        assert_eq!(update(&mut s, Msg::Volume(1.7)), vec![Effect::SetVolume(1.0)]);
        assert_eq!(s.volume, Some(1.0));
        // Moving the slider unmutes, as in the stock audio applet.
        assert!(!s.muted);
        assert_eq!(update(&mut s, Msg::Volume(-0.2)), vec![Effect::SetVolume(0.0)]);
        assert_eq!(s.volume, Some(0.0));
        // An over-amplified output is shown full (a report after the hold).
        update_at(&mut s, Msg::VolumeChanged(Some(1.5), false), Instant::now() + HOLD);
        assert_eq!(s.volume, Some(1.0));
        // No output: nothing to set.
        update(&mut s, Msg::VolumeChanged(None, false));
        assert_eq!(update(&mut s, Msg::Volume(0.5)), vec![]);
        assert_eq!(update(&mut s, Msg::MuteToggled), vec![]);
        assert_eq!(s.volume, None);
    }

    #[test]
    fn mute_toggle_emits_set_mute() {
        let mut s = State::default();
        update(&mut s, Msg::VolumeChanged(Some(0.5), false));
        assert_eq!(update(&mut s, Msg::MuteToggled), vec![Effect::SetMute(true)]);
        assert!(s.muted);
    }

    #[test]
    fn wifi_off_hides_network_name() {
        let mut s = State::default();
        update(&mut s, Msg::WifiChanged(true, Some("home-5G".into())));
        assert_eq!(s.wifi, Toggle { on: true, value: Some("home-5G".into()) });
        assert_eq!(update(&mut s, Msg::WifiChanged(false, Some("home-5G".into()))), vec![]);
        assert_eq!(s.wifi, Toggle { on: false, value: None });
        // Turning it on from the popup asks NetworkManager; the name comes
        // with NetworkManager's next report.
        assert_eq!(update(&mut s, Msg::WifiToggled), vec![Effect::SetWifi(true)]);
        assert_eq!(s.wifi, Toggle { on: true, value: None });
        update(&mut s, Msg::WifiChanged(true, Some("home-5G".into())));
        assert_eq!(update(&mut s, Msg::WifiToggled), vec![Effect::SetWifi(false)]);
        assert_eq!(s.wifi, Toggle { on: false, value: None });
    }

    #[test]
    fn a_failed_toggle_goes_back() {
        let mut s = State::default();
        update(&mut s, Msg::WifiChanged(true, Some("home-5G".into())));
        assert_eq!(update(&mut s, Msg::WifiToggled), vec![Effect::SetWifi(false)]);
        assert!(!s.wifi.on);
        // rfkill or polkit said no, and NetworkManager could not be read.
        assert_eq!(update(&mut s, Msg::WifiSetFailed(false)), vec![]);
        assert_eq!(s.wifi, Toggle { on: true, value: None });
        // A failure that a newer report already settled changes nothing.
        update(&mut s, Msg::WifiChanged(false, None));
        update(&mut s, Msg::WifiSetFailed(true));
        assert!(!s.wifi.on);

        assert_eq!(update(&mut s, Msg::BtToggled), vec![Effect::SetBt(true)]);
        update(&mut s, Msg::BtSetFailed(true));
        assert_eq!(s.bt, Toggle { on: false, value: None });
    }

    #[test]
    fn reports_do_not_move_a_slider_being_dragged() {
        let t = Instant::now();
        let mut s = State::default();
        update_at(&mut s, Msg::BrightnessChanged(Some(0.2)), t);
        update_at(&mut s, Msg::VolumeChanged(Some(0.2), false), t);
        update_at(&mut s, Msg::Brightness(0.6), t);
        update_at(&mut s, Msg::Volume(0.6), t);
        // The system's older values, during the drag: dropped.
        update_at(&mut s, Msg::BrightnessChanged(Some(0.3)), t + HOLD / 2);
        update_at(&mut s, Msg::VolumeChanged(Some(0.3), true), t + HOLD / 2);
        assert_eq!((s.brightness, s.volume, s.muted), (Some(0.6), Some(0.6), false));
        // After the hold the system is followed again (the volume keys).
        update_at(&mut s, Msg::BrightnessChanged(Some(0.7)), t + HOLD);
        update_at(&mut s, Msg::VolumeChanged(Some(0.7), true), t + HOLD);
        assert_eq!((s.brightness, s.volume, s.muted), (Some(0.7), Some(0.7), true));
        // An output that goes away is followed at once.
        update_at(&mut s, Msg::Volume(0.5), t + HOLD);
        update_at(&mut s, Msg::VolumeChanged(None, false), t + HOLD);
        assert_eq!(s.volume, None);
    }

    #[test]
    fn bt_off_hides_device_name() {
        let mut s = State::default();
        update(&mut s, Msg::BtChanged(true, Some("Headphones".into())));
        assert_eq!(update(&mut s, Msg::BtToggled), vec![Effect::SetBt(false)]);
        assert_eq!(s.bt, Toggle { on: false, value: None });
    }

    #[test]
    fn brightness_none_hides_slider() {
        let mut s = State::default();
        // No backlight: the slider stays hidden and nothing is set.
        assert_eq!(update(&mut s, Msg::Brightness(0.5)), vec![]);
        assert_eq!(s.brightness, None);
        update(&mut s, Msg::BrightnessChanged(Some(0.3)));
        assert_eq!(update(&mut s, Msg::Brightness(1.4)), vec![Effect::SetBrightness(1.0)]);
        assert_eq!(s.brightness, Some(1.0));
        update(&mut s, Msg::BrightnessChanged(None));
        assert_eq!(s.brightness, None);
    }

    #[test]
    fn open_detail_wifi_spawns_cosmic_settings_wireless() {
        let mut s = State::default();
        assert_eq!(
            update(&mut s, Msg::OpenDetail(Detail::Wifi)),
            vec![Effect::Spawn(vec!["cosmic-settings".into(), "wireless".into()])]
        );
        assert_eq!(s, State::default());
    }

    #[test]
    fn layout_change_is_shown() {
        let mut s = State::default();
        assert_eq!(update(&mut s, Msg::LayoutChanged("English (US)".into())), vec![]);
        assert_eq!(s.layout, "English (US)");
    }
}
