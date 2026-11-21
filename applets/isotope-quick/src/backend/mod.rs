//! The system side of the quick settings: one module per thing it shows.
//! Each reads the system into a [`crate::state::Msg`] (a subscription) and
//! carries out the [`crate::state::Effect`]s that concern it. They are kept
//! thin: what can be decided without the system is a pure function here,
//! tested, or in `state.rs`.

pub mod audio;
pub mod bluetooth;
pub mod brightness;
pub mod dnd;
pub mod keyboard;
pub mod mode;
pub mod network;

use std::time::Duration;

/// How long a backend waits before it tries a missing service again.
pub const RETRY: Duration = Duration::from_secs(5);
