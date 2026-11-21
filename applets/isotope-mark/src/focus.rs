//! The focused window, from the toplevel info protocols (ext-foreign-toplevel-list
//! and cosmic-toplevel-info), the way the stock app list and minimize applets
//! read them: a Wayland connection of its own on a thread, on the privileged
//! socket cosmic-panel hands an applet whose desktop file has
//! `X-HostWaylandDisplay`.

use std::os::{
    fd::{FromRawFd, RawFd},
    unix::net::UnixStream,
};

use cosmic::{
    cctk::{
        self,
        cosmic_protocols::toplevel_info::v1::client::zcosmic_toplevel_handle_v1::State,
        sctk::{
            self,
            reexports::{calloop, calloop_wayland_source::WaylandSource},
            registry::{ProvidesRegistryState, RegistryState},
        },
        toplevel_info::{ToplevelInfoHandler, ToplevelInfoState},
        wayland_client::{Connection, QueueHandle, globals::registry_queue_init},
        wayland_protocols::ext::foreign_toplevel_list::v1::client::ext_foreign_toplevel_handle_v1::ExtForeignToplevelHandleV1,
    },
    iced::{
        self, Subscription,
        futures::{self, SinkExt, channel::mpsc},
        stream,
    },
};

/// The focused window: its app ID and title, both empty when no window has
/// the focus (the desktop).
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Focus {
    pub app_id: String,
    pub title: String,
}

/// A new [`Focus`] each time the focused window, or its title, changes.
pub fn subscription() -> iced::Subscription<Focus> {
    Subscription::run_with(std::any::TypeId::of::<Focus>(), |_| {
        stream::channel(1, move |output: mpsc::Sender<Focus>| async move {
            let _ = std::thread::spawn(move || {
                if let Err(err) = watch(output) {
                    tracing::error!("cannot follow the focused window: {err}");
                }
            });
            futures::future::pending().await
        })
    })
}

struct AppData {
    tx: mpsc::Sender<Focus>,
    registry_state: RegistryState,
    toplevel_info_state: ToplevelInfoState,
    sent: Option<Focus>,
}

impl AppData {
    /// Sends the activated toplevel, if it changed. `closing` is a toplevel
    /// that is going away but is still in the list.
    fn send_focus(&mut self, closing: Option<&ExtForeignToplevelHandleV1>) {
        let focus = self
            .toplevel_info_state
            .toplevels()
            .filter(|t| Some(&t.foreign_toplevel) != closing)
            .find(|t| t.state.contains(&State::Activated))
            .map(|t| Focus {
                app_id: t.app_id.clone(),
                title: t.title.clone(),
            })
            .unwrap_or_default();
        if self.sent.as_ref() != Some(&focus) {
            self.sent = Some(focus.clone());
            let _ = futures::executor::block_on(self.tx.send(focus));
        }
    }
}

impl ProvidesRegistryState for AppData {
    fn registry(&mut self) -> &mut RegistryState {
        &mut self.registry_state
    }

    sctk::registry_handlers!();
}

impl ToplevelInfoHandler for AppData {
    fn toplevel_info_state(&mut self) -> &mut ToplevelInfoState {
        &mut self.toplevel_info_state
    }

    fn new_toplevel(&mut self, _: &Connection, _: &QueueHandle<Self>, _: &ExtForeignToplevelHandleV1) {
        self.send_focus(None);
    }

    fn update_toplevel(&mut self, _: &Connection, _: &QueueHandle<Self>, _: &ExtForeignToplevelHandleV1) {
        self.send_focus(None);
    }

    fn toplevel_closed(&mut self, _: &Connection, _: &QueueHandle<Self>, toplevel: &ExtForeignToplevelHandleV1) {
        self.send_focus(Some(toplevel));
    }
}

sctk::delegate_registry!(AppData);
cctk::delegate_toplevel_info!(AppData);

/// Sets close-on-exec on `socket`, keeping its other fd flags.
fn close_on_exec(socket: &impl std::os::fd::AsFd) -> rustix::io::Result<()> {
    let flags = rustix::io::fcntl_getfd(socket)?;
    rustix::io::fcntl_setfd(socket, flags | rustix::io::FdFlags::CLOEXEC)
}

/// Sets close-on-exec on the privileged socket, if the panel passed one. Called
/// from main() before the applet runs, so no click can spawn isotope-menu with
/// the fd still inheritable; `watch` sets it again, which does no harm.
pub fn close_privileged_socket_on_exec() {
    let Some(fd) = std::env::var("X_PRIVILEGED_WAYLAND_SOCKET")
        .ok()
        .and_then(|fd| fd.parse::<RawFd>().ok())
    else {
        return;
    };
    // SAFETY: cosmic-panel passes this fd to the applet and keeps it open; the
    // borrow ends with this call.
    let fd = unsafe { std::os::fd::BorrowedFd::borrow_raw(fd) };
    if let Err(err) = close_on_exec(&fd) {
        tracing::warn!("cannot set close-on-exec on the privileged socket: {err}");
    }
}

/// Follows the focused window until the connection or the applet goes away.
fn watch(tx: mpsc::Sender<Focus>) -> Result<(), Box<dyn std::error::Error>> {
    let socket = std::env::var("X_PRIVILEGED_WAYLAND_SOCKET")
        .ok()
        .and_then(|fd| fd.parse::<RawFd>().ok())
        // SAFETY: cosmic-panel passes this fd to the applet for this use only.
        .map(|fd| unsafe { UnixStream::from_raw_fd(fd) });
    if let Some(socket) = &socket {
        // cosmic-panel hands the fd over without close-on-exec: set it, so
        // isotope-menu and whatever it starts do not inherit a privileged
        // connection to the compositor.
        close_on_exec(socket)?;
    }
    let conn = match socket {
        Some(socket) => Connection::from_socket(socket)?,
        None => Connection::connect_to_env()?,
    };
    let (globals, event_queue) = registry_queue_init(&conn)?;
    let qh = event_queue.handle();
    let mut event_loop = calloop::EventLoop::<AppData>::try_new()?;
    WaylandSource::new(conn, event_queue)
        .insert(event_loop.handle())
        .map_err(|e| e.error)?;

    let registry_state = RegistryState::new(&globals);
    let toplevel_info_state = ToplevelInfoState::try_new(&registry_state, &qh)
        .ok_or("the compositor offers no toplevel info to this client")?;
    let mut app_data = AppData {
        tx,
        registry_state,
        toplevel_info_state,
        sent: None,
    };
    // Show the desktop's empty name until a window reports in.
    app_data.send_focus(None);
    loop {
        event_loop.dispatch(None, &mut app_data)?;
        if app_data.tx.is_closed() {
            return Ok(());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use rustix::io::{FdFlags, fcntl_getfd, fcntl_setfd};

    #[test]
    fn privileged_socket_is_closed_on_exec() {
        let (socket, _peer) = UnixStream::pair().unwrap();
        // As cosmic-panel hands it over: inheritable.
        fcntl_setfd(&socket, FdFlags::empty()).unwrap();
        assert!(!fcntl_getfd(&socket).unwrap().contains(FdFlags::CLOEXEC));
        close_on_exec(&socket).unwrap();
        assert!(fcntl_getfd(&socket).unwrap().contains(FdFlags::CLOEXEC));
    }
}
