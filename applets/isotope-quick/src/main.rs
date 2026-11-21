//! Quick settings: one top-bar button (the network and volume icons; the
//! stock battery applet stays next to it) that opens one popup with Wi-Fi,
//! Bluetooth and the keyboard layout, Dark style and Do not disturb, the
//! display brightness and sound volume, and Settings and the power buttons.
//! It replaces the stock network, Bluetooth, audio and power applets in
//! Isotope's top bar; they stay installed.
//!
//! `state.rs` decides (a pure reducer, tested); `backend/` reads and sets the
//! system; this file draws, and carries the reducer's effects to the
//! backends. See docs/superpowers/specs/2026-11-20-isotope-look-design.md,
//! "Quick settings (control centre)", and the mock's `.cc` popup.

mod backend;
mod state;

use std::{process::Command, rc::Rc, sync::LazyLock};

use cosmic::{
    Element, Task, app,
    applet::token::subscription::{TokenRequest, TokenUpdate, activation_token_subscription},
    cctk::sctk::reexports::calloop,
    iced::{
        self, Alignment, Background, Border, Color, Length, Subscription,
        widget::{column, row, stack},
        window,
    },
    theme,
    widget::{self, Id, autosize, button, container, icon, slider, text},
};
use cosmic::cosmic_theme::palette::WithAlpha;
use tokio::sync::mpsc::UnboundedSender;

use backend::{audio, bluetooth, brightness, dnd, keyboard, mode, network};
use state::{Detail, Effect, Msg, State};

static AUTOSIZE_MAIN_ID: LazyLock<Id> = LazyLock::new(|| Id::new("autosize-main"));

/// The popup's glyphs: the mock's (docs/superpowers/specs/assets/isotope-look/
/// mock.html), drawn as symbolic icons so they take the theme's colours.
mod glyph {
    pub const WIFI: &[u8] = include_bytes!("icons/wifi.svg");
    pub const BT: &[u8] = include_bytes!("icons/bt.svg");
    pub const KBD: &[u8] = include_bytes!("icons/kbd.svg");
    pub const MOON: &[u8] = include_bytes!("icons/moon.svg");
    pub const BELL: &[u8] = include_bytes!("icons/bell.svg");
    pub const SUN: &[u8] = include_bytes!("icons/sun.svg");
    pub const VOL: &[u8] = include_bytes!("icons/vol.svg");
}

/// The round toggle of a row or tile, and its glyph.
const CIRCLE: f32 = 36.0;
const GLYPH: u16 = 18;
/// The height of a slider, a pill as in the mock.
const PILL: f32 = 28.0;

fn main() -> iced::Result {
    tracing_subscriber::fmt::init();
    let _ = tracing_log::LogTracer::init();
    cosmic::applet::run::<Quick>(())
}

struct Quick {
    core: cosmic::app::Core,
    popup: Option<window::Id>,
    state: State,
    /// Asks the panel for an activation token for a command it starts.
    token_tx: Option<calloop::channel::Sender<TokenRequest>>,
    /// Commands waiting for their token.
    pending: Vec<Vec<String>>,
    layouts: Vec<keyboard::Described>,
    env_xkb: keyboard::Xkb,
    session: Option<zbus::Connection>,
    brightness_tx: Option<UnboundedSender<brightness::Request>>,
    max_brightness: Option<i32>,
    raw_brightness: Option<i32>,
    audio_tx: Option<UnboundedSender<audio::Request>>,
}

#[derive(Debug, Clone)]
enum Message {
    TogglePopup,
    Closed(window::Id),
    State(Msg),
    Xkb(keyboard::Xkb),
    Session(Option<zbus::Connection>),
    Brightness(brightness::Event),
    Audio(audio::Event),
    Token(TokenUpdate),
    Ignore,
}

impl Quick {
    fn reduce(&mut self, msg: Msg) -> app::Task<Message> {
        let close = matches!(msg, Msg::OpenDetail(_));
        let effects = state::update(&mut self.state, msg);
        let mut tasks: Vec<app::Task<Message>> = Vec::new();
        for effect in effects {
            match effect {
                Effect::SetWifi(on) => tasks.push(cosmic::task::future(async move {
                    network::set_wifi(on).await.map_or(Message::Ignore, Message::State)
                })),
                Effect::SetBt(on) => tasks.push(cosmic::task::future(async move {
                    bluetooth::set_bt(on).await.map_or(Message::Ignore, Message::State)
                })),
                Effect::SetDark(dark) => mode::set_dark(dark),
                Effect::SetDnd(on) => dnd::set_dnd(on),
                Effect::SetBrightness(f) => {
                    if let (Some(tx), Some(max)) = (&self.brightness_tx, self.max_brightness) {
                        let _ = tx.send(brightness::Request::SetDisplayBrightness(
                            brightness::raw(f, max),
                        ));
                    }
                }
                Effect::SetVolume(v) => {
                    if let Some(tx) = &self.audio_tx {
                        let _ = tx.send(audio::Request::Volume(v));
                    }
                }
                Effect::SetMute(m) => {
                    if let Some(tx) = &self.audio_tx {
                        let _ = tx.send(audio::Request::Mute(m));
                    }
                }
                Effect::Spawn(argv) => self.spawn(argv),
            }
        }
        // A row or button that opens something closes the popup.
        if close && let Some(p) = self.popup.take() {
            tasks.push(cosmic::surface::surface_task(cosmic::surface::action::destroy_popup(p)));
        }
        Task::batch(tasks)
    }

    /// Starts a command with an activation token from the panel when it
    /// can, so the window it opens gets the focus, as the stock applets do.
    fn spawn(&mut self, argv: Vec<String>) {
        if argv.is_empty() {
            return;
        }
        if let Some(tx) = &self.token_tx {
            let exec = argv.join(" ");
            if tx
                .send(TokenRequest { app_id: <Self as cosmic::Application>::APP_ID.to_string(), exec })
                .is_ok()
            {
                self.pending.push(argv);
                return;
            }
        }
        start(&argv, None);
    }
}

fn start(argv: &[String], token: Option<String>) {
    let mut cmd = Command::new(&argv[0]);
    cmd.args(&argv[1..])
        // The panel's Wayland sockets are this applet's, not the command's.
        .env_remove("WAYLAND_SOCKET")
        .env_remove("X_PRIVILEGED_WAYLAND_SOCKET");
    if let Some(token) = token {
        cmd.env("XDG_ACTIVATION_TOKEN", &token);
        cmd.env("DESKTOP_STARTUP_ID", &token);
    }
    tokio::spawn(cosmic::process::spawn(cmd));
}

impl cosmic::Application for Quick {
    type Message = Message;
    type Executor = cosmic::SingleThreadExecutor;
    type Flags = ();
    const APP_ID: &'static str = "io.kairos.Isotope.QuickSettings";

    fn init(core: cosmic::app::Core, _flags: ()) -> (Self, app::Task<Message>) {
        let session = Task::perform(zbus::Connection::session(), |res| {
            if let Err(err) = &res {
                tracing::error!("no session bus: {err}");
            }
            cosmic::Action::App(Message::Session(res.ok()))
        });
        (
            Self {
                core,
                popup: None,
                state: State::default(),
                token_tx: None,
                pending: Vec::new(),
                layouts: keyboard::described(),
                env_xkb: keyboard::env_xkb(),
                session: None,
                brightness_tx: None,
                max_brightness: None,
                raw_brightness: None,
                audio_tx: None,
            },
            session,
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

    fn on_close_requested(&self, id: window::Id) -> Option<Message> {
        Some(Message::Closed(id))
    }

    fn update(&mut self, message: Message) -> app::Task<Message> {
        match message {
            Message::TogglePopup => {
                if let Some(p) = self.popup.take() {
                    return cosmic::surface::surface_task(cosmic::surface::action::destroy_popup(p));
                }
                return cosmic::surface::surface_task(cosmic::surface::action::app_popup(
                    |_| Default::default(),
                    |app: &mut Quick| {
                        let id = window::Id::unique();
                        app.popup = Some(id);
                        // Anchor on the whole button: two icons, not one.
                        let applet = &app.core.applet;
                        let (w, _) = applet.suggested_size(true);
                        let (major, minor) = applet.suggested_padding(true);
                        let gap = app.core.system_theme().cosmic().space_xxs();
                        let pad = |p: u16| Some(i32::from(p) + (i32::from(w) + i32::from(gap)) / 2);
                        let (wp, hp) = if applet.is_horizontal() {
                            (pad(major), None)
                        } else {
                            (Some(i32::from(minor)), pad(major))
                        };
                        applet.get_popup_settings(app.core.main_window_id().unwrap(), id, None, wp, hp)
                    },
                    None,
                ));
            }
            Message::Closed(id) => {
                if self.popup == Some(id) {
                    self.popup = None;
                }
            }
            Message::State(msg) => return self.reduce(msg),
            Message::Xkb(xkb) => {
                let name = keyboard::layout_name(&xkb, &self.env_xkb, &self.layouts);
                return self.reduce(Msg::LayoutChanged(name));
            }
            Message::Session(conn) => self.session = conn,
            Message::Brightness(event) => {
                match event {
                    brightness::Event::Sender(tx) => self.brightness_tx = Some(tx),
                    brightness::Event::MaxDisplayBrightness(max) => self.max_brightness = Some(max),
                    brightness::Event::DisplayBrightness(b) => self.raw_brightness = Some(b),
                }
                if let Some(b) = self.raw_brightness {
                    let f = brightness::fraction(b, self.max_brightness);
                    return self.reduce(Msg::BrightnessChanged(f));
                }
            }
            Message::Audio(audio::Event::Sender(tx)) => self.audio_tx = Some(tx),
            Message::Audio(audio::Event::Changed(msg)) => return self.reduce(msg),
            Message::Token(u) => match u {
                TokenUpdate::Init(tx) => self.token_tx = Some(tx),
                TokenUpdate::Finished => self.token_tx = None,
                TokenUpdate::ActivationToken { token, exec } => {
                    if let Some(i) = self.pending.iter().position(|a| a.join(" ") == exec) {
                        let argv = self.pending.remove(i);
                        start(&argv, token);
                    }
                }
            },
            Message::Ignore => {}
        }
        Task::none()
    }

    fn view(&self) -> Element<'_, Message> {
        let applet = &self.core.applet;
        let (w, h) = applet.suggested_size(true);
        let (major, minor) = applet.suggested_padding(true);
        let gap = self.core.system_theme().cosmic().space_xxs();
        let panel_icon = |name: &'static str| {
            widget::icon(icon::from_name(name).size(w).symbolic(true).into())
                .class(theme::Svg::Custom(Rc::new(|theme| iced::widget::svg::Style {
                    color: Some(theme.cosmic().background(theme.transparent).on.into()),
                })))
                .width(Length::Fixed(f32::from(w)))
                .height(Length::Fixed(f32::from(h)))
        };
        let icons = [state::net_icon(&self.state), state::volume_icon(&self.state)];
        // As the stock time applet does: the icons in a row (a column on a
        // vertical bar) as thick as an icon button, made so by an empty
        // space, and centred in it; the button is padded only along the
        // bar. A button of fixed thickness puts its content at its start.
        let content: Element<'_, Message> = if applet.is_horizontal() {
            let items = row(icons.map(|i| panel_icon(i).into()))
                .spacing(gap)
                .align_y(Alignment::Center);
            let tall = widget::space::vertical().height(Length::Fixed(f32::from(h + 2 * minor)));
            button::custom(row![items, tall].align_y(Alignment::Center))
                .padding([0, major])
                .class(theme::Button::AppletIcon)
                .on_press_down(Message::TogglePopup)
                .into()
        } else {
            let items = column(icons.map(|i| panel_icon(i).into()))
                .spacing(gap)
                .align_x(Alignment::Center);
            let wide = widget::space::horizontal().width(Length::Fixed(f32::from(w + 2 * minor)));
            button::custom(column![items, wide].align_x(Alignment::Center))
                .padding([major, 0])
                .class(theme::Button::AppletIcon)
                .on_press_down(Message::TogglePopup)
                .into()
        };
        autosize::autosize(content, AUTOSIZE_MAIN_ID.clone()).into()
    }

    fn view_window(&self, id: window::Id) -> Element<'_, Message> {
        if self.popup != Some(id) {
            return widget::text("").into();
        }
        let s = &self.state;
        let gap = 10;
        let on_off = |t: &state::Toggle<Option<String>>| match (t.on, &t.value) {
            (false, _) => "Off".to_string(),
            (true, Some(name)) => name.clone(),
            (true, None) => "On".to_string(),
        };

        let net = block(
            column![
                toggle_row(glyph::WIFI, "Wi-Fi", on_off(&s.wifi), s.wifi.on, Msg::WifiToggled, Detail::Wifi),
                toggle_row(glyph::BT, "Bluetooth", on_off(&s.bt), s.bt.on, Msg::BtToggled, Detail::Bluetooth),
                toggle_row(
                    glyph::KBD,
                    "Keyboard",
                    s.layout.clone(),
                    false,
                    Msg::OpenDetail(Detail::Keyboard),
                    Detail::Keyboard,
                ),
            ]
            .spacing(gap),
        )
        .width(Length::FillPortion(1));

        let tiles = column![
            tile(glyph::MOON, "Dark style", s.dark, Msg::DarkToggled),
            tile(glyph::BELL, "Do not disturb", s.dnd, Msg::DndToggled),
        ]
        .spacing(gap)
        .height(Length::Fill)
        .width(Length::FillPortion(1));

        let mut content = column![row![net, tiles].spacing(gap)].spacing(gap);

        if let Some(b) = s.brightness {
            content = content.push(slider_block("Display", glyph::SUN, b, None, |v| {
                Message::State(Msg::Brightness(v))
            }));
        }
        if let Some(v) = s.volume {
            content = content.push(slider_block(
                "Sound",
                glyph::VOL,
                if s.muted { 0.0 } else { v },
                Some(Msg::MuteToggled),
                |v| Message::State(Msg::Volume(v)),
            ));
        }

        let power = |name: &'static str, detail: Detail| {
            button::custom(
                widget::icon(icon::from_name(name).size(20).symbolic(true).into())
                    .width(Length::Fixed(20.0))
                    .height(Length::Fixed(20.0)),
            )
            .padding(8)
            .class(theme::Button::Icon)
            .on_press(Message::State(Msg::OpenDetail(detail)))
        };
        let bottom = row![
            button::custom(text::body("Settings…"))
                .padding([8, 12])
                .class(theme::Button::Text)
                .on_press(Message::State(Msg::OpenDetail(Detail::Settings))),
            widget::space::horizontal().width(Length::Fill),
            power("system-log-out-symbolic", Detail::LogOut),
            power("system-suspend-symbolic", Detail::Suspend),
            power("system-reboot-symbolic", Detail::Restart),
            power("system-shutdown-symbolic", Detail::ShutDown),
        ]
        .spacing(2)
        .align_y(Alignment::Center);
        content = content.push(bottom);

        self.core.applet.popup_container(container(content).padding(12)).into()
    }

    fn subscription(&self) -> Subscription<Message> {
        let mut subs = vec![
            network::subscription().map(Message::State),
            bluetooth::subscription().map(Message::State),
            audio::subscription().map(Message::Audio),
            activation_token_subscription(0).map(Message::Token),
            self.core
                .watch_config::<keyboard::Comp>(keyboard::COMP_ID)
                .map(|u| Message::Xkb(u.config.xkb_config)),
            self.core
                .watch_config::<mode::ThemeMode>(mode::ID)
                .map(|u| Message::State(Msg::DarkChanged(u.config.is_dark))),
            self.core
                .watch_config::<dnd::NotificationsConfig>(dnd::ID)
                .map(|u| Message::State(Msg::DndChanged(u.config.do_not_disturb))),
        ];
        if let Some(conn) = &self.session {
            subs.push(brightness::subscription(conn.clone()).map(Message::Brightness));
        }
        Subscription::batch(subs)
    }
}

// --- Drawing: the mock's blocks, tiles and pills ---------------------------

/// A glyph drawn in `color` (the accent's text colour on an "on" circle).
fn glyph<'a>(svg: &'static [u8], size: u16, on: bool) -> Element<'a, Message> {
    widget::icon(icon::from_svg_bytes(svg).symbolic(true))
        .class(theme::Svg::Custom(Rc::new(move |theme| iced::widget::svg::Style {
            color: Some(if on {
                theme.cosmic().accent.on.into()
            } else {
                theme.cosmic().background(theme.transparent).on.into()
            }),
        })))
        .width(Length::Fixed(f32::from(size)))
        .height(Length::Fixed(f32::from(size)))
        .into()
}

/// The round toggle: the accent when on, the neutral hover colour when off.
fn circle<'a>(svg: &'static [u8], on: bool) -> widget::Container<'a, Message, cosmic::Theme> {
    container(glyph(svg, GLYPH, on))
        .center(Length::Fixed(CIRCLE))
        .class(theme::Container::custom(move |theme| {
            let c = theme.cosmic();
            iced::widget::container::Style {
                background: Some(Background::Color(if on {
                    c.accent.base.into()
                } else {
                    c.background(theme.transparent).component.hover.into()
                })),
                border: Border { radius: (CIRCLE / 2.0).into(), ..Default::default() },
                ..Default::default()
            }
        }))
}

/// A rounded block on the popup, a step lighter than it, with a hairline.
fn block<'a>(content: impl Into<Element<'a, Message>>) -> widget::Container<'a, Message, cosmic::Theme> {
    container(content).padding(10).class(theme::Container::custom(block_style))
}

fn block_style(theme: &cosmic::Theme) -> iced::widget::container::Style {
    let c = theme.cosmic();
    let bg = c.background(theme.transparent);
    iced::widget::container::Style {
        background: Some(Background::Color(bg.component.base.into())),
        border: Border {
            radius: c.corner_radii.radius_m.into(),
            width: 1.0,
            color: bg.divider.into(),
        },
        text_color: Some(bg.on.into()),
        ..Default::default()
    }
}

/// A button with no look of its own but a faint hover, for rows and tiles.
fn plain_button<'a>(content: impl Into<Element<'a, Message>>) -> button::Button<'a, Message> {
    let style = |hover: bool| {
        move |_focused: bool, theme: &cosmic::Theme| {
            let c = theme.cosmic();
            let bg = c.background(theme.transparent);
            button::Style {
                shadow_offset: iced::Vector::default(),
                background: hover.then(|| Background::Color(bg.component.hover.with_alpha(0.5).into())),
                overlay: None,
                border_radius: c.corner_radii.radius_s.into(),
                border_width: 0.0,
                border_color: Color::TRANSPARENT,
                outline_width: 0.0,
                outline_color: Color::TRANSPARENT,
                icon_color: None,
                text_color: Some(bg.on.into()),
            }
        }
    };
    button::custom(content).padding(0).class(theme::Button::Custom {
        active: Box::new(style(false)),
        disabled: Box::new(move |theme| style(false)(false, theme)),
        hovered: Box::new(style(true)),
        pressed: Box::new(style(true)),
    })
}

fn title_and_state<'a>(title: &'static str, sub: String) -> Element<'a, Message> {
    column![
        text::body(title).font(cosmic::font::semibold()),
        text::caption(sub).class(theme::Text::Custom(|theme| {
            iced::widget::text::Style {
                color: Some(theme.cosmic().background(theme.transparent).on.with_alpha(0.65).into()),
                ..Default::default()
            }
        })),
    ]
    .into()
}

/// A row of the network block: the round toggle, then the name and state,
/// which open the detail (with a chevron to say so).
fn toggle_row<'a>(
    svg: &'static [u8],
    title: &'static str,
    sub: String,
    on: bool,
    toggle: Msg,
    detail: Detail,
) -> Element<'a, Message> {
    let open = plain_button(
        row![
            title_and_state(title, sub),
            widget::space::horizontal().width(Length::Fill),
            widget::icon(icon::from_name("go-next-symbolic").size(16).symbolic(true).into())
                .width(Length::Fixed(16.0))
                .height(Length::Fixed(16.0)),
        ]
        .align_y(Alignment::Center)
        .padding([2, 4]),
    )
    .width(Length::Fill)
    .on_press(Message::State(Msg::OpenDetail(detail)));
    row![
        plain_button(circle(svg, on)).on_press(Message::State(toggle)),
        open,
    ]
    .spacing(8)
    .align_y(Alignment::Center)
    .into()
}

/// A toggle tile (Dark style, Do not disturb): the whole tile toggles.
fn tile<'a>(svg: &'static [u8], title: &'static str, on: bool, toggle: Msg) -> Element<'a, Message> {
    // The tile is as tall as its half of the network block; its circle and
    // label sit in its middle, as in the mock (`.one`: align-items centre).
    // A button puts its content at the top, so a container centres it.
    button::custom(
        container(
            row![circle(svg, on), text::body(title).font(cosmic::font::semibold())]
                .spacing(8)
                .align_y(Alignment::Center),
        )
        .center_y(Length::Fill),
    )
    .padding(10)
    .width(Length::Fill)
    .height(Length::Fill)
    .class(theme::Button::Custom {
        active: Box::new(|_, theme| tile_style(theme, false)),
        disabled: Box::new(|theme| tile_style(theme, false)),
        hovered: Box::new(|_, theme| tile_style(theme, true)),
        pressed: Box::new(|_, theme| tile_style(theme, true)),
    })
    .on_press(Message::State(toggle))
    .into()
}

fn tile_style(theme: &cosmic::Theme, hover: bool) -> button::Style {
    let b = block_style(theme);
    let c = theme.cosmic();
    let bg = c.background(theme.transparent);
    button::Style {
        shadow_offset: iced::Vector::default(),
        background: Some(Background::Color(if hover {
            bg.component.hover.into()
        } else {
            bg.component.base.into()
        })),
        overlay: None,
        border_radius: b.border.radius,
        border_width: b.border.width,
        border_color: b.border.color,
        outline_width: 0.0,
        outline_color: Color::TRANSPARENT,
        icon_color: None,
        text_color: b.text_color,
    }
}

/// A labelled slider block: a pill filled up to the value, with the glyph
/// inside at its start (a mute button for Sound), as the mock's `.sl`.
///
/// The fill is drawn here, not by iced's slider: the slider insets its rail by
/// half a handle at each end and draws its handle over the fill, which showed
/// as a pale knob on the see-through fill. Here the slider only takes the
/// input (its rail and handle are transparent) and the fill is a pill from
/// the left end to just past the handle's centre:
/// `PILL + value * (width - PILL)`, made of a round left cap, a square middle
/// and a round right cap that do not overlap, so the alpha stays even.
fn slider_block<'a>(
    label: &'static str,
    svg: &'static [u8],
    value: f32,
    on_glyph: Option<Msg>,
    on_change: fn(f32) -> Message,
) -> Element<'a, Message> {
    let input = slider(0.0..=1.0, value, on_change)
        .step(0.01_f32)
        .height(PILL)
        .handle_width(PILL)
        .class(theme::iced::Slider::Custom {
            active: Rc::new(invisible_slider),
            hovered: Rc::new(invisible_slider),
            dragging: Rc::new(invisible_slider),
        });
    let (filled, rest) = fill_portions(value);
    let half = |left: bool| {
        container(widget::space::horizontal())
            .width(Length::Fixed(PILL / 2.0))
            .height(Length::Fixed(PILL))
            .class(theme::Container::custom(move |theme| pill_fill(theme, Some(left))))
    };
    let mut fill = row![half(true)];
    if filled > 0 {
        fill = fill.push(
            container(widget::space::horizontal())
                .width(Length::FillPortion(filled))
                .height(Length::Fixed(PILL))
                .class(theme::Container::custom(|theme| pill_fill(theme, None))),
        );
    }
    fill = fill.push(half(false));
    if rest > 0 {
        fill = fill.push(widget::space::horizontal().width(Length::FillPortion(rest)));
    }
    let track = container(widget::space::horizontal())
        .width(Length::Fill)
        .height(Length::Fixed(PILL))
        .class(theme::Container::custom(|theme| {
            let c = theme.cosmic();
            iced::widget::container::Style {
                background: Some(Background::Color(c.background(theme.transparent).component.hover.into())),
                border: Border { radius: (PILL / 2.0).into(), ..Default::default() },
                ..Default::default()
            }
        }));
    let mark: Element<'a, Message> = match on_glyph {
        Some(msg) => plain_button(container(glyph_dark(svg)).center(Length::Fixed(PILL)))
            .on_press(Message::State(msg))
            .into(),
        None => container(glyph_dark(svg)).center(Length::Fixed(PILL)).into(),
    };
    block(
        column![
            text::body(label).font(cosmic::font::semibold()),
            stack![track, fill, input, mark],
        ]
        .spacing(6),
    )
    .width(Length::Fill)
    .into()
}

/// How a pill's width beyond its two round caps splits between the fill and
/// the rest, in thousandths, for a value in 0..=1 (clamped). The fill ends at
/// the slider's handle centre plus half a pill, so it follows the pointer.
fn fill_portions(value: f32) -> (u16, u16) {
    let filled = (value.clamp(0.0, 1.0) * 1000.0).round() as u16;
    (filled, 1000 - filled)
}

/// The glyph on a pill: the popup's background colour, on a fill that is its
/// text colour, so the two swap between dark and light (at night the mock's
/// dark glyph on a light pill). 18 px, the size of the toggles' glyphs: at
/// 16 px the speaker's waves were too thin to read.
fn glyph_dark<'a>(svg: &'static [u8]) -> Element<'a, Message> {
    widget::icon(icon::from_svg_bytes(svg).symbolic(true))
        .class(theme::Svg::Custom(Rc::new(|theme| iced::widget::svg::Style {
            color: Some(theme.cosmic().background(theme.transparent).base.with_alpha(1.0).into()),
        })))
        .width(Length::Fixed(f32::from(GLYPH)))
        .height(Length::Fixed(f32::from(GLYPH)))
        .into()
}

/// A piece of the pill's fill: the popup's text colour, a little see-through
/// as in the mock (light at night, dark by day). `Some(true)` is the round
/// left cap, `Some(false)` the round right cap, `None` the square middle.
fn pill_fill(theme: &cosmic::Theme, cap: Option<bool>) -> iced::widget::container::Style {
    let c = theme.cosmic();
    let r = PILL / 2.0;
    let radius = match cap {
        Some(true) => iced::border::Radius { top_left: r, bottom_left: r, top_right: 0.0, bottom_right: 0.0 },
        Some(false) => iced::border::Radius { top_left: 0.0, bottom_left: 0.0, top_right: r, bottom_right: r },
        None => 0.0.into(),
    };
    iced::widget::container::Style {
        background: Some(Background::Color(c.background(theme.transparent).on.with_alpha(0.88).into())),
        border: Border { radius, ..Default::default() },
        ..Default::default()
    }
}

/// The slider takes the input only: nothing of it is drawn.
fn invisible_slider(_theme: &cosmic::Theme) -> iced::widget::slider::Style {
    let none = Background::Color(Color::TRANSPARENT);
    iced::widget::slider::Style {
        rail: iced::widget::slider::Rail {
            backgrounds: (none, none),
            width: PILL,
            border: Border::default(),
        },
        handle: iced::widget::slider::Handle {
            shape: iced::widget::slider::HandleShape::Rectangle {
                width: PILL as u16,
                height: PILL as u16,
                border_radius: (PILL / 2.0).into(),
            },
            background: none,
            border_width: 0.0,
            border_color: Color::TRANSPARENT,
        },
        breakpoint: iced::widget::slider::Breakpoint { color: Color::TRANSPARENT },
    }
}

#[cfg(test)]
mod tests {
    use super::fill_portions;

    #[test]
    fn the_fill_splits_the_pill_by_the_value() {
        assert_eq!(fill_portions(0.0), (0, 1000));
        assert_eq!(fill_portions(0.62), (620, 380));
        assert_eq!(fill_portions(1.0), (1000, 0));
        // Out of range (an output above 100%) is shown full, never wider.
        assert_eq!(fill_portions(1.4), (1000, 0));
        assert_eq!(fill_portions(-0.1), (0, 1000));
    }
}
