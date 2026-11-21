//! The Isotope mark at the left of the COSMIC top bar, and the name of the
//! focused app next to it, like the Apple menu and the app menu on macOS.
//! Clicking it opens the Isotope menu (`isotope-menu`), which Super+Escape
//! also opens.
//!
//! See docs/superpowers/specs/2026-11-20-isotope-look-design.md, "Top bar: the
//! Isotope mark and the focused app".

mod focus;
mod name;

use std::{collections::HashSet, process::Command, rc::Rc, sync::LazyLock};

use cosmic::{
    Element, Task, app,
    desktop::fde,
    iced::{self, Alignment, Length},
    theme,
    widget::{self, Id, autosize},
};

use focus::Focus;
use name::DesktopEntryName;

/// The mark: docs/assets/isotope-mark.svg (the shapes update/icons.go draws
/// for the tray) without its colours, drawn as a symbolic icon so it takes
/// the panel's text colour. The same file is installed as the applet's icon.
const MARK_SVG: &[u8] = include_bytes!("../data/icons/scalable/apps/io.kairos.Isotope.Mark-symbolic.svg");

/// The program the mark opens.
const MENU: &str = "isotope-menu";

/// The longest name shown, in characters.
const MAX_NAME: usize = 40;

static AUTOSIZE_MAIN_ID: LazyLock<Id> = LazyLock::new(|| Id::new("autosize-main"));

fn main() -> iced::Result {
    tracing_subscriber::fmt::init();
    let _ = tracing_log::LogTracer::init();
    focus::close_privileged_socket_on_exec();
    cosmic::applet::run::<Mark>(())
}

struct Mark {
    core: cosmic::app::Core,
    locales: Vec<String>,
    entries: Vec<DesktopEntryName>,
    /// App IDs with no desktop entry even after a reload, so a window that
    /// has none does not reload the entries at every title change.
    unknown: HashSet<String>,
    focus: Focus,
}

#[derive(Debug, Clone)]
enum Message {
    Press,
    Focus(Focus),
}

/// The name of every desktop entry on the system, in the session's language.
fn desktop_entries(locales: &[String]) -> Vec<DesktopEntryName> {
    fde::Iter::new(fde::default_paths())
        .filter_map(|p| fde::DesktopEntry::from_path(p, Some(locales)).ok())
        .map(|e| DesktopEntryName {
            id: e.id().to_string(),
            wm_class: e.startup_wm_class().map(str::to_string),
            name: e.name(locales).map(|n| n.to_string()).unwrap_or_default(),
        })
        .collect()
}

impl Mark {
    fn has_entry(&self, app_id: &str) -> bool {
        self.entries.iter().any(|e| {
            e.id.eq_ignore_ascii_case(app_id)
                || e.wm_class.as_deref().is_some_and(|c| c.eq_ignore_ascii_case(app_id))
        })
    }

    fn name(&self) -> String {
        name::shorten(
            &name::display_name(&self.focus.app_id, &self.focus.title, &self.entries),
            MAX_NAME,
        )
    }
}

impl cosmic::Application for Mark {
    type Message = Message;
    type Executor = cosmic::SingleThreadExecutor;
    type Flags = ();
    const APP_ID: &'static str = "io.kairos.Isotope.Mark";

    fn init(core: cosmic::app::Core, _flags: ()) -> (Self, app::Task<Message>) {
        let locales = fde::get_languages_from_env();
        let entries = desktop_entries(&locales);
        (
            Self {
                core,
                locales,
                entries,
                unknown: HashSet::new(),
                focus: Focus::default(),
            },
            Task::none(),
        )
    }

    fn core(&self) -> &cosmic::app::Core {
        &self.core
    }

    fn core_mut(&mut self) -> &mut cosmic::app::Core {
        &mut self.core
    }

    fn style(&self) -> Option<iced::theme::Style> {
        Some(cosmic::applet::style())
    }

    fn update(&mut self, message: Message) -> app::Task<Message> {
        match message {
            Message::Press => {
                // The panel's Wayland sockets are this applet's, not the menu's.
                match Command::new(MENU)
                    .env_remove("WAYLAND_SOCKET")
                    .env_remove("X_PRIVILEGED_WAYLAND_SOCKET")
                    .spawn()
                {
                    Ok(mut child) => {
                        std::thread::spawn(move || {
                            let _ = child.wait();
                        });
                    }
                    Err(err) => tracing::error!("cannot start {MENU}: {err}"),
                }
            }
            Message::Focus(focus) => {
                // An app installed since the applet started: read the entries again, once.
                let id = &focus.app_id;
                if !id.is_empty() && !self.unknown.contains(id) && !self.has_entry(id) {
                    self.entries = desktop_entries(&self.locales);
                    if !self.has_entry(id) {
                        self.unknown.insert(id.clone());
                    }
                }
                self.focus = focus;
            }
        }
        Task::none()
    }

    fn view(&self) -> Element<'_, Message> {
        let applet = &self.core.applet;
        let (icon_w, icon_h) = applet.suggested_size(true);
        let (major_pad, minor_pad) = applet.suggested_padding(true);
        let mark = widget::icon(widget::icon::from_svg_bytes(MARK_SVG).symbolic(true))
            .class(theme::Svg::Custom(Rc::new(|theme| iced::widget::svg::Style {
                color: Some(theme.cosmic().background(theme.transparent).on.into()),
            })))
            .width(Length::Fixed(f32::from(icon_w)))
            .height(Length::Fixed(f32::from(icon_h)));

        let content: Element<'_, Message> = if applet.is_horizontal() {
            // As the stock time applet does: a row as tall as an icon button
            // (an empty space of that height), its items centred in it, in a
            // button padded only along the bar. A fixed-height button would
            // put its content at the top.
            let name = self.name();
            let mut row = widget::row::with_capacity(2)
                .push(mark)
                .spacing(self.core.system_theme().cosmic().space_xs())
                .align_y(Alignment::Center);
            if !name.is_empty() {
                row = row.push(applet.text(name).font(cosmic::font::semibold()));
            }
            let tall = widget::space::vertical().height(Length::Fixed(f32::from(icon_h + 2 * minor_pad)));
            widget::button::custom(iced::widget::row![row, tall].align_y(Alignment::Center))
                .padding([0, major_pad])
                .class(theme::Button::AppletIcon)
                .on_press_down(Message::Press)
                .into()
        } else {
            // On a vertical panel there is no room for a name: the mark alone.
            applet
                .button_from_element(mark, true)
                .on_press_down(Message::Press)
                .into()
        };
        autosize::autosize(content, AUTOSIZE_MAIN_ID.clone()).into()
    }

    fn subscription(&self) -> iced::Subscription<Message> {
        focus::subscription().map(Message::Focus)
    }
}

#[cfg(test)]
mod tests {
    /// The shapes of an SVG: every `d`, `cx`, `cy` and `r` attribute, in order.
    fn shapes(svg: &str) -> Vec<String> {
        let mut out = Vec::new();
        for attr in [" d=\"", " cx=\"", " cy=\"", " r=\""] {
            let mut rest = svg;
            while let Some(i) = rest.find(attr) {
                let v = &rest[i + attr.len()..];
                let end = v.find('"').unwrap();
                out.push(format!("{}{}", attr.trim(), &v[..end]));
                rest = &v[end..];
            }
        }
        out
    }

    #[test]
    fn mark_is_the_isotope_mark_without_colours() {
        let source = include_str!("../../../docs/assets/isotope-mark.svg");
        let mark = std::str::from_utf8(super::MARK_SVG).unwrap();
        assert_eq!(shapes(mark), shapes(source));
        assert_eq!(shapes(mark).len(), 5);
        assert!(!mark.contains("fill"), "a symbolic icon takes the panel's colour");
    }

    /// The numbers after `attr="` in an SVG, e.g. a path's coordinates.
    fn numbers(svg: &str, attr: &str) -> Vec<f32> {
        let start = svg.find(&format!(" {attr}=\"")).unwrap() + attr.len() + 3;
        let value = &svg[start..start + svg[start..].find('"').unwrap()];
        value
            .split(|c: char| !(c.is_ascii_digit() || c == '.'))
            .filter(|n| !n.is_empty())
            .map(|n| n.parse().unwrap())
            .collect()
    }

    #[test]
    fn mark_fills_its_icon_like_a_stock_symbolic_icon() {
        // The stock 16 px symbolic icons reach to about a pixel of their
        // edges. The mark's paths span 68-444 x 33-478 of the 512 tray
        // canvas; the icon's square viewBox is cropped to them so the mark
        // in the bar is as tall as its neighbours.
        let mark = std::str::from_utf8(super::MARK_SVG).unwrap();
        let view = numbers(mark, "viewBox");
        assert_eq!(view.len(), 4);
        assert_eq!(view[2], view[3], "square");
        let (mut lo_x, mut hi_x, mut lo_y, mut hi_y) = (f32::MAX, f32::MIN, f32::MAX, f32::MIN);
        let mut rest = mark;
        while let Some(i) = rest.find(" d=\"") {
            for xy in numbers(rest, "d").chunks(2) {
                lo_x = lo_x.min(xy[0]);
                hi_x = hi_x.max(xy[0]);
                lo_y = lo_y.min(xy[1]);
                hi_y = hi_y.max(xy[1]);
            }
            rest = &rest[i + 4..];
        }
        assert!(lo_x >= view[0] && lo_y >= view[1], "the mark is inside the viewBox");
        assert!(hi_x <= view[0] + view[2] && hi_y <= view[1] + view[3]);
        let px = 16.0 / view[3];
        assert!((hi_y - lo_y) * px >= 15.0, "{} px tall at 16 px", (hi_y - lo_y) * px);
        // Centred: the margins differ by under half a pixel.
        let (top, bottom) = (lo_y - view[1], view[1] + view[3] - hi_y);
        let (left, right) = (lo_x - view[0], view[0] + view[2] - hi_x);
        assert!((top - bottom).abs() * px < 0.5 && (left - right).abs() * px < 0.5);
    }
}
