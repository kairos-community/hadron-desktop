# Cua Agent Phase 1: Native XLibre Compatibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove that pinned Cua builds for Hadron musl and controls the exact XLibre/i3 desktop shown through QEMU VNC.

**Architecture:** Build an agent-only overlay containing libXtst, AT-SPI, X11 GTK3, and Cua. Boot a test-only derivative with Ly autologin, deterministic GTK and Chromium fixtures, and an in-session Go probe that owns Cua over MCP stdio. Compare Cua's desktop PNG with a QMP capture of the VNC-visible scanout.

**Tech Stack:** Hadron musl toolchain, Rust 1.97, Cua 0.7.1 at `3cadb5f82e7d2ed071a2082764276ec872a52135`, libXtst 1.2.5, gsettings-desktop-schemas 47.1, AT-SPI 2.54.0, GTK 3.24.43, Go 1.25, MCP Go SDK 1.6.1, Flatpak Chromium, QEMU/QMP/VNC.

## Global Constraints

- Base the overlay on `i3-desktop:dev`. The normal Sway and i3 images receive no agent files.
- Inherit `DISPLAY`, `XAUTHORITY`, and `DBUS_SESSION_BUS_ADDRESS` from Ly; never synthesize a display.
- Do not enable portal input/capture, `/dev/uinput`, Xvfb, Xephyr, Xdummy, Sway headless, a nested compositor, or a second VNC session.
- Set `CUA_DRIVER_RS_TELEMETRY_ENABLED=false` and `CUA_DRIVER_RS_UPDATE_CHECK=false`.
- Force foreground delivery for state-changing compatibility checks.
- Fixtures never enter the production `agent` target; the test account uses the same non-privileged groups as production.
- Stop the feature if this phase is not green.

---

## File Structure

**Created:**

- `Dockerfile.agent` — native agent-only runtime and Cua build.
- `agent/go.mod`, `agent/go.sum` — pinned service/probe module.
- `agent/internal/cua/client.go`, `client_test.go` — serialized MCP stdio client.
- `test/agent/Dockerfile.compat` — test-only account, fixtures, Chromium, and reporter.
- `test/agent/cmd/cua-compat-probe/main.go` — graphical acceptance probe.
- `test/agent/fixtures/gtk3/main.c` — accessible stateful GTK app.
- `test/agent/fixtures/chromium/index.html` — stateful Chromium page.
- `test/agent/fixtures/chromium.commit` — immutable Flathub commit exercised by the gates.
- `test/agent/rootfs/etc/i3/config.d/99-cua-compat.conf` — starts fixtures/probe.
- `test/agent/rootfs/etc/systemd/system/cua-compat-report.service` — serial/artifact reporter.
- `test/agent/rootfs/usr/local/libexec/cua-compat-report` — reporter script.
- `test/agent/frame_compare.py` — PNG/PPM comparison.
- `test/agent/run.sh` — real graphical QEMU gate.

**Modified:** `.gitignore`.

## Task 1: Build the Cua MCP child client test-first

- [ ] Create `agent/go.mod`:

```go
module github.com/mudler/hadron-desktop/agent

go 1.25.0

require github.com/modelcontextprotocol/go-sdk v1.6.1
```

- [ ] In `client_test.go`, make `TestHelperProcess` run an MCP server on `mcp.StdioTransport` when `GO_WANT_CUA_HELPER=1`. Register `echo` and `block`, then add `TestClientStartsCallsAndCloses`, `TestClientCancelsInflightCall`, `TestClientReportsChildExit`, and `TestClientSerializesCalls`. The last test must observe a maximum concurrency of one.

- [ ] Run `cd agent && go test ./internal/cua -run TestClient -count=1`. Expected: compilation fails because the client is absent.

- [ ] Implement this API in `client.go`:

```go
type Options struct {
	Binary string
	Args   []string
	Env    []string
}

func Start(context.Context, Options) (*Client, error)
func (c *Client) Call(context.Context, string, any) (*mcp.CallToolResult, error)
func (c *Client) Close() error
```

`Start` uses `mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}` and `mcp.NewClient(...).Connect`. Serialize complete `CallTool` operations with one mutex. Make `Close` idempotent, close the MCP session first, and never log to stdout.

- [ ] Run:

```bash
cd agent
go mod tidy
go test ./internal/cua -run TestClient -count=1
cd ..
git add agent
git commit -m "feat(agent): add Cua MCP stdio client"
```

Expected: all four tests pass.

## Task 2: Build native Cua and the X11 accessibility runtime

- [ ] Start `Dockerfile.agent` with these immutable inputs:

```dockerfile
ARG BASE_IMAGE=i3-desktop:dev
FROM ${BASE_IMAGE} AS xlibre-desktop
FROM rust:1.97.0-alpine3.22 AS rust-toolchain
FROM ghcr.io/kairos-io/hadron-toolchain:main AS agent-native
COPY --from=xlibre-desktop /usr/include/ /usr/include/
COPY --from=xlibre-desktop /usr/lib/ /usr/lib/
RUN ldconfig
ENV PKG_CONFIG_PATH=/usr/lib/pkgconfig:/usr/share/pkgconfig
ARG LIBXTST_VERSION=1.2.5
ARG GSETTINGS_SCHEMAS_VERSION=47.1
ARG ATSPI_VERSION=2.54.0
ARG GTK3_VERSION=3.24.43
ARG CUA_COMMIT=3cadb5f82e7d2ed071a2082764276ec872a52135
```

- [ ] In `agent-runtime-build`, build libXtst from the official X.Org tarball with `--prefix=/usr --libdir=/usr/lib --disable-static`. Install once into the live builder and once with `DESTDIR=/agent-runtime`. Assert `/agent-runtime/usr/lib/libXtst.so.6` exists.

- [ ] Build gsettings-desktop-schemas 47.1 into `/agent-runtime` with `-Dintrospection=false`. This supplies `org.gnome.desktop.interface` and `org.gnome.desktop.a11y.applications` for the AT-SPI launcher.

- [ ] Build AT-SPI 2.54.0 with:

```bash
meson setup build --prefix=/usr --libdir=lib --libexecdir=libexec \
  --buildtype=release -Ddefault_bus=dbus-daemon \
  -Ddbus_daemon=/usr/bin/dbus-daemon -Duse_systemd=true \
  -Dgtk2_atk_adaptor=false -Ddocs=false \
  -Dintrospection=disabled -Dx11=enabled
ninja -C build
ninja -C build install
DESTDIR=/agent-runtime ninja -C build install
```

Reject the build unless the artifact contains `at-spi-bus-launcher`, `at-spi2-registryd`, `libatk-bridge-2.0.so.0`, `org.a11y.Bus.service`, `org.a11y.atspi.Registry.service`, and `at-spi-dbus-bus.service`.

- [ ] Build GTK 3.24.43 into the same artifact with X11 enabled, Wayland/Broadway disabled, and docs/demos/examples/tests disabled:

```bash
meson setup build --prefix=/usr --libdir=lib --buildtype=release \
  -Dx11_backend=true -Dwayland_backend=false -Dbroadway_backend=false \
  -Dintrospection=false -Dgtk_doc=false -Dman=false \
  -Ddemos=false -Dexamples=false -Dtests=false \
  -Dprint_backends=file -Dcolord=no
```

- [ ] Declare `FROM agent-runtime-build AS cua-build`, then copy Cargo/Rustup from `rust-toolchain`, check out the pinned Cua commit, and run:

```bash
cd /build/cua/libs/cua-driver/rust
cargo build --locked -p cua-driver --release
install -Dm755 target/release/cua-driver /cua/usr/bin/cua-driver
/cua/usr/bin/cua-driver --version
ldd /cua/usr/bin/cua-driver | tee /cua/ldd.txt
! grep -q 'not found' /cua/ldd.txt
```

Do not pass portal features. Hadron's compiler/linker and copied X11 libraries must perform the native link.

- [ ] End with production target `agent` based on `BASE_IMAGE`. Copy only `/agent-runtime` and `/cua`, run `glib-compile-schemas` and `ldconfig`, and reject unresolved Cua links. It contains no account, autologin override, fixture, token, TLS key, or service yet.

- [ ] Verify and commit:

```bash
make DESKTOP=i3 image
docker build -f Dockerfile.agent --target agent \
  --build-arg BASE_IMAGE=i3-desktop:dev -t hadron-agent:compat .
docker run --rm hadron-agent:compat sh -c \
  'cua-driver --version; ! ldd /usr/bin/cua-driver | grep -q "not found"; test -x /usr/libexec/at-spi-bus-launcher'
git add Dockerfile.agent
git commit -m "build(agent): compile Cua and X11 accessibility runtime"
```

Expected: Cua reports 0.7.1 and all checks pass.

## Task 3: Add deterministic test-only UI fixtures

- [ ] Implement `main.c` as a 900x640 window titled `Hadron Cua GTK Fixture` with accessible names `Click count`, `Double click count`, `Text input`, `Named key`, `Drag target`, and `Scrollable rows`. Use GtkButton, GtkEntry, GtkEventBox, and GtkScrolledWindow. Atomically write:

```json
{"clicks":0,"double_clicks":0,"text":"","key":"","dragged":false,"scroll_value":0}
```

to `/run/user/1000/hadron-cua-gtk-state.json` after every action. Separate drag source and target by 220 pixels or more.

- [ ] Implement `index.html` with title `Hadron Cua Chromium Fixture`, stable ARIA labels, click/input/key/drag/drop/scroll controls, and a visible `pre#state`. Mirror state into the title and visible page only; expose no CDP assertion path.

- [ ] In `Dockerfile.compat`, compile the GTK fixture in a Hadron-toolchain builder after copying headers/libraries from `BASE_IMAGE`:

```dockerfile
RUN cc -O2 -Wall -Wextra $(pkg-config --cflags gtk+-3.0) \
  /src/main.c -o /out/hadron-cua-gtk-fixture \
  $(pkg-config --libs gtk+-3.0)
```

- [ ] Resolve `org.chromium.Chromium//stable` once with `flatpak remote-info --show-commit flathub org.chromium.Chromium`, validate the result is 64 lowercase hex characters, and save that exact value in `test/agent/fixtures/chromium.commit`. Install with `flatpak install --commit="$(cat /src/chromium.commit)"` system-wide only in this test image, then assert `flatpak info --show-commit` equals the pinned file and copy it to `/etc/hadron-agent-test/chromium-commit`.

- [ ] Build, inspect, and commit:

```bash
docker build -f test/agent/Dockerfile.compat \
  --build-arg BASE_IMAGE=hadron-agent:compat -t hadron-agent-compat:test .
docker run --rm hadron-agent-compat:test sh -c \
  'test -x /usr/local/libexec/hadron-cua-gtk-fixture; flatpak info org.chromium.Chromium'
git add test/agent/Dockerfile.compat test/agent/fixtures
git commit -m "test(agent): add GTK and Chromium Cua fixtures"
```

## Task 4: Boot one real Ly/XLibre test session

- [ ] In `Dockerfile.compat`, create locked UID 1000 `agent` in groups `audio,video,render,input,bluetooth,seat,hadron-agent-control` and explicitly exclude `admin`, `sudo`, and `docker`.

- [ ] Set Ly values `auto_login_user = agent`, `auto_login_session = i3`, and `auto_login_service = ly-autologin`. A test-only unit drop-in clears the normal live-mode condition and then retains `ConditionKernelCommandLine=!install-mode`. Ly must still own tty1 and XLibre.

- [ ] Create `99-cua-compat.conf`:

```text
exec --no-startup-id /usr/local/libexec/hadron-cua-gtk-fixture
exec --no-startup-id flatpak run --filesystem=/opt/hadron-agent-fixtures:ro org.chromium.Chromium --disable-gpu --ozone-platform=x11 --no-first-run --new-window file:///opt/hadron-agent-fixtures/chromium/index.html
exec --no-startup-id /usr/local/libexec/cua-compat-probe
```

- [ ] Implement `cua-compat-report`. It waits 240 seconds for `/run/user/1000/hadron-cua-compat/result.json`, emits only `CUACOMPAT: BEGIN/PASS/FAIL/DONE` serial markers, gathers result JSON, Cua PNG, GTK state, Chromium commit, `cua-driver doctor --json`, `xprop -root`, `xrandr`, and the user journal, redacts environment keys containing `TOKEN`, and writes a tar at offset zero of `/dev/disk/by-id/virtio-hadronagentartifacts`.

- [ ] Validate shell syntax and account isolation, then commit:

```bash
sh -n test/agent/rootfs/usr/local/libexec/cua-compat-report
docker build -f test/agent/Dockerfile.compat \
  --build-arg BASE_IMAGE=hadron-agent:compat -t hadron-agent-compat:test .
docker run --rm hadron-agent-compat:test sh -c \
  'id agent; ! id -nG agent | grep -Eq "(^| )(admin|sudo|docker)( |$)"'
git add test/agent/Dockerfile.compat test/agent/rootfs
git commit -m "test(agent): boot a visible XLibre compatibility session"
```

## Task 5: Implement the in-session probe

- [ ] Define a result with booleans named `environment`, `xtest`, `window_discovery`, `ewmh_activation`, `desktop_capture`, `window_capture`, `gtk_accessibility`, `gtk_click`, `gtk_double_click`, `gtk_drag`, `gtk_scroll`, `gtk_type`, `gtk_named_key`, `chromium_accessibility`, `chromium_click`, and `chromium_type`. Write a failing-first `AllPassed` unit test.

- [ ] Spawn `/usr/bin/cua-driver mcp --no-daemon-relaunch` through `internal/cua`. Append the two opt-out variables plus `CUA_DRIVER_RS_A11Y_ADVERTISE_MODE=all`. Fail `environment` unless the three inherited session variables and their Xauthority/bus endpoints work.

- [ ] Exercise `get_desktop_state`, `list_windows`, `get_window_state`, `bring_to_front`, `click`, `double_click`, `drag`, `scroll`, `type_text`, and `press_key`. Set `delivery_mode: foreground` on every mutation. Assert GTK only through its state file. Assert Chromium through a fresh AT-SPI tree and visible state text, never JavaScript or DevTools.

- [ ] Build the probe statically in `golang:1.25.0-alpine3.22` with `CGO_ENABLED=0`, copy it only into the test image, run `cd agent && go test ./...`, rebuild the test image, and confirm the probe is executable.

- [ ] Commit:

```bash
git add agent test/agent/cmd test/agent/Dockerfile.compat
git commit -m "test(agent): drive XLibre fixtures through Cua MCP"
```

## Task 6: Add the visible QEMU/framebuffer gate

- [ ] Implement `test/agent/run.sh compatibility` to build all images, wrap the test image with AuroraBoot, create a 256 MiB raw artifact disk, and boot OVMF QEMU with `virtio-vga`, `-vnc 127.0.0.1:19`, serial logging, a Unix QMP socket, and user networking. Use KVM when available and `-accel tcg,thread=multi` otherwise. Never include `-display none` or `-nographic`.

- [ ] Wait 600 seconds for `CUACOMPAT: DONE`. Before stopping the VM, send QMP `screendump` to create `qmp-desktop.ppm`. Extract the guest tar and fail on any false result or `CUACOMPAT: FAIL`.

- [ ] Implement `frame_compare.py` to decode Cua PNG and QMP PPM, require equal dimensions, sample a 16x16 grid excluding a 12-pixel border, and pass only when mean absolute RGB error is below 24. Write `frame-compare.json` with both dimensions, error, and `passed`.

- [ ] Append to `.gitignore`:

```gitignore
/test/agent/artifacts/
/test/agent/runtime/
/build/agent-desktop/
```

- [ ] Run syntax checks and commit:

```bash
bash -n test/agent/run.sh
python3 -m py_compile test/agent/frame_compare.py
git add .gitignore test/agent/run.sh test/agent/frame_compare.py
git commit -m "test(agent): gate Cua against the visible QEMU framebuffer"
```

## Task 7: Run the stop/go checkpoint

- [ ] Run:

```bash
rm -rf test/agent/artifacts test/agent/runtime build/agent-desktop
test/agent/run.sh compatibility
jq -e 'to_entries | all(.value == true)' \
  test/agent/artifacts/compatibility/result.json
jq -e '.passed == true and .cua_width == .qmp_width and .cua_height == .qmp_height' \
  test/agent/artifacts/compatibility/frame-compare.json
```

Expected: exit zero, no fail marker, all result fields true, equal framebuffer dimensions, and mean error below 24.

- [ ] Have the harness write `compatibility.json` with Cua revision/version, XLibre version, AT-SPI version, Chromium Flatpak commit, dimensions, and image ID. Do not commit generated artifacts.

- [ ] Run `git status --short`. Expected: only the user's pre-existing untracked ISO remains. Do not begin Phase 2 while this gate is red.
