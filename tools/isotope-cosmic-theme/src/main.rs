//! isotope-cosmic-theme <cosmic-theme.json> <out-dir>
//!
//! Overlays the Isotope theme (theme/cosmic-theme.json) on cosmic-theme's
//! ThemeBuilder, builds the theme, and writes the builder and the computed
//! theme, dark and light, as cosmic-config entries, plus the Mode entry that
//! makes dark the default. <out-dir> is meant to be /usr/share/cosmic.

use cosmic_config::CosmicConfigEntry;
use cosmic_theme::palette::{Srgb, Srgba};
use cosmic_theme::{
    BlurStrength, CornerRadii, CosmicPalette, CosmicPaletteInner, Roundness, Theme, ThemeBuilder,
    ThemeMode,
};
use serde_json::Value;
use std::path::{Path, PathBuf};

type Result<T> = std::result::Result<T, String>;

fn hex_bytes(s: &str) -> Result<[u8; 3]> {
    let h = s.strip_prefix('#').ok_or_else(|| format!("{s:?}: not #rrggbb"))?;
    if h.len() != 6 {
        return Err(format!("{s:?}: not #rrggbb"));
    }
    let b = |i: usize| u8::from_str_radix(&h[i..i + 2], 16).map_err(|e| format!("{s:?}: {e}"));
    Ok([b(0)?, b(2)?, b(4)?])
}

fn srgb(s: &str) -> Result<Srgb> {
    let [r, g, b] = hex_bytes(s)?;
    Ok(Srgb::<u8>::new(r, g, b).into_format())
}

fn srgba(s: &str) -> Result<Srgba> {
    let [r, g, b] = hex_bytes(s)?;
    Ok(Srgba::<u8>::new(r, g, b, 255).into_format())
}

/// An 8-bit "#rrggbb" of a colour, rounded like the config round trip.
fn to_hex(c: Srgb) -> String {
    let c: Srgb<u8> = c.into_format();
    format!("#{:02x}{:02x}{:02x}", c.red, c.green, c.blue)
}

fn palette_slot<'a>(p: &'a mut CosmicPaletteInner, key: &str) -> Result<&'a mut Srgba> {
    Ok(match key {
        "bright_red" => &mut p.bright_red,
        "bright_green" => &mut p.bright_green,
        "bright_orange" => &mut p.bright_orange,
        "neutral_0" => &mut p.neutral_0,
        "neutral_1" => &mut p.neutral_1,
        "neutral_2" => &mut p.neutral_2,
        "neutral_3" => &mut p.neutral_3,
        "neutral_4" => &mut p.neutral_4,
        "neutral_5" => &mut p.neutral_5,
        "neutral_6" => &mut p.neutral_6,
        "neutral_7" => &mut p.neutral_7,
        "neutral_8" => &mut p.neutral_8,
        "neutral_9" => &mut p.neutral_9,
        "neutral_10" => &mut p.neutral_10,
        "accent_blue" => &mut p.accent_blue,
        "accent_indigo" => &mut p.accent_indigo,
        "accent_purple" => &mut p.accent_purple,
        "accent_pink" => &mut p.accent_pink,
        "accent_red" => &mut p.accent_red,
        "accent_orange" => &mut p.accent_orange,
        "accent_yellow" => &mut p.accent_yellow,
        "accent_green" => &mut p.accent_green,
        "accent_warm_grey" => &mut p.accent_warm_grey,
        _ => return Err(format!("palette: unknown key {key:?}")),
    })
}

fn str_of<'a>(v: &'a Value, what: &str) -> Result<&'a str> {
    v.as_str().ok_or_else(|| format!("{what}: not a string"))
}

fn obj<'a>(v: &'a Value, what: &str) -> Result<&'a serde_json::Map<String, Value>> {
    v.get(what)
        .and_then(Value::as_object)
        .ok_or_else(|| format!("missing object {what:?}"))
}

/// Build the builder and the theme of one mode from its JSON section.
pub fn build(mode: &Value, dark: bool) -> Result<(ThemeBuilder, Theme)> {
    let top = mode.as_object().ok_or("not an object")?;
    if let Some(k) = top.keys().find(|k| !["palette", "builder", "look", "on_accent"].contains(&k.as_str())) {
        return Err(format!("unknown section {k:?}"));
    }
    let mut b = if dark { ThemeBuilder::dark() } else { ThemeBuilder::light() };

    // Palette: every key lands on the field of that name.
    let mut inner = b.palette.clone().inner();
    for (k, v) in obj(mode, "palette")? {
        if k == "name" {
            inner.name = str_of(v, "palette.name")?.to_owned();
        } else {
            *palette_slot(&mut inner, k)? = srgba(str_of(v, k)?)?;
        }
    }
    b.palette = if dark { CosmicPalette::Dark(inner) } else { CosmicPalette::Light(inner) };

    for (k, v) in obj(mode, "builder")? {
        let s = str_of(v, k)?;
        match k.as_str() {
            "accent" => b.accent = Some(srgb(s)?),
            "success" => b.success = Some(srgb(s)?),
            "warning" => b.warning = Some(srgb(s)?),
            "destructive" => b.destructive = Some(srgb(s)?),
            "bg_color" => b.bg_color = Some(srgba(s)?),
            "primary_container_bg" => b.primary_container_bg = Some(srgba(s)?),
            "secondary_container_bg" => b.secondary_container_bg = Some(srgba(s)?),
            // The focused window's outline in cosmic-comp (None: the accent).
            "window_hint" => b.window_hint = Some(srgb(s)?),
            _ => return Err(format!("builder: unknown key {k:?}")),
        }
    }

    for (k, v) in obj(mode, "look")? {
        let flag = || v.as_bool().ok_or_else(|| format!("look.{k}: not a bool"));
        match k.as_str() {
            "frosted" => {
                b.frosted = serde_json::from_value::<BlurStrength>(v.clone())
                    .map_err(|e| format!("look.frosted: {e}"))?
            }
            "frosted_windows" => b.frosted_windows = flag()?,
            "frosted_system_interface" => b.frosted_system_interface = flag()?,
            "frosted_panel" => b.frosted_panel = flag()?,
            "frosted_applets" => b.frosted_applets = flag()?,
            "frosted_maximized_apps" => b.frosted_maximized_apps = flag()?,
            "active_hint" => {
                b.active_hint = v.as_u64().ok_or("look.active_hint: not a number")? as u32
            }
            "gaps" => {
                let g: Vec<u64> = v
                    .as_array()
                    .ok_or("look.gaps: not an array")?
                    .iter()
                    .map(|n| n.as_u64().ok_or("look.gaps: not a number"))
                    .collect::<std::result::Result<_, _>>()?;
                if g.len() != 2 {
                    return Err("look.gaps: want [outer, inner]".into());
                }
                b.gaps = (g[0] as u32, g[1] as u32);
            }
            "corner_radii" => {
                let r = match str_of(v, "look.corner_radii")? {
                    "Round" => Roundness::Round,
                    "SlightlyRound" => Roundness::SlightlyRound,
                    "Square" => Roundness::Square,
                    other => return Err(format!("look.corner_radii: unknown {other:?}")),
                };
                b.corner_radii = CornerRadii::from(r);
            }
            _ => return Err(format!("look: unknown key {k:?}")),
        }
    }

    let mut theme = b.clone().build();

    // on_accent is not a builder field: set the on-colour of the accent and of
    // the accent button after the build.
    let on = srgba(str_of(mode.get("on_accent").ok_or("missing on_accent")?, "on_accent")?)?;
    theme.accent.on = on;
    theme.accent_button.on = on;

    // The computed accent must be the one the JSON names.
    let want = str_of(&mode["builder"]["accent"], "builder.accent")?;
    let got = to_hex(theme.accent.base.color);
    if got != want {
        return Err(format!("computed accent {got} differs from JSON accent {want}"));
    }
    Ok((b, theme))
}

fn write_all(json: &Value, out: &Path) -> Result<()> {
    // cosmic-config writes under $XDG_CONFIG_HOME/cosmic.
    let tmp = std::env::temp_dir().join(format!("isotope-cosmic-theme-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&tmp);
    std::env::set_var("XDG_CONFIG_HOME", &tmp);

    let e = |e: cosmic_config::Error| e.to_string();
    for (key, dark) in [("dark", true), ("light", false)] {
        let section = json.get(key).ok_or_else(|| format!("missing {key:?}"))?;
        let (b, t) = build(section, dark).map_err(|m| format!("{key}: {m}"))?;
        let (bc, tc) = if dark {
            (ThemeBuilder::dark_config(), Theme::dark_config())
        } else {
            (ThemeBuilder::light_config(), Theme::light_config())
        };
        b.write_entry(&bc.map_err(e)?).map_err(e)?;
        t.write_entry(&tc.map_err(e)?).map_err(e)?;
    }
    for k in json.as_object().into_iter().flat_map(|o| o.keys()) {
        if k != "dark" && k != "light" {
            return Err(format!("unknown top-level key {k:?}"));
        }
    }
    ThemeMode { is_dark: true, auto_switch: false }
        .write_entry(&ThemeMode::config().map_err(e)?)
        .map_err(e)?;

    copy_dir(&tmp.join("cosmic"), out).map_err(|x| x.to_string())?;
    let _ = std::fs::remove_dir_all(&tmp);
    Ok(())
}

fn copy_dir(from: &Path, to: &Path) -> std::io::Result<()> {
    std::fs::create_dir_all(to)?;
    for ent in std::fs::read_dir(from)? {
        let ent = ent?;
        let dst: PathBuf = to.join(ent.file_name());
        if ent.file_type()?.is_dir() {
            copy_dir(&ent.path(), &dst)?;
        } else {
            std::fs::copy(ent.path(), dst)?;
        }
    }
    Ok(())
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() != 3 {
        eprintln!("usage: isotope-cosmic-theme <cosmic-theme.json> <out-dir>");
        std::process::exit(2);
    }
    let run = || -> Result<()> {
        let text = std::fs::read_to_string(&args[1]).map_err(|e| format!("{}: {e}", args[1]))?;
        let json: Value = serde_json::from_str(&text).map_err(|e| format!("{}: {e}", args[1]))?;
        write_all(&json, Path::new(&args[2]))
    };
    if let Err(m) = run() {
        eprintln!("isotope-cosmic-theme: {m}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn fixture() -> Value {
        serde_json::from_str(include_str!("../../../theme/cosmic-theme.json")).unwrap()
    }

    #[test]
    fn dark_accent_matches_json() {
        let j = fixture();
        let (_, t) = build(&j["dark"], true).unwrap();
        assert_eq!(to_hex(t.accent.base.color), j["dark"]["builder"]["accent"]);
    }

    #[test]
    fn light_is_not_dark() {
        let j = fixture();
        let (_, dark) = build(&j["dark"], true).unwrap();
        let (_, light) = build(&j["light"], false).unwrap();
        assert!(!light.is_dark && dark.is_dark);
    }

    #[test]
    fn on_accent_is_set() {
        let j = fixture();
        for (k, d) in [("dark", true), ("light", false)] {
            let (_, t) = build(&j[k], d).unwrap();
            let want = j[k]["on_accent"].as_str().unwrap();
            assert_eq!(to_hex(t.accent.on.color), want);
            assert_eq!(to_hex(t.accent_button.on.color), want);
        }
    }

    #[test]
    fn frosted_flags_carried() {
        let j = fixture();
        let (_, t) = build(&j["dark"], true).unwrap();
        assert!(t.frosted_windows && !t.frosted_maximized_apps);
    }

    #[test]
    fn focus_hint_carried() {
        let j = fixture();
        for (k, d) in [("dark", true), ("light", false)] {
            let (b, t) = build(&j[k], d).unwrap();
            let want = j[k]["builder"]["window_hint"].as_str().unwrap();
            assert_eq!(b.window_hint.map(to_hex).as_deref(), Some(want));
            assert_eq!(t.window_hint.map(to_hex).as_deref(), Some(want));
            assert_eq!(u64::from(t.active_hint), j[k]["look"]["active_hint"].as_u64().unwrap());
            assert!(t.active_hint > 0);
        }
    }

    #[test]
    fn unknown_mode_section_is_an_error() {
        let mut j = fixture();
        j["dark"]["looks"] = serde_json::json!({});
        assert!(build(&j["dark"], true).unwrap_err().contains("looks"));
    }

    #[test]
    fn unknown_key_is_an_error() {
        let mut j = fixture();
        j["dark"]["palette"]["neutral_99"] = "#000000".into();
        assert!(build(&j["dark"], true).is_err());
    }
}
