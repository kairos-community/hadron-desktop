# Cua Agent Phase 4: UI E2E Fixture and Release Gates Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the agent appliance a reusable visible-VM test fixture, prove installation/privilege/recovery behavior end to end, and publish the agent ISO only when every gate passes.

**Architecture:** Extend the Phase 1 harness into a lifecycle tool that creates a Kairos `cidata` seed, boots/install QEMU with host-forwarded MCP plus VNC/QMP, performs TOFU certificate capture, and emits a permission-restricted descriptor. A static Go smoke client uses only the seven public MCP tools. Separate compatibility, contract, install, recovery, and reference-UI modes retain redacted diagnostics. CI builds standard ISOs as before and builds/publishes the third agent ISO only after all modes pass. A reusable workflow gives downstream UI repositories the same fixture contract.

**Tech Stack:** QEMU/OVMF/virtio-vga, QMP, VNC/noVNC, Kairos `cidata` ISO9660, Go MCP client, OpenSSL, xorriso, POSIX shell, GitHub Actions, Syft SPDX JSON.

## Global Constraints

- Every UI check drives the DRM-backed XLibre session shown by VNC; no headless or nested display is accepted.
- The fixture and tests use only public MCP tools, health endpoints, VNC, and QMP. They do not call raw Cua APIs.
- Descriptor and token files are mode 0600. No command may print bearer plaintext.
- The generic ISO is reused unchanged; per-VM seed, token, digest, TLS trust capture, disk, and descriptor stay outside it.
- Zero-touch installation requires `install.auto=true`, `install.device=/dev/vda`, and `hadron_agent.enabled=true`.
- Failed UI jobs retain screenshots, serial, QMP, MCP result metadata, and redacted journals.
- The release job publishes no agent ISO unless all five agent gates pass.
- Standard Sway/i3 release behavior stays independently buildable.

---

## File Structure

**Created:**

- `test/agent/lib/common.sh` — ports, OVMF, permissions, cleanup, redaction.
- `test/agent/lib/qmp.sh` — QMP execute/wait/screenshot/send-key helpers.
- `test/agent/make-seed.sh` — secure token/digest plus `cidata` ISO generator.
- `test/agent/fixture.schema.json` — stable eight-field descriptor schema.
- `test/agent/cmd/mcp-smoke/main.go` — public-contract, privilege, and UI client.
- `test/agent/build-fixtures.sh` — build/package GTK reference app for upload.
- `test/agent/README.md` — local and downstream fixture usage.
- `.github/workflows/agent-gates.yml` — PR/manual/nightly agent gate workflow.
- `.github/workflows/reusable-agent-ui-e2e.yml` — downstream UI gate.
- `scripts/release-manifest.sh` — deterministic build manifest generation.

**Modified:**

- `test/agent/run.sh` — `compatibility`, `contract`, `install`, `recovery`, `ui`, `fixture`, and `stop` modes.
- `tools/vm.sh` — optional host-forward and descriptor-friendly knobs without changing defaults.
- `.github/workflows/release.yml` — third artifact, gates, SBOM/manifest, asset verification.
- `README.md` — fixture and release artifact links.
- `.gitignore` — token, seed, descriptor, disk, and socket paths.

## Task 1: Freeze the fixture descriptor and secret handling

- [ ] Create a JSON Schema requiring exactly these string fields and no others:

```text
mcp_url
bearer_token_file
ca_certificate_file
tls_fingerprint
vnc_address
novnc_url
qmp_socket
artifact_directory
```

`mcp_url` must be HTTPS and end in `/mcp`; fingerprint is 64 lowercase hex; every file/path is absolute. `novnc_url` is present as an empty string when noVNC is disabled.

- [ ] Add shell tests that reject a missing field, extra field, relative path, HTTP MCP URL, world-readable descriptor, world-readable bearer, and a descriptor containing token text.

- [ ] `common.sh` starts with `umask 077`, allocates loopback ports, installs one EXIT trap, tracks QEMU/noVNC PIDs, and uses a redaction function for `hdn_u_`/`hdn_a_` strings. It never enables shell tracing after token generation.

- [ ] Extend `.gitignore` with:

```gitignore
/test/agent/runtime/*.token
/test/agent/runtime/*.iso
/test/agent/runtime/*.json
/test/agent/runtime/*.qcow2
/test/agent/runtime/*.sock
/test/agent/artifacts/
```

- [ ] Run descriptor tests and `bash -n test/agent/lib/common.sh`. Commit as `test(agent): freeze the reusable VM descriptor`.

## Task 2: Generate an explicit Kairos `cidata` seed

- [ ] Write failing tests for token prefix/length, digest-only YAML, optional independent admin token, exact install authorization, file modes, and absence of token text from command output.

- [ ] `make-seed.sh` generates 32 random bytes per requested credential, creates `hdn_u_` and optional `hdn_a_` bearers, writes each plaintext to a 0600 file, and computes lowercase SHA-256 over the complete bearer.

- [ ] Write `user-data` with:

```yaml
#cloud-config
hostname: hadron-agent-e2e
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
    user_token_hash: "sha256:${USER_DIGEST}"
    admin_token_hash: "sha256:${ADMIN_DIGEST}"
```

For live/contract mode omit the entire `install` block. For no-admin mode omit the admin line. Create empty `meta-data`.

- [ ] Build the datasource exactly as Kairos tests do:

```bash
xorriso -as mkisofs -quiet -V cidata \
  -graft-points -o "$SEED_ISO" \
  user-data="$SEED_DIR/user-data" meta-data="$SEED_DIR/meta-data"
```

- [ ] Use `xorriso -indev ... -cat /user-data` in tests to prove the ISO contains digests and no `hdn_` bearer.

- [ ] Commit as `test(agent): generate permission-restricted cidata seeds`.

## Task 3: Turn QEMU into a detachable visible fixture

- [ ] Extend `tools/vm.sh` with optional `HOST_MCP_PORT`, `QMP`, `SERIAL_LOG`, `PID_FILE`, and `SEED_ISO`. Defaults remain unchanged. When set, add `hostfwd=tcp:127.0.0.1:HOST_MCP_PORT-:7443`, a QMP Unix server, serial file, pidfile, and read-only seed CD.

- [ ] In `test/agent/run.sh fixture`, create a 24 GiB qcow2 disk, OVMF vars, seed, loopback VNC, optional noVNC, serial log, artifact directory, and QMP socket. Boot the installer ISO with disk boot index 0, installer CD 1, seed CD 2, and `virtio-vga`.

- [ ] Wait for TCP 7443. Capture the self-signed leaf without disabling subsequent verification:

```bash
openssl s_client -connect "127.0.0.1:$MCP_PORT" -showcerts </dev/null \
  2>/dev/null | openssl x509 -out "$CA_CERT"
openssl x509 -in "$CA_CERT" -outform DER |
  sha256sum | awk '{print $1}' > "$FINGERPRINT_FILE"
curl --fail --cacert "$CA_CERT" \
  "https://127.0.0.1:$MCP_PORT/readyz"
```

The certificate SAN includes 127.0.0.1 from Phase 3.

- [ ] Emit the descriptor atomically with all eight fields, absolute paths, mode 0600, and no token content. `fixture` remains running and prints only the descriptor path. `stop` reads the runtime PID file, requests graceful QMP powerdown, escalates after 20 seconds, stops noVNC, and keeps artifacts.

- [ ] Implement QMP helpers for `qmp_capabilities`, `query-status`, `screendump`, `system_reset`, `system_powerdown`, and HMP `sendkey meta_l-shift-esc`. Parse JSON rather than grepping partial socket output.

- [ ] Run one detached fixture, validate schema/modes, view VNC, then stop it. Commit as `test(agent): expose a detachable visible VM fixture`.

## Task 4: Build the public MCP smoke/privilege client

- [ ] Implement a bearer-injecting HTTP RoundTripper and trust pool loaded only from the descriptor CA file. Connect with `mcp.StreamableClientTransport`; never set InsecureSkipVerify.

- [ ] Add `contract` mode that:

1. Lists exactly seven tools.
2. Runs `terminal` with `id -u; id -un; id -nG` and proves user `agent` lacks admin/sudo/docker.
3. Starts a PTY process, writes stdin, polls output, proves `/proc/self/cgroup` contains its dedicated MCP leaf, and terminates it with no surviving grandchild.
4. Writes, reads, searches, and structured-patches files under `/home/agent/e2e`.
5. Captures the desktop with `computer_use`.
6. Proves ordinary read of `/root` fails.
7. Proves ordinary access to `/run/docker.sock` fails.
8. Proves the ordinary bearer cannot invoke the root socket.
9. With the separate admin file, proves terminal identity is root and an invalid admin token is rejected.
10. Lists the same seven public names for both credential classes.

- [ ] Add deterministic exit codes: 0 pass, 1 assertion, 2 descriptor/arguments, 3 transport/readiness. Write JSON results to the artifact directory but never serialize bearer values or command/file contents.

- [ ] Run unit tests against the Phase 2 in-process gateway, build statically, then run against a live fixture. Commit as `test(agent): verify MCP auth and privilege boundaries end to end`.

## Task 5: Gate non-destructive live boot and zero-touch installation

- [ ] `contract` mode attaches a blank qcow2 disk but uses a live seed without `install`. Hash the disk before and after, query QMP block stats, and require no guest writes to the blank disk. Exercise the smoke client against the live agent session.

- [ ] `install` mode uses the explicit install seed and waits through the automatic reboot until QMP reports the disk boot and `/readyz` succeeds. It then runs all seven tools.

- [ ] Through MCP, write `/home/agent/e2e/persistence-marker`, request `systemctl reboot` with the admin bearer, wait for port loss/readiness, and read the marker after the second boot. Confirm endpoint config, digest identity, hostname, and locked agent account persist.

- [ ] Boot the generic ISO once with no seed and a blank disk. Require the interactive/non-install live desktop, no disk hash change, and no `kairos-agent install` process.

- [ ] Collect QMP screenshots before install, after first installed login, and after second reboot. Commit as `test(agent): gate safe live boot and seeded installation`.

## Task 6: Gate process, service, and graphical-session recovery

- [ ] Start one non-replayable foreground click and one long process, then exercise these failures independently:

| Failure | Expected observation |
|---|---|
| kill `cua-driver` | ready false, shell still works, Cua reconnects, no click replay |
| kill user broker | ready false, user systemd restarts it, shell/Cua recover |
| kill gateway | port closes, systemd restarts it, bearer still works |
| `i3-msg exit` | watchdog restarts Ly, Ly autologs on tty1, no hidden display starts, Cua reattaches |
| local emergency key via QMP | both credentials receive PAUSED, MCP process groups die, i3 remains visible |
| emergency key again | service returns to ready with no old operation replay |

- [ ] Bound each recovery to 90 seconds and require at least one observed degraded/not-ready transition so a test cannot pass without seeing the failure.

- [ ] Retain redacted system/user journals, serial, health timeline, process result metadata, QMP screenshots, and the Cua/QMP foreground comparison on failure.

- [ ] Run `test/agent/run.sh recovery` twice to detect stale sockets/processes. Commit as `test(agent): gate appliance recovery and emergency pause`.

## Task 7: Add reference GTK and Chromium UI suites using only public tools

- [ ] `build-fixtures.sh` compiles the Phase 1 GTK app against the agent runtime and produces a tar plus SHA-256 under the ignored runtime directory.

- [ ] `ui` mode uses `write_file` and `terminal` to install the GTK fixture under `/home/agent/e2e/bin`, launches it in the existing visible session, and drives click, double-click, drag, scroll, text, and named key exclusively through public `computer_use`. Assert state via accessibility plus its application-owned JSON.

- [ ] Install the exact commit from `test/agent/fixtures/chromium.commit` for the agent user from Flathub, verify the installed commit, copy the HTML fixture, and launch it with `--ozone-platform=x11` in the same XLibre session. Drive click/type/key/drag/scroll through public `computer_use`. Assert through fresh accessibility state and visible pixels, not CDP. Cache the downloaded Flatpak objects in CI under a key containing that commit.

- [ ] For both apps compare a public computer screenshot with a QMP capture and retain VNC-visible failure shots.

- [ ] Run `test/agent/run.sh ui` from a fresh installed disk. Commit as `test(agent): add GTK and Chromium public-tool UI gates`.

## Task 8: Document and expose downstream UI consumption

- [ ] `test/agent/README.md` documents prerequisites, start/stop commands, descriptor schema, TLS trust, secret handling, application upload/launch, public computer-use assertions, artifact paths, and a minimal Go client invocation.

- [ ] Add `reusable-agent-ui-e2e.yml` with required inputs `agent_iso_url`, `agent_iso_sha256`, `hadron_ref`, and `test_command`. It checks out the caller repository plus Hadron at the requested ref, verifies the ISO checksum, installs QEMU/OVMF/xorriso/jq/OpenSSL/noVNC dependencies, starts `fixture`, exports `HADRON_AGENT_FIXTURE`, executes the caller-controlled command, always stops the VM, and uploads redacted artifacts.

- [ ] The reusable workflow requests no repository write permission and no secrets. The SHA-256 input is mandatory; downloading an unverified ISO fails.

- [ ] Add a local sample under `test/agent/examples/descriptor-smoke.sh` that validates the descriptor and calls `mcp-smoke contract`. Run it against a fixture.

- [ ] Commit as `docs(agent): publish the downstream UI E2E contract`.

## Task 9: Add PR/nightly agent gates

- [ ] Create `agent-gates.yml` for pull requests touching `agent/**`, `rootfs-agent/**`, `Dockerfile.agent`, `test/agent/**`, `Makefile`, or the workflow itself; also allow manual dispatch and nightly schedule.

- [ ] Install `qemu-system-x86`, `ovmf`, `xorriso`, `jq`, `python3-pil`, `websockify`, and `novnc`. Reclaim runner disk as the release workflow already does.

- [ ] Jobs run in dependency order: Go unit/race/static checks; native compatibility; MCP/privilege contract; install; recovery; GTK/Chromium UI. Build/upload the agent ISO once and reuse it. Upload diagnostics only with `if: always()` and 7-day retention.

- [ ] Add concurrency cancellation for superseded branch runs. Use a 330-minute overall agent VM timeout and per-gate shell timeouts already defined in the harness.

- [ ] Validate workflow syntax, manually dispatch it, and require every job green. Commit as `ci(agent): gate the visible agent appliance`.

## Task 10: Publish exactly three release ISOs with evidence

- [ ] Keep the existing Sway/i3 build matrix. Add an `agent` job that runs all agent gates before preparing `hadron-desktop-agent-${RELEASE_TAG}-amd64.iso`. Upload nothing from that job until gates pass.

- [ ] For every image, generate `.sha256`, SPDX JSON with `anchore/syft:v1.29.0`, and `.build.json`. The manifest contains release tag, Git commit, Hadron base reference/digest, final OCI image ID, ISO SHA-256, desktop flavor, XLibre version where applicable, and for agent Cua revision plus AT-SPI version.

- [ ] Make release depend on both standard build and agent job. Verify exactly three ISOs, three checksums, three SBOMs, and three build manifests; validate checksums and required JSON keys before upload.

- [ ] Update release download names and README. Do not sign artifacts in this feature.

- [ ] Dry-run with a non-publishing workflow dispatch, inspect all 12 files, then commit as `ci(release): publish the gated agent ISO`.

## Task 11: Final clean-room acceptance

- [ ] From clean generated directories run:

```bash
cd agent
go test -race ./...
cd ..
make DESKTOP=sway image
make DESKTOP=i3 image
make agent-iso VERSION=v0.1.0-acceptance
test/agent/run.sh compatibility
test/agent/run.sh contract
test/agent/run.sh install
test/agent/run.sh recovery
test/agent/run.sh ui
bash test/agent/profile_isolation.sh
```

- [ ] Validate a detached fixture descriptor and execute the documented sample from a separate temporary UI repository checkout.

- [ ] Confirm standard image exports contain no agent/Cua paths; the agent account lacks forbidden groups; the root helper is inactive without admin provisioning; all eight descriptor fields exist; token files/descriptors are 0600; serial/journals contain no bearer.

- [ ] Confirm `git status --short` shows no generated files and preserves the user's pre-existing untracked ISO. Record final gate durations and artifact hashes in the release job, not the repository.
