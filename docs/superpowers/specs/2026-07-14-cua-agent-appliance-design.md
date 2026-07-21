# Cua-controlled Hadron agent appliance

**Date:** 2026-07-14

**Status:** Design approved; pending implementation plan

**Scope:** Add an opt-in Hadron agent appliance ISO, derived from the existing
XLibre/i3 desktop flavor, that exposes a small authenticated MCP interface for
foreground computer use, direct shell execution, process management, and
filesystem access. The normal Sway and XLibre/i3 desktop ISOs remain separate
and unchanged in behavior.

## Context

Hadron Desktop now builds two musl-based graphical variants: Sway/Wayland and
i3 on XLibre. We want a third, deliberately agent-enabled appliance that a
human can boot in QEMU or on physical hardware, watch and operate locally, and
also control from an external agent.

The graphical automation must target the same real desktop shown on the
virtio-gpu/DRM output. In QEMU, the VNC or noVNC display must show the exact
session that Cua controls. A hidden Xvfb, nested desktop, or separate VNC-only
session would violate this requirement.

The design was checked against:

- Hadron Desktop `41d4d6b`, including the XLibre/i3 flavor merged on
  2026-07-14.
- `trycua/cua` commit `3cadb5f82e7d2ed071a2082764276ec872a52135`,
  reviewed on 2026-07-14.
- Cua Driver's native Linux X11 and Wayland backends.
- Cua computer-server's REST, WebSocket, and MCP modes.
- Hermes Agent's compact computer-use, terminal, process, and file-tool shape.

## Goals

1. Produce a separate `hadron-desktop-agent` ISO; agent control is never
   enabled implicitly in the ordinary desktop images.
2. Boot an unprivileged `agent` account automatically into a visible XLibre/i3
   session on the real seat.
3. Expose a standard MCP Streamable HTTP endpoint on a private/LAN interface,
   protected by HTTPS and bearer-token authentication.
4. Expose exactly seven public tools:
   `computer_use`, `bash`, `read_file`, `search_files`, `write_file`,
   `patch`, and `browser`. (`browser` was added by the
   2026-07-20 amendment; see `specs/2026-07-20-browser-tool-amendment.md`.)
5. Execute normal shell and filesystem operations as the same unprivileged
   `agent` user that owns the desktop session.
6. Support an optional, separately provisioned administrator bearer for
   privileged terminal, process, and filesystem operations.
7. Support safe interactive installation and declarative zero-touch
   installation for CI and fleet use.
8. Provide a reusable visible-VM fixture so Hadron and downstream UI projects
   can write computer-use E2E tests and make them release gates.

## Non-goals

- Adding agent control to the normal Sway or i3 ISO.
- Exposing Cua Driver's complete internal tool catalog.
- Exposing Cua computer-server's unauthenticated REST or WebSocket API.
- Running an agent model or planning loop inside the appliance.
- Supporting arbitrary raw background input as a v1 contract.
- Holding a key down. `computer_use key` is an atomic tap: cua-driver exposes no
  key-down/key-up pair, so hold-to-act interfaces (real-time games,
  press-and-hold controls) are out of scope. See
  `specs/2026-07-20-browser-tool-amendment.md`.
- Treating bearer tokens or application-level tool filtering as the security
  boundary. The operating-system user and VM remain the primary boundary.
- Providing hostile-code containment after the optional administrator token
  has deliberately granted root access.

## Display-stack decision

The agent profile is built on the XLibre/i3 flavor, not Sway.

Cua Driver classifies Linux X11 as supported and Sway as supported with
compositor-specific limits. Its X11 backend uses established X11 mechanisms:

- EWMH properties for window discovery, stacking, and activation.
- XGetImage for desktop and window capture.
- XTest for foreground pointer and keyboard delivery.
- XSendEvent and XInput2/MPX for supported background paths.
- AT-SPI over the session D-Bus for accessibility trees and semantic actions.

The current Hadron i3 flavor already provides a real XLibre server, i3's EWMH
window management, X11 Mesa/GLX, the XLibre libinput driver, `libX11`, `libXi`,
Xauthority handling, and a session D-Bus. Its environment also directs GTK,
Qt, Mozilla, and SDL applications to X11.

XLibre intends to retain X11 backward compatibility, so Cua should interact
with it as an ordinary X11 server. Cua does not currently name XLibre as an
accepted upstream validation lane; therefore compatibility is an explicit
Hadron release gate rather than an assumption.

The agent overlay must add the compatibility pieces absent from the current
i3 image:

- A Cua Driver binary built natively against Hadron's musl toolchain. The
  upstream glibc Linux release is not copied into the image.
- `libXtst`; the build copies every other native client dependency selected by
  the pinned Cua build, and the image gate rejects unresolved dynamic links.
- The AT-SPI bus, registry, and toolkit bridge runtime needed for GTK, Qt,
  Chromium, and Electron applications to publish accessibility trees.

V1 defaults to foreground EWMH + XTest delivery. It does not grant the agent
access to `/dev/uinput` merely to expose Cua's more complex MPX paths.

## Architecture

The appliance has three persistent components and one optional component.

### Foreground desktop and Cua Driver

The installer creates a locked-password `agent` account. It is a member of the
groups needed for the graphical seat, audio, and Bluetooth, but is not a member
of `admin`, `sudo`, or `docker`.

`ly` auto-logs this account into the XLibre/i3 session on tty1. `ly` owns the
real X server lifecycle and supplies `DISPLAY` and `XAUTHORITY`; the session
launcher creates or joins the session D-Bus and imports all three values into
the `agent` user manager. The broker then starts Cua Driver with that session
environment instead of scraping another process's environment.

Cua Driver uses its native X11 backend. Native Wayland mode is not enabled.
The session broker owns Cua Driver as a child process and communicates through
MCP stdio. The raw driver therefore has no listening socket at all.

The public `computer_use` adapter defaults state-changing pixel operations to
Cua's foreground-delivery route. The appliance is a dedicated agent desktop,
so activating the intended window is both observable and more reliable across
toolkits than synthetic background delivery.

### Unprivileged session broker

`hadron-agent-session` is a persistent systemd user service running as
`agent`. The account's user manager is allowed to linger so shell and file
operations do not depend on compositor health. When i3 starts, the session
launcher imports `DISPLAY`, `XAUTHORITY`, and `DBUS_SESSION_BUS_ADDRESS` into
the user manager and starts or refreshes the broker. The broker then spawns Cua
Driver over stdio and owns that child connection.

The broker implements the seven public tool semantics for the ordinary bearer:

- `computer_use` maps the unified Hadron action schema to the smaller set of
  Cua Driver calls needed for capture, accessibility, application focus, and
  input.
- `terminal`, `process`, and all file tools execute with UID/GID and home
  directory belonging to `agent`.
- Commands and long-running processes are placed in tracked scopes or cgroups
  so they can be polled, terminated, and stopped by the emergency control.
- Computer-use actions are serialized per seat. Shell and file calls may run
  concurrently within configured resource limits.

The broker remains available when XLibre or Cua Driver is temporarily
unavailable. Terminal and file operations continue, while `computer_use`
returns the typed `SESSION_UNAVAILABLE` error until the visible desktop and
driver reconnect.

### Network MCP gateway

`hadron-agent-gateway` is a small musl-compatible native service running under
its own service identity. It binds the configured HTTPS address, normally
`0.0.0.0:7443`, and exposes:

- `/mcp` for MCP Streamable HTTP.
- `/healthz` for process liveness.
- `/readyz` for full agent-session readiness.

The health endpoints require no bearer and reveal only component booleans and
the appliance version; they never reveal host configuration, tokens, process
details, or tool arguments.

The gateway handles TLS, standard MCP transport, ordinary and administrator
bearer validation, schema validation, request and response limits, connection
limits, and audit metadata. It does not execute shell commands, access the
desktop, mount the Docker socket, or run as root. Ordinary calls are forwarded
to the session broker through a permissioned Unix socket under
`/run/hadron-agent/`.

When enabled, mDNS advertises `_hadron-agent._tcp.local` and the desktop shows
the canonical endpoint, for example:

```text
https://hadron-a1b2c3.local:7443/mcp
```

There is no public pass-through to Cua Driver, no generic REST action endpoint,
and no public WebSocket action API in v1.

### Optional privileged helper

`hadron-agent-root-helper` is not enabled unless an administrator credential
is explicitly provisioned. It implements only `terminal`, `process`,
`read_file`, `search_files`, `write_file`, and `patch`; an authenticated
administrator `computer_use` call is routed to the unprivileged session broker
and always targets the visible `agent` session.

The helper validates the raw administrator bearer independently against its
token digest instead of trusting a scope assertion from the gateway. The
gateway cannot convert an ordinary bearer into a privileged request merely by
choosing the administrator Unix socket. The helper accepts only the fixed tool
schemas and runs privileged commands in tracked scopes.

## Request flow

Ordinary graphical and OS calls follow these paths:

```text
MCP client
  -> HTTPS + ordinary bearer
  -> hadron-agent-gateway
  -> permissioned broker Unix socket
  -> hadron-agent-session
  -> Cua Driver
  -> visible XLibre/i3 session

MCP client
  -> HTTPS + ordinary bearer
  -> hadron-agent-gateway
  -> hadron-agent-session
  -> shell/process/filesystem as agent
```

An explicitly privileged call follows:

```text
MCP client
  -> HTTPS + administrator bearer
  -> hadron-agent-gateway
  -> raw bearer + request over root-helper Unix socket
  -> independent administrator-token validation
  -> privileged shell/process/filesystem operation
```

## Public MCP contract

The gateway advertises exactly these tools:

| Tool | Contract |
|---|---|
| `computer_use` | Unified capture, accessibility/SOM, click, drag, scroll, type, key, wait, list-applications, and focus-application actions. |
| `bash` | Execute a shell script to completion. No timeout and no output cap; write your own `timeout` into the script if you need one. |
| `read_file` | Read file content or metadata with explicit offset and size limits. |
| `search_files` | Search file names or contents below a supplied path with bounded results. |
| `write_file` | Create or replace file content. |
| `patch` | Apply a structured patch to one or more existing files. |
| `browser` | Drive web content by ref from the page's own accessibility information: navigate, snapshot, click, type, press, scroll, back, text. |

The same seven names are visible for both credential classes. Credential scope
determines the OS identity used for terminal, process, and filesystem tools.
`computer_use` and `browser` are the exceptions: both always execute in the
unprivileged session broker, so an administrator bearer never obtains a
root-owned desktop or browser.
Results include the execution identity where it helps prevent confusion.

The OS permission model is the filesystem boundary. A separate path allowlist
would not be a meaningful security boundary because the same credential has
direct shell execution. Request limits still bound content size, execution
time, output volume, open processes, and concurrent operations.

Computer-use operations return structured errors such as
`SESSION_UNAVAILABLE`, `INVALID_ARGUMENT`, `DEADLINE_EXCEEDED`, and
`OUTPUT_TRUNCATED`. A failed or disconnected state-changing action is not
automatically replayed.

## Authentication and transport security

- LAN-facing MCP is HTTPS-only. Plain HTTP requires an explicit development
  flag and is restricted to loopback.
- Ordinary and administrator tokens are independent, random 256-bit bearer
  credentials with the `hdn_u_` and `hdn_a_` prefixes, respectively.
- Only token digests are stored. High-entropy token digests are compared in
  constant time.
- The gateway receives read-only copies of the configured ordinary and
  administrator digests. When the root helper is enabled, it receives its own
  root-only copy of the administrator digest and revalidates the raw bearer.
- Token values are not accepted through Docker build arguments, embedded in a
  generic ISO, printed to the journal, or included in audit records.
- Rotation allows a bounded overlap in which the previous and new digest are
  both accepted, followed by explicit retirement of the previous token.
- The generic appliance generates its TLS private key on the machine. The
  foreground first-run panel shows the certificate fingerprint. Managed CI or
  fleet deployments may provision trust from their own CA.

The ordinary bearer intentionally grants powerful shell and file access within
the appliance. Its containment boundary is the unprivileged account and VM,
not a denylist. The administrator bearer deliberately removes that OS-level
boundary and must be treated as root-equivalent.

## ISO and installation behavior

The release produces three independent artifacts:

```text
hadron-desktop-sway-<version>-amd64.iso
hadron-desktop-i3-<version>-amd64.iso
hadron-desktop-agent-<version>-amd64.iso
```

The agent artifact is an additive overlay on the XLibre/i3 image. It contains
the pinned Cua Driver build, accessibility runtime, broker, gateway, optional
root-helper binary, service definitions, and agent-specific OEM configuration.
The ordinary i3 image does not inherit these files or services.

The generic agent ISO contains no machine-specific bearer or TLS private key.
Its live OEM stage creates an ephemeral locked `agent` account and its default
non-destructive path boots the visible agent session without writing a disk.
An explicit install path uses the existing Hadron disk-selection and
wipe-confirmation flow, creates the persistent `agent` account and credentials,
then installs the profile and boots it automatically on restart.

Zero-touch installation is activated only by declarative provisioning. A CI
or fleet seed names the installation device and explicitly opts into automatic
installation; the ISO does not guess that disk wiping is authorized.

## CI build and provisioning contract

The repository exposes an explicit agent build entry point while retaining the
existing desktop selector. The intended user-facing shape is:

```sh
make DESKTOP=sway iso
make DESKTOP=i3 iso
make agent-iso VERSION=v1.2.3
```

The agent target first builds or reuses the i3 image, then applies a separate
`Dockerfile.agent` overlay. Cua is pinned by immutable source revision and
built for musl. Release metadata records the Hadron base digest, XLibre
version, Cua revision, and final OCI image digest.

Per-machine state is supplied separately through a Kairos cloud-config seed.
The custom `hadron_agent` section is the stable provisioning contract:

```yaml
#cloud-config
hostname: hadron-ci-1842

install:
  auto: true
  device: /dev/vda
  reboot: true

hadron_agent:
  enabled: true
  endpoint:
    listen: "0.0.0.0:7443"
    mdns: false
  auth:
    user_token_hash: "sha256:<digest>"
    admin_token_hash: "sha256:<digest>" # optional
```

CI generates the bearer with a cryptographically secure random source, keeps
the plaintext in its masked secret channel, and puts only the digest in the
seed. The preferred flow attaches the seed as a datasource or seed image so
many machines can reuse the same generic agent ISO digest. An ephemeral ISO
with embedded cloud-config is allowed for isolated tests but must not be
published as the generic release artifact.

## Boot and foreground lifecycle

The installed appliance starts in this order:

1. Networking, the MCP gateway, and the lingering `agent` user manager start.
   The unprivileged broker can serve shell and file calls, but full readiness
   is false.
2. `ly` creates the real logind/seat session and auto-logs in `agent`.
3. XLibre starts on the DRM/virtio-gpu output and i3 becomes ready.
4. The session launcher imports its environment into the user manager; the
   broker starts Cua Driver over stdio.
5. The gateway connects to the broker, marks `/readyz` ready, and advertises
   the service through mDNS if configured.

No failure path creates a hidden graphical session. If there is no usable DRM
output, shell and file operations may still work, but `computer_use` remains
unavailable and readiness reports the degraded component.

Systemd restarts the gateway and broker with bounded backoff; the broker
restarts its Cua Driver child with bounded backoff. `ly` recovers a failed
graphical login. Pending UI operations fail on component loss; callers must
observe the result and decide whether a retry is safe.

The agent profile disables automatic screen locking and display power-off so
the controlled session remains visible and capturable.

## Foreground status and human intervention

An i3bar status block continuously shows one of:

```text
Agent: Starting | Ready | Active | Paused | Degraded
```

The indicator visibly changes while a remote `computer_use` action is active.
A first-run foreground panel shows the MCP URL, TLS fingerprint, and a newly
generated ordinary token exactly once when the token was generated locally.
CI-provisioned plaintext tokens are not reconstructed or shown by the guest.

Human keyboard and pointer input coexist with remote control. Human activity
does not automatically pause the agent, and remote computer-use operations are
serialized only against other remote computer-use operations.

`Super+Shift+Escape` is the local emergency control. Activating it:

- Rejects new MCP operations, including administrator operations.
- Closes active MCP sessions.
- Cancels the current computer-use operation.
- Terminates command and process groups created through MCP.
- Leaves i3 and locally started applications running.

The same local control resumes service. This is a runaway-automation safety
mechanism, not a containment promise against hostile code already executing as
the `agent` user or root.

## Audit behavior

The gateway records timestamp, stable token identifier, credential class, tool
name, duration, result class, and truncation or timeout flags. It does not log
bearer values, shell command strings, typed text, file content, patches, or
screenshots by default. Administrator operations are distinguishable in the
journal without exposing their content.

## Verification strategy

### Gate 1: native XLibre/Cua compatibility

The first implementation milestone is a compatibility spike using the pinned
Cua revision. It boots the real agent image with virtio-gpu and QEMU VNC. It
must not use Xvfb, Sway's headless backend, or a nested compositor.

The test validates:

- Cua Driver builds and runs against musl and all dynamic dependencies resolve.
- `DISPLAY`, `XAUTHORITY`, and session D-Bus are correct.
- i3 window discovery, bounds, stacking, and EWMH activation work.
- Desktop and per-window screenshots work.
- Foreground click, double-click, drag, scroll, typing, and named-key delivery
  change application-owned state.
- AT-SPI yields a non-empty tree and can activate and edit GTK controls.
- A Chromium or Electron fixture accepts foreground click and typing.
- A Cua desktop screenshot and a QMP framebuffer capture show the same visible
  desktop at compatible dimensions.

Failure blocks the feature. It does not trigger a fallback to a hidden desktop
or silently reduce the accepted tool contract.

### Gate 2: MCP and privilege contract

Contract tests verify:

- The server advertises exactly seven tools and stable schemas, with every
  closed value set published as a JSON Schema enum.
- Missing and invalid bearers are rejected.
- The ordinary bearer runs all OS operations as `agent`.
- The ordinary bearer cannot read `/root`, connect to the Docker socket, or
  invoke the root helper.
- Administrator support is absent by default and works only with a separately
  provisioned valid administrator token.
- The root helper independently rejects invalid administrator bearers.
- HTTPS, request limits, timeouts, output truncation, and concurrency limits
  behave deterministically.
- Emergency pause rejects both credential classes and terminates MCP-owned
  process groups.

### Gate 3: installation and recovery

A clean QEMU disk plus provisioning seed must install without interaction,
reboot from disk, auto-login to the visible XLibre/i3 session, become ready on
the forwarded MCP port, execute all seven tools, and retain configuration after
another reboot.

The generic ISO is separately booted without a seed to prove that it does not
write a disk automatically. Recovery tests kill Cua Driver, the session broker,
the gateway, and the graphical session independently and verify the documented
readiness and restart behavior.

### Reusable UI E2E fixture

The QEMU harness is also a consumer-facing test fixture. It starts an appliance
and emits a permission-restricted JSON descriptor with these stable fields:

- `mcp_url`: the host-forwarded MCP URL.
- `bearer_token_file`: a mode-0600 file containing the ordinary bearer.
- `ca_certificate_file` and `tls_fingerprint`: the available trust material.
- `vnc_address` and `novnc_url`: the configured visible-console addresses.
- `qmp_socket` and `artifact_directory`: control and diagnostic paths.

The descriptor and bearer file are permission-restricted; the plaintext bearer
is never printed to the normal job log.

A UI test can then:

1. Boot or reuse the ready appliance.
2. Copy or install its application artifact through the normal file and shell
   tools.
3. Launch the application in the visible XLibre session.
4. Drive it exclusively through the public `computer_use` contract.
5. Assert accessibility state, visible pixels, application-owned state, or a
   combination of them.
6. Retain MCP traces, gateway journals, QMP screenshots, and VNC-visible failure
   captures as test artifacts, with secrets redacted.

Hadron's reference GTK and Chromium/Electron fixtures are mandatory agent-ISO
release gates. Downstream UI repositories may reuse the same harness or invoke
a reusable workflow and designate their own suites as required CI gates. This
keeps the appliance contract consumer-neutral while making real UI automation
repeatable.

## Release policy

Release CI continues to build the ordinary Sway and i3 artifacts and adds the
agent artifact as a separate job. The agent ISO is published only when all
three agent gates pass. Updating the pinned Cua revision requires the entire
native compatibility, MCP isolation, installation, recovery, and reference-UI
matrix to pass again.

Release assets include SHA-256 checksums, an SBOM, and a build manifest that
records the pinned upstream revisions and image digests. Artifact signing is
outside this feature's scope.

## Acceptance criteria

The feature is complete when all of the following are true:

1. Building and releasing the normal Sway and i3 variants does not enable or
   include the agent services.
2. The separate agent ISO derives from XLibre/i3 and boots a real visible
   foreground session automatically.
3. QEMU VNC/noVNC and Cua observe and control the same session.
4. A client can connect to the documented HTTPS MCP URL with an ordinary bearer
   and use exactly the seven approved tools.
5. Ordinary operations run as a non-admin, non-Docker `agent` user.
6. Privileged operations are unavailable unless a separate administrator
   credential is provisioned and independently validated by the root helper.
7. Interactive installation requires disk confirmation; declarative CI
   installation is zero-touch only when an explicit device is supplied.
8. Emergency pause stops new and active remote work while leaving the local
   desktop usable.
9. The compatibility, privilege, install/recovery, and reference UI E2E suites
   gate publication of the agent ISO.
10. Downstream UI tests can consume the machine-readable VM fixture without
    depending on Cua's internal APIs.

## References

- [Cua architecture](https://cua.ai/docs/explanation/architecture)
- [Cua Driver platform support](https://github.com/trycua/cua/blob/main/docs/content/docs/reference/cua-driver/platform-support.mdx)
- [Cua Driver Linux platform crate](https://github.com/trycua/cua/tree/main/libs/cua-driver/rust/crates/platform-linux)
- [Cua computer-server](https://github.com/trycua/cua/tree/main/libs/python/computer-server)
- [Hermes Agent tools reference](https://github.com/NousResearch/hermes-agent/blob/main/website/docs/reference/tools-reference.md)
- [XLibre](https://www.xlibre.net/)
