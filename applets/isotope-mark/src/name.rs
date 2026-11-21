//! The name the top bar shows for the focused window.

/// What the applet keeps of one desktop entry: enough to match a window's app
/// ID to it and its name in the session's language.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct DesktopEntryName {
    /// The desktop file ID (its file name without `.desktop`).
    pub id: String,
    /// `StartupWMClass`, which some apps set to the app ID their windows use.
    pub wm_class: Option<String>,
    /// `Name`, localized.
    pub name: String,
}

/// The name of the focused app: the localized name of the desktop entry for
/// `app_id`, else the window title, else `""` (nothing focused: the desktop).
///
/// The entry is found the way the stock app list finds it
/// (`freedesktop_desktop_entry::find_app_by_id`): a `StartupWMClass` equal to
/// the app ID first, then the desktop file ID, ignoring ASCII case.
pub fn display_name(app_id: &str, title: &str, entries: &[DesktopEntryName]) -> String {
    let entry = (!app_id.is_empty())
        .then(|| {
            entries
                .iter()
                .find(|e| {
                    e.wm_class
                        .as_deref()
                        .is_some_and(|c| c.eq_ignore_ascii_case(app_id))
                })
                .or_else(|| entries.iter().find(|e| e.id.eq_ignore_ascii_case(app_id)))
        })
        .flatten();
    match entry {
        Some(e) if !e.name.is_empty() => e.name.clone(),
        _ => title.to_string(),
    }
}

/// `name` cut to at most `max` characters, the cut marked with an ellipsis,
/// so a long window title cannot push the workspaces off the top bar.
pub fn shorten(name: &str, max: usize) -> String {
    if name.chars().count() <= max {
        return name.to_string();
    }
    let mut cut: String = name.chars().take(max.saturating_sub(1)).collect();
    cut.truncate(cut.trim_end().len());
    cut.push('…');
    cut
}

#[cfg(test)]
mod tests {
    use super::*;

    fn entry(id: &str, wm_class: Option<&str>, name: &str) -> DesktopEntryName {
        DesktopEntryName {
            id: id.to_string(),
            wm_class: wm_class.map(str::to_string),
            name: name.to_string(),
        }
    }

    fn entries() -> Vec<DesktopEntryName> {
        vec![
            entry("com.system76.CosmicTerm", None, "Terminal"),
            entry("com.system76.CosmicFiles", None, "Files"),
            entry("org.mozilla.firefox", Some("firefox"), "Firefox"),
        ]
    }

    #[test]
    fn known_app_uses_entry_name() {
        assert_eq!(
            display_name("com.system76.CosmicFiles", "Home — Files", &entries()),
            "Files"
        );
    }

    #[test]
    fn known_app_matches_case_insensitively_and_by_wm_class() {
        assert_eq!(display_name("com.system76.cosmicfiles", "x", &entries()), "Files");
        assert_eq!(display_name("firefox", "Mozilla Firefox", &entries()), "Firefox");
    }

    #[test]
    fn unknown_app_uses_title() {
        assert_eq!(
            display_name("org.example.Unknown", "Some window", &entries()),
            "Some window"
        );
    }

    #[test]
    fn entry_without_a_name_uses_title() {
        let e = vec![entry("org.example.Nameless", None, "")];
        assert_eq!(display_name("org.example.Nameless", "Its title", &e), "Its title");
    }

    #[test]
    fn no_focus_is_empty() {
        assert_eq!(display_name("", "", &entries()), "");
    }

    #[test]
    fn short_names_are_kept_long_ones_cut() {
        assert_eq!(shorten("Files", 5), "Files");
        assert_eq!(shorten("Über lange Fenster", 6), "Über…");
        assert_eq!(shorten("abcdef", 4).chars().count(), 4);
    }
}
