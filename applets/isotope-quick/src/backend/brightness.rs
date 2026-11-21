//! Display brightness: cosmic-settings-daemon's `DisplayBrightness` and
//! `MaxDisplayBrightness` on the session bus, through the subscription the
//! stock battery applet uses (cosmic-settings-daemon-subscription). The
//! daemon owns the backlight (logind's SetBrightness), and cosmic-osd shows
//! its changes. No backlight (a desktop, a VM) leaves the slider hidden.

pub use cosmic_settings_daemon_subscription::{Event, Request, subscription};

/// The daemon's brightness as a fraction; `None` without a backlight.
pub fn fraction(brightness: i32, max: Option<i32>) -> Option<f32> {
    let max = max.filter(|m| *m > 0)?;
    Some((brightness.clamp(0, max) as f32) / (max as f32))
}

/// A fraction as the daemon's brightness. Never 0: a black screen is not a
/// brightness anyone picks on a slider (the keys still go lower).
pub fn raw(fraction: f32, max: i32) -> i32 {
    ((fraction.clamp(0.0, 1.0) * max as f32).round() as i32).clamp(1.min(max), max.max(0))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn no_backlight_is_none() {
        assert_eq!(fraction(5, None), None);
        assert_eq!(fraction(5, Some(0)), None);
        assert_eq!(fraction(50, Some(100)), Some(0.5));
        assert_eq!(fraction(150, Some(100)), Some(1.0));
    }

    #[test]
    fn a_fraction_is_never_black() {
        assert_eq!(raw(0.5, 255), 128);
        assert_eq!(raw(0.0, 255), 1);
        assert_eq!(raw(1.0, 20), 20);
        assert_eq!(raw(2.0, 20), 20);
    }
}
