# Agent appliance: first-boot Chromium

Date: 2026-07-23
Status: draft for review, not yet planned

Give the agent appliance a working browser automatically. The Cua agent's
`browser` tool drives a Chromium instance over the Chrome DevTools Protocol on
`127.0.0.1:9222`, but **nothing in the shipping image installs or launches that
browser today** — it is assumed a human already started one. This feature closes
that gap: install Chromium on first boot and keep it running with the debug port
open, so the `browser` tool works out of the box on a fresh appliance.

## Motivation

`agent/internal/cua/browser.go` connects to a browser on `browserCDPPort = 9222`
and errors when none is present (`"browser: no browser window is open; start one,
then retry"`). A grep across the shipping overlays (`rootfs/`, `rootfs-agent/`)
for `9222`, `remote-debugging-port`, `chromium`, `flatpak run` returns nothing —
every match is under `test/`. So the appliance ships an agent that can drive a
browser and no browser for it to drive. The install recipe is already proven, but
only in the test harness (`test/agent/run.sh`, `test/agent/Dockerfile.compat`).

## Scope

In scope: installing Chromium on first boot, persisting it, launching it with the
debug port, and keeping it alive — for the **agent appliance only** (guarded off
every other variant).

Out of scope: the desktop (sway/i3) variants — they intentionally ship no browser
and get one from Flathub per user on demand. Any change to the `browser` tool's
CDP client. A general app-install framework.

## Established facts this builds on

From the agent overlay and agent Go code (all verified, not assumed):

- **The `agent` user is fixed**: UID 1000, created in the cos-setup `boot` stage
  by `rootfs-agent/system/oem/90_agent_profile.yaml`, password-locked,
  linger-enabled (`loginctl enable-linger agent`), autologin to i3 via ly. The
  desktop session and Cua run **as `agent`** on the lingering user bus
  (`/run/user/1000/bus`), not as root.
- **The profile marker** `/etc/hadron-agent/profile` exists only on the appliance;
  every agent unit gates on `ConditionPathExists=/etc/hadron-agent/profile`, which
  is a hard no-op (dangling `.wants` symlink) on any other image.
- **The `browser` tool only connects**; it never launches. Cua finds the window
  via `list_windows` and talks CDP on loopback 9222. Nothing gates on 9222 today.
- **`hadron-user-setup`** already configures a signed `--user` Flathub remote
  (fingerprint `6E5C05D979C76DAF93C081354184DD4D907A7CAE`,
  `https://dl.flathub.org/repo/`) for every UID 1000–59999 user, so `agent` has a
  `--user` remote. It installs no apps.
- **First-boot oneshot convention** (`hadron-agent-provision.service`): profile
  gate, `Wants=`/`After=network-online.target`,
  `After=cos-setup-boot.service cos-setup-network.service` (the `agent` user is
  created in the boot stage), `Type=oneshot` + `RemainAfterExit=yes`, run-once via
  a filesystem marker on a persisted path.
- **Persistence is opt-in**: `/var/lib` is ephemeral on immutable Kairos.
  `rootfs-agent/system/oem/93_agent_persistence.yaml` persists
  `/var/lib/hadron-agent` by read-modify-write appending to `CUSTOM_BIND_MOUNTS`
  in `/run/cos/cos-layout.env` (sorted after `92_docker.yaml`'s
  `/var/lib/docker`). Nothing persists `/home` or `/var/lib/flatpak` today.
- **The proven recipe** (`test/agent/Dockerfile.compat`): add a signed `--system`
  Flathub remote, `flatpak install --system … flathub org.chromium.Chromium`,
  launch with `--no-sandbox --disable-gpu --disable-dev-shm-usage
  --ozone-platform=x11 --remote-debugging-port=9222`.

## Design decisions (open questions the recon left; flagged for review)

These are the choices this feature had to make. Each has a real alternative;
they are called out so they can be overruled at review.

### D1 — System install, not per-user

Install `org.chromium.Chromium` **system-wide** (`flatpak --system`,
`/var/lib/flatpak`), run from a **root** system oneshot.

*Rationale:* it matches the existing first-boot unit pattern exactly (provision,
autoinstall are root system oneshots) with no `su - agent` / `machinectl shell`
gymnastics and no dependency on a running user bus at install time. A system
install is visible to the agent user's `flatpak run` on loopback regardless. The
recipe is already proven `--system` in `Dockerfile.compat`.

*Alternative:* a `--user` install for `agent`, reusing the remote
`hadron-user-setup` already configures. Rejected because it must run as the agent
user with a live user manager, and its persistence depends on `/home` surviving
reboots — which this repo does not configure and I could not confirm is a Kairos
default. System install lets us make persistence explicit (D2).

### D2 — Persist `/var/lib/flatpak` explicitly

Add `/var/lib/flatpak` to `CUSTOM_BIND_MOUNTS`, mirroring
`93_agent_persistence.yaml`'s read-modify-write so it does not clobber the Docker
or hadron-agent entries. Without this the ~2 GB install repeats on every boot.

*This is the deciding factor for install-once vs install-every-boot.* With it, the
install oneshot is marker-gated and runs once; the marker lives on the persisted
`/var/lib/hadron-agent`.

### D3 — Track `stable`, do not pin a commit (in production)

Production installs `org.chromium.Chromium` tracking the `stable` branch and does
**not** pin to an OSTree commit.

*Rationale:* the appliance installs whenever it first boots — possibly months
after the image was built. Flathub garbage-collects old commits, so a pinned
commit can become uninstallable and fail first boot. Tracking `stable` is always
installable.

*Trade-off, and the reason this is flagged:* it sacrifices fleet reproducibility
— two appliances first-booting weeks apart may get different Chromium versions.
The **test harness keeps its pin** (`test/agent/fixtures/chromium.commit`) for
deterministic CI; production and test deliberately diverge here. If reproducible
fleet images matter more than first-boot resilience, this decision flips to a
build-arg-supplied pin.

### D4 — Launch via a `systemd --user` unit with `Restart=always`

A per-user unit `hadron-agent-browser.service` (`ConditionUser=agent`) runs
`flatpak run … --remote-debugging-port=9222` and restarts on crash.

*Rationale:* on an unattended appliance a dead browser means a dead agent (the
tool errors with no browser on 9222). `Restart=always` makes that self-healing.
It ties into the session the same way Cua does.

*Alternative:* an i3 `exec` line, exactly like the test fixture
`test/agent/rootfs/etc/i3/config.d/99-cua-compat.conf`. Rejected because i3 `exec`
does not restart a crashed process.

## Architecture

Three units plus one persistence stage, all guarded off non-agent images.

### 1. Install — `hadron-agent-browser-install.service` (root, system)

`rootfs-agent/etc/systemd/system/hadron-agent-browser-install.service`, enabled by
a committed `multi-user.target.wants` symlink (the profile gate makes it a no-op
elsewhere).

- `ConditionPathExists=/etc/hadron-agent/profile`
- `ConditionPathExists=!/var/lib/hadron-agent/browser/installed` (run-once marker
  on the persisted path)
- `Wants=network-online.target`,
  `After=network-online.target cos-setup-boot.service cos-setup-network.service`
- `Type=oneshot`, `RemainAfterExit=yes`, generous `TimeoutStartSec` (the download
  is ~2 GB — several minutes on a slow link)
- `ExecStart=/usr/bin/hadron-agent-browser-install`
- `WantedBy=multi-user.target`

The script (`rootfs-agent/usr/bin/hadron-agent-browser-install`):
1. Re-assert the profile marker and bail cleanly if absent.
2. Add the signed `--system` Flathub remote if not present (same fingerprint and
   URL `hadron-user-setup` uses; reuse its descriptor file).
3. `flatpak install --system --noninteractive -y flathub org.chromium.Chromium`
   (branch `stable`, per D3).
4. On success, write `/var/lib/hadron-agent/browser/installed` so the unit is a
   no-op on subsequent boots.

Failure is non-fatal to boot: the unit is a leaf on `multi-user.target`, and the
marker is only written on success, so a network outage means it retries next boot
while the appliance stays usable (the browser tool just errors until then).

### 2. Persistence — `94_agent_browser_persistence.yaml`

`rootfs-agent/system/oem/94_agent_browser_persistence.yaml`, sorted after
`93_agent_persistence.yaml`, appends `/var/lib/flatpak` to `CUSTOM_BIND_MOUNTS`
with the same read-modify-write guard so it composes with the Docker and
hadron-agent entries rather than replacing them. Guarded on the profile marker.

### 3. Launch — `hadron-agent-browser.service` (user, `agent`)

`rootfs-agent/usr/lib/systemd/user/hadron-agent-browser.service`,
`ConditionUser=agent`.

- `After=hadron-agent-session.service`, `PartOf=hadron-agent-session.service`,
  `WantedBy=hadron-agent-session.service` — so it inherits the DISPLAY/XAUTHORITY/
  DBUS environment `hadron-agent-session-ready` imports, and restarts whenever the
  session does (that script already `systemctl --user restart`s the session).
- `ExecStart=/usr/bin/hadron-agent-browser-run` — a thin wrapper around
  `flatpak run … org.chromium.Chromium` with the test-proven flags:
  `--ozone-platform=x11 --no-sandbox --disable-gpu --disable-dev-shm-usage
  --no-first-run --remote-debugging-port=9222 --remote-allow-origins=*`.
- `Restart=always`, `RestartSec` a few seconds.
- A `ConditionPathExists=/var/lib/hadron-agent/browser/installed` so it does not
  thrash-restart before the install has ever succeeded.

Whether the launch unit needs to be explicitly (re)started by
`hadron-agent-session-ready` — alongside its existing session restart — or whether
`PartOf`/`WantedBy` on the session is sufficient, is an implementation detail to
settle when the ordering is testable; the intent is "starts once the display env
is live, restarts with the session, restarts on crash."

## Readiness (deliberately minimal)

The `browser` tool already degrades gracefully when 9222 is absent, so this
feature does **not** add a fleet-wide readiness gate in its first cut (YAGNI). The
launch wrapper may optionally wait for 9222 to accept a connection before
considering itself started, so `systemctl --user status` reflects reality. A
`browser` field in the `status.json` contract documented in
`hadron-agent-first-run` is a natural later addition but is not required for the
feature to work, and is left out of scope here.

## Requirements and constraints

- **Disk**: Chromium + the freedesktop runtime is ~2 GB; the appliance install
  target needs headroom (the test README cites ≥32 GB guest disk). Worth a note in
  the appliance install docs, not a code change.
- **Network on first boot**: required for the install; handled by retry-on-next-
  boot, never blocks boot.
- **Signed remote only**: the `--system` remote reuses the same GPG fingerprint
  and URL as `hadron-user-setup`; no unsigned remote is ever added.
- **Guarded off non-agent images**: every unit gates on
  `/etc/hadron-agent/profile`; the persistence stage guards on it too.
- **No change to the agent's audited safety surface**: this feature adds units and
  scripts; it does not touch `hadron-agent-install`, the provision/seed path, or
  the gateway.

## Testing

- **Shell unit tests** in the `test/agent/` style for the install script: profile
  gate is a no-op off-appliance; marker present ⇒ no-op; remote-add is idempotent;
  the install command is the injectable seam (mirror the `HADRON_INSTALL_CMD`
  pattern so tests assert did/didn't-install without pulling 2 GB).
- **A persistence assertion** that `94_…` appends rather than replaces
  `CUSTOM_BIND_MOUNTS` (the exact bug `93_` was written to avoid).
- **The existing `ui` E2E gate** (`test/agent/run.sh`) already installs and drives
  Chromium on 9222; extend or reuse it to assert the *shipping* units bring the
  browser up on a booted appliance, rather than the test harness doing the install
  itself. This is the one check that proves the feature end to end.
- **CI**: these live under `test/agent/**`, already covered by `agent-gates`.

## Risks

| Risk | Mitigation |
|---|---|
| Flathub unreachable on first boot | Marker written only on success; retries next boot; boot never blocked |
| `/var/lib/flatpak` persistence entry clobbers Docker/hadron-agent entries | Read-modify-write append, guarded, sorted after `93_` — same pattern already proven |
| `stable` drift across a fleet (D3) | Accepted trade for install resilience; test harness stays pinned; flip to a build-arg pin if reproducibility is required |
| Browser crashes and the agent goes dark | `Restart=always` on the user unit |
| 2 GB install on a small disk | Documented disk requirement; install failure is non-fatal |
| `flatpak run` lacks the display env | Launch unit ordered after `hadron-agent-session-ready`'s env import, `PartOf` the session |

## References

- `test/agent/Dockerfile.compat` — the proven `--system` install + pin recipe
- `test/agent/run.sh` (the `ui` gate) — the `--remote-debugging-port=9222` launch
- `test/agent/rootfs/etc/i3/config.d/99-cua-compat.conf` — the exact launch flags
- `rootfs-agent/system/oem/93_agent_persistence.yaml` — the `CUSTOM_BIND_MOUNTS`
  read-modify-write pattern to mirror
- `rootfs-agent/etc/systemd/system/hadron-agent-provision.service` — the first-boot
  oneshot conventions
- `rootfs/usr/bin/hadron-user-setup` — the signed Flathub remote (fingerprint/URL)
- `agent/internal/cua/browser.go` — `browserCDPPort = 9222`, connect-only
