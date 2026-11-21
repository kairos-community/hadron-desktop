//! The keyboard layout: the first layout of cosmic-comp's `xkb_config`
//! (com.system76.CosmicComp), named from xkeyboard-config's rules as the
//! stock input-sources applet names it. cosmic-comp falls back to
//! `XKB_DEFAULT_LAYOUT` when the key is empty, which is how Isotope sets the
//! installer's layout (/etc/profile.d/isotope-keyboard.sh), so this does too.
//!
//! Only `xkb_config` is read, with a struct of its own, so this does not
//! pull in cosmic-comp-config (and its libinput bindings) for one key.

use cosmic::cosmic_config::{self, CosmicConfigEntry, cosmic_config_derive::CosmicConfigEntry};
use serde::{Deserialize, Serialize};

pub const COMP_ID: &str = "com.system76.CosmicComp";

#[derive(Debug, Clone, Default, PartialEq, Deserialize, Serialize)]
pub struct Xkb {
    #[serde(default)]
    pub layout: String,
    #[serde(default)]
    pub variant: String,
}

/// The one key of cosmic-comp's config this reads.
#[derive(Debug, Clone, Default, PartialEq, CosmicConfigEntry)]
#[version = 1]
pub struct Comp {
    pub xkb_config: Xkb,
}

/// A layout and its variants, as xkeyboard-config describes them.
#[derive(Debug, Clone, PartialEq)]
pub struct Described {
    pub name: String,
    pub description: String,
    pub variants: Vec<(String, String)>,
}

pub fn described() -> Vec<Described> {
    match xkb_data::keyboard_layouts() {
        Ok(all) => all
            .layouts()
            .iter()
            .map(|l| Described {
                name: l.name().to_string(),
                description: l.description().to_string(),
                variants: l
                    .variants()
                    .map(|vs| {
                        vs.iter()
                            .map(|v| (v.name().to_string(), v.description().to_string()))
                            .collect()
                    })
                    .unwrap_or_default(),
            })
            .collect(),
        Err(err) => {
            tracing::warn!("cannot read the keyboard layouts: {err}");
            Vec::new()
        }
    }
}

/// The name of the layout in use: the first of `xkb` (or of the
/// environment's when `xkb` has none), described when known, else its code.
pub fn layout_name(xkb: &Xkb, env: &Xkb, layouts: &[Described]) -> String {
    let xkb = if xkb.layout.trim().is_empty() { env } else { xkb };
    let first = |csv: &str| csv.split(',').next().unwrap_or("").trim().to_string();
    let (layout, variant) = (first(&xkb.layout), first(&xkb.variant));
    let layout = if layout.is_empty() { "us".to_string() } else { layout };
    let Some(l) = layouts.iter().find(|l| l.name == layout) else {
        return layout;
    };
    if variant.is_empty() {
        return l.description.clone();
    }
    l.variants
        .iter()
        .find(|(name, _)| *name == variant)
        .map(|(_, d)| d.clone())
        .unwrap_or_else(|| l.description.clone())
}

/// What the session's environment says (XKB_DEFAULT_LAYOUT, _VARIANT).
pub fn env_xkb() -> Xkb {
    Xkb {
        layout: std::env::var("XKB_DEFAULT_LAYOUT").unwrap_or_default(),
        variant: std::env::var("XKB_DEFAULT_VARIANT").unwrap_or_default(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn xkb(layout: &str, variant: &str) -> Xkb {
        Xkb { layout: layout.into(), variant: variant.into() }
    }

    fn table() -> Vec<Described> {
        vec![
            Described {
                name: "us".into(),
                description: "English (US)".into(),
                variants: vec![("intl".into(), "English (US, intl., with dead keys)".into())],
            },
            Described { name: "it".into(), description: "Italian".into(), variants: vec![] },
        ]
    }

    #[test]
    fn names_the_first_layout() {
        let none = Xkb::default();
        assert_eq!(layout_name(&xkb("it,us", ""), &none, &table()), "Italian");
        assert_eq!(layout_name(&xkb("us", "intl"), &none, &table()), "English (US, intl., with dead keys)");
        // An unknown variant: the layout's name; an unknown layout: its code.
        assert_eq!(layout_name(&xkb("us", "nope"), &none, &table()), "English (US)");
        assert_eq!(layout_name(&xkb("de", ""), &none, &table()), "de");
    }

    #[test]
    fn falls_back_to_the_environment_then_us() {
        assert_eq!(layout_name(&Xkb::default(), &xkb("it", ""), &table()), "Italian");
        assert_eq!(layout_name(&Xkb::default(), &Xkb::default(), &table()), "English (US)");
    }

    #[test]
    fn reads_cosmic_comps_xkb_config() {
        // The file cosmic-comp and COSMIC Settings write.
        let ron = r#"(rules: "", model: "pc104", layout: "it,us", variant: ",intl", options: Some("caps:none"), repeat_delay: 600)"#;
        let x: Xkb = ron::from_str(ron).unwrap();
        assert_eq!(x, xkb("it,us", ",intl"));
    }
}
