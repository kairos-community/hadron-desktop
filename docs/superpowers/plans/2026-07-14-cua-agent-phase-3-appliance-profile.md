# Cua Agent Phase 3: Appliance Profile Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the compatible Cua runtime and MCP services into a separate live/installable Hadron agent ISO with automatic visible login, secure first-boot provisioning, status, mDNS, and emergency control.

**Architecture:** Extend `Dockerfile.agent` with a static Go build and a `rootfs-agent/` overlay, always layered on the i3 image. A root provisioning oneshot creates machine TLS/auth state and runtime directories. A dedicated gateway system identity serves HTTPS. A lingering `agent` user manager runs the session broker before the display is ready; an i3 hook imports the real session environment and restarts it so it owns Cua. The root helper is conditionally started only when an admin digest exists. Agent-specific OEM stages support live mode and explicit interactive/zero-touch installation.

**Tech Stack:** Docker multi-stage builds, Go 1.25 static binary, systemd system/user units, Ly/XLibre/i3, Kairos OEM YAML, POSIX shell, ECDSA P-256 TLS, zeroconf mDNS.

## Global Constraints

- `make agent-iso` always derives from `DESKTOP=i3` and uses `Dockerfile.agent`.
- The ordinary Sway and i3 builds remain byte-for-byte free of agent profile files and behavior.
- The generic ISO embeds no bearer value/digest or TLS private key.
- `agent` has a locked password and groups `audio,video,render,input,bluetooth,seat,hadron-agent-control` only; never `admin`, `sudo`, or `docker`.
- The gateway identity cannot execute tools. The agent broker owns Cua. The root helper is absent from the active service graph unless explicitly enabled.
- Live boot is non-destructive. Disk writing requires either interactive confirmation or `install.auto=true` with an explicit device in a seed.
- The first-run panel may read a one-shot plaintext token from tmpfs; persistent state contains only digests.
- No failure path starts a hidden display.

---

## File Structure

**Created:**

- `rootfs-agent/etc/hadron-agent/defaults.json` — non-secret limits and endpoint defaults.
- `rootfs-agent/etc/systemd/system/hadron-agent-provision.service` — machine-state bootstrap.
- `rootfs-agent/etc/systemd/system/hadron-agent-gateway.service` — public unprivileged gateway.
- `rootfs-agent/etc/systemd/system/hadron-agent-root-helper.service` — conditional root helper.
- `rootfs-agent/etc/systemd/system/hadron-agent-display-watchdog.service` — bounded Ly restart for a lost foreground session.
- `rootfs-agent/etc/systemd/system/hadron-agent-autoinstall.service` — seed-gated default-boot zero-touch install.
- `rootfs-agent/usr/lib/systemd/user/hadron-agent-session.service` — lingering agent broker.
- `rootfs-agent/etc/tmpfiles.d/hadron-agent.conf` — parent runtime/state layout.
- `rootfs-agent/etc/systemd/system/ly@.service.d/20-agent-profile.conf` — allow live agent login, still block install mode.
- `rootfs-agent/etc/i3/config.d/90-hadron-agent.conf` — panel, no-lock policy, and emergency binding.
- `rootfs-agent/usr/bin/start-desktop` — agent-only i3 launcher using the lingering user-manager bus.
- `rootfs-agent/usr/bin/hadron-agent-session-ready` — import real display/session environment.
- `rootfs-agent/usr/bin/hadron-agent-first-run` — one-shot foreground connection panel.
- `rootfs-agent/usr/bin/hadron-status` — agent-aware i3 status producer.
- `rootfs-agent/usr/local/bin/hadron-agent-install` — safe agent installer.
- `rootfs-agent/system/oem/90_agent_profile.yaml` — live/installed account and service boot.
- `rootfs-agent/system/oem/91_agent_installer.yaml` — install-mode override.
- `agent/internal/provision/config.go`, `config_test.go` — `hadron_agent` seed merge/validation.
- `agent/internal/provision/state.go`, `state_test.go` — TLS/token/config materialization.
- `agent/internal/discovery/mdns.go`, `mdns_test.go` — optional service advertisement.
- `test/agent/install_script_test.sh` — installer safety tests.

**Modified:**

- `Dockerfile.agent` — compile/copy service binary and agent rootfs.
- `Makefile` — `agent-image` and `agent-iso` targets.
- `agent/go.mod` / `go.sum` — YAML and zeroconf dependencies.
- `agent/cmd/hadron-agent/main.go` — provision/token commands.
- `README.md` — opt-in agent build/run/provisioning documentation.

## Task 1: Implement and freeze the `hadron_agent` provisioning schema

- [ ] Add `gopkg.in/yaml.v3@v3.0.1` and write failing tests that merge sorted `/oem/*.yaml` files, ignore unrelated Kairos keys, use last-defined agent values, reject duplicate conflicting secrets, and validate the full schema.

- [ ] Support this exact shape:

```yaml
hadron_agent:
  enabled: true
  endpoint:
    listen: "0.0.0.0:7443"
    mdns: true
    insecure_loopback: false
  auth:
    user_token_hash: "sha256:0000000000000000000000000000000000000000000000000000000000000000"
    user_previous_token_hash: ""
    user_previous_valid_until: ""
    admin_token_hash: ""
    admin_previous_token_hash: ""
    admin_previous_valid_until: ""
  tls:
    certificate_pem: ""
    private_key_pem: ""
  limits:
    max_concurrent_calls: 16
    max_processes: 16
    max_request_bytes: 2097152
```

Only `enabled`, `endpoint`, and `auth.user_token_hash` are needed for normal CI. Inline TLS is allowed only in the permission-restricted seed; both PEM fields must be present together.

- [ ] Defaults are listen `0.0.0.0:7443`, mDNS true, insecure loopback false, 16 calls/processes, and 2 MiB request bodies. Reject non-loopback insecure listeners, digest prefixes other than `sha256:`, overlap over 24 hours, port zero, and unknown top-level `hadron_agent` fields.

- [ ] Because choosing the separate agent ISO is the opt-in boundary, an absent `hadron_agent` section on that profile resolves to enabled with the defaults above and locally generated machine state. An explicit `hadron_agent.enabled=false` suppresses gateway, broker, root helper, autologin, and first-run panel. The ordinary images contain no profile marker or provisioning service, so this default cannot affect them.

- [ ] Run `go test ./internal/provision -run Config -count=1` and commit as `feat(agent): define appliance provisioning schema`.

## Task 2: Materialize machine TLS, digests, and service configs

- [ ] Write failing state tests for live first boot, installed first boot, subsequent installed boot, CI-provided digest, optional admin digest, provided TLS pair, generated TLS pair, one-shot token ownership, fingerprint, atomic writes, and permission failures.

- [ ] Persist only:

```text
/var/lib/hadron-agent/gateway/config.json   root:hadron-agent-gateway 0440
/var/lib/hadron-agent/gateway/tls.crt       root:hadron-agent-gateway 0440
/var/lib/hadron-agent/gateway/tls.key       root:hadron-agent-gateway 0440
/var/lib/hadron-agent/session/config.json   root:agent 0440
/var/lib/hadron-agent/root/config.json      root:root 0400, only with admin
/var/lib/hadron-agent/root/enabled          root:root 0400, only with admin
```

Write with temporary files, fsync, rename, and directory fsync. Never put plaintext bearers under `/var`.

- [ ] When no user digest is provisioned, generate a 256-bit `hdn_u_` token, persist its digest, and write plaintext once to `/run/hadron-agent/first-run-token` owned by `agent` mode 0400. In live mode the overlay state disappears at reboot; installed state reuses the digest and does not recreate plaintext.

- [ ] Generate an ECDSA P-256 self-signed certificate valid for 397 days with SANs `localhost`, `127.0.0.1`, `::1`, current hostname, `hostname.local`, and every non-loopback address present after network-online. Record its SHA-256 DER fingerprint in redacted runtime status. Managed deployments that need stable addresses provision their own certificate/key pair. If a later DHCP lease is not covered, keep serving the same certificate and surface an actionable SAN-mismatch warning; never silently rotate trust identity.

- [ ] Add root-only `hadron-agent provision --oem-dir /oem --state-dir /var/lib/hadron-agent --runtime-dir /run/hadron-agent` and `hadron-agent token rotate user|admin --overlap 15m`. Rotation is available through an explicitly provisioned admin terminal or local recovery console, prints a new bearer only to its controlling terminal, never journal/stdout when no TTY, writes gateway/root current+previous digest state atomically, then reloads the gateway and active root helper. A failed reload rolls the config files back before returning nonzero.

- [ ] Run `go test -race ./internal/provision ./internal/auth` and commit as `feat(agent): provision machine TLS and bearer state`.

## Task 3: Add optional mDNS without widening the API

- [ ] Add `github.com/grandcat/zeroconf@v1.0.0` and unit-test enable/disable, hostname normalization, shutdown, duplicate start, and TXT values.

- [ ] Advertise service `_hadron-agent._tcp` in domain `local.` at the configured port with TXT keys `path=/mcp`, `tls=1`, and the actual binary build version under `version`. Never advertise token IDs, fingerprints, IPs, paths outside `/mcp`, or admin state.

- [ ] Start advertisement only after the gateway listener is live; stop it during pause only if the gateway is stopping, not for an ordinary safety pause. A disabled mDNS setting creates no multicast socket.

- [ ] Run `go test -race ./internal/discovery ./internal/gateway` and commit as `feat(agent): advertise the MCP endpoint over mDNS`.

## Task 4: Build and assemble the service binary in `Dockerfile.agent`

- [ ] Add a `golang:1.25.0-alpine3.22` builder. Copy `agent/go.mod`/`go.sum` first for caching, download modules, then copy `agent/` and build:

```dockerfile
ARG VERSION=v0.0.0
ARG CUA_COMMIT=3cadb5f82e7d2ed071a2082764276ec872a52135
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -trimpath \
  -ldflags="-s -w -X main.version=${VERSION} -X main.cuaRevision=${CUA_COMMIT}" \
  -o /out/hadron-agent ./cmd/hadron-agent
```

- [ ] Add `COPY rootfs-agent/ /` and copy the service binary into `/usr/bin/hadron-agent` only in target `agent`. Create locked system user/group `hadron-agent-gateway` and group `hadron-agent-control`. Do not create the human `agent` account at image build; OEM/install paths create it.

- [ ] Enable `hadron-agent-autoinstall.service`, `hadron-agent-provision.service`, `hadron-agent-gateway.service`, and `hadron-agent-display-watchdog.service` in multi-user target and enable `hadron-agent-session.service` globally. Do not enable the root helper; its OEM stage links it only when `root/enabled` exists.

- [ ] Verify:

```bash
docker build -f Dockerfile.agent --target agent \
  --build-arg BASE_IMAGE=i3-desktop:dev -t agent-desktop:dev .
docker run --rm agent-desktop:dev sh -c \
  'hadron-agent version; test -x /usr/bin/cua-driver; test -f /etc/hadron-agent/defaults.json; ! getent passwd agent'
```

- [ ] Commit as `build(agent): assemble the opt-in appliance overlay`.

## Task 5: Add account, socket, user-manager, and service lifecycle

- [ ] Create `90_agent_profile.yaml`. Before any agent service in live or installed non-install mode, create locked UID 1000 `agent` if absent, ensure only the approved supplemental groups, create `/home/agent` ownership, and run `loginctl enable-linger agent`. Abort if UID 1000 belongs to another account.

- [ ] Use these socket directories and permissions:

| Directory | Owner | Mode |
|---|---|---|
| `/run/hadron-agent/session` | `agent:hadron-agent-gateway` | 0750 |
| `/run/hadron-agent/root` | `root:hadron-agent-gateway` | 0750 |
| `/run/hadron-agent/control` | `hadron-agent-gateway:hadron-agent-control` | 0750 |
| `/run/hadron-agent/status` | `hadron-agent-gateway:agent` | 0750 |

Each socket is 0660. The agent cannot unlink or impersonate the root socket.

- [ ] `hadron-agent-provision.service` is root, oneshot, wants/starts after `network-online.target` and `hadron-agent-autoinstall.service`, runs before `hadron-agent-gateway.service` and `ly@tty1.service`, and refuses to start outside an image containing `/etc/hadron-agent/profile`.

- [ ] `hadron-agent-gateway.service` runs as `hadron-agent-gateway` with no supplementary privileged groups, no capabilities, `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes`, and write access only to its runtime status/control paths. Set `Restart=on-failure`, `RestartSec=2s`, `RestartSteps=5`, `RestartMaxDelaySec=30s`, and `ExecReload` to deliver SIGHUP. The gateway parses a replacement config completely before atomically swapping verifier state.

- [ ] The root-helper unit has `ConditionPathExists=/var/lib/hadron-agent/root/enabled`, listens only on its Unix socket, and permits arbitrary filesystem and child-process access because that is the explicit root-equivalent admin contract. Do not apply `RestrictAddressFamilies` to its children; the absence of a network listener, independent bearer validation, and the VM boundary are the intended controls. Set `Delegate=yes` so MCP commands receive child cgroups and add the same parse-then-swap SIGHUP reload behavior for admin digests.

- [ ] The globally installed user unit has `ConditionUser=agent`, runs `hadron-agent session` only in that user's manager, remains useful without display variables, restarts with 2-to-30-second backoff, and owns Cua as its child only after the environment is imported. Set `Delegate=yes`; production startup fails if the broker cannot create a child cgroup for MCP-owned terminal/process work.

- [ ] Add unit-shape tests using `systemd-analyze verify` in a container, then commit as `feat(agent): wire isolated gateway and broker services`.

## Task 6: Import the visible session and configure Ly autologin

- [ ] Create the agent-specific Ly drop-in. Clear inherited `ConditionPathExists` and `ConditionKernelCommandLine` values, then set only `ConditionKernelCommandLine=!install-mode`. Set `auto_login_user=agent`, `auto_login_session=i3`, and `auto_login_service=ly-autologin` during provisioning.

- [ ] Override `/usr/bin/start-desktop` only in `rootfs-agent`. Source the normal i3 environment, wait up to 10 seconds for `/run/user/$(id -u)/bus`, set `DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$(id -u)/bus`, call `hadron-agent-session-ready`, and then `exec i3 --config /etc/i3/config`. Do not call `dbus-run-session`; the lingering user manager and graphical session intentionally share one user bus.

- [ ] `hadron-agent-session-ready` validates inherited `DISPLAY`, `XAUTHORITY`, and `DBUS_SESSION_BUS_ADDRESS`, confirms the latter is the user-manager bus, and runs:

```bash
systemctl --user import-environment DISPLAY XAUTHORITY DBUS_SESSION_BUS_ADDRESS
systemctl --user restart hadron-agent-session.service
```

It never scrapes another process environment.

- [ ] Keep first-run/status/emergency commands in `90-hadron-agent.conf`; environment import belongs to the agent-only session launcher so it happens before i3 starts. Disable screen locking/DPMS in the agent profile without changing normal i3 configuration.

- [ ] Add `hadron-agent display-watchdog --user agent --grace 30s` and its root system service with `After=hadron-agent-provision.service ly@tty1.service` and `ConditionKernelCommandLine=!install-mode`. After Ly has started, require both an agent logind graphical session and an agent-owned i3 process. If either remains absent for 30 seconds, run `systemctl restart ly@tty1.service`; Ly startup then performs its one allowed autologin again. Rate-limit restarts to three per five minutes and report degraded readiness after the limit. The watchdog may only restart Ly on tty1; it must never launch X, i3, Xvfb, or a nested session itself.

- [ ] Document the local emergency chord as `Super+Shift+Escape`; its i3 spelling is `$mod+Shift+Escape` because `$mod` is `Mod4` in the inherited XLibre/i3 configuration.

- [ ] Test the launcher with a fake `systemctl` recorder and assert exactly those three names are imported. Test the watchdog's grace period, rate limit, install-mode exclusion, and Ly-only restart command. Commit as `feat(agent): attach and recover Cua on the foreground XLibre session`.

## Task 7: Implement first-run display, status, and emergency pause

- [ ] `hadron-agent-first-run` opens in `st` only when `/run/hadron-agent/first-run-token` exists. Wait up to 60 seconds for redacted gateway status, then show MCP URL, certificate fingerprint, ordinary token, and the text “shown once; store it now”. Wait for Enter, then unlink the tmpfs token. Never call logger/journal; if status times out, retain the token file so the panel retries on the next i3 start.

- [ ] Use `https://`, the current hostname, `.local`, the configured port, and `/mcp` as the canonical displayed URL when mDNS is enabled; otherwise display the first non-loopback address with the configured port. A CI-provided digest creates no plaintext token file and therefore no credential panel.

- [ ] The agent i3 config starts the panel and binds:

```text
bindsym $mod+Shift+Escape exec --no-startup-id /usr/bin/hadron-agent control toggle
```

- [ ] Override `hadron-status` only in `rootfs-agent`. Read redacted `status.json` and prepend exactly one of `Agent: Starting`, `Ready`, `Active`, `Paused`, or `Degraded`. `Active` means a remote `computer_use` call is currently executing, not merely that an MCP connection exists. Poll every second while active/paused and every five seconds otherwise.

- [ ] Add shell tests for missing token, token deletion, no token in captured stderr, all five status mappings, and the exact i3 binding. Commit as `feat(agent): add foreground status and emergency control`.

## Task 8: Add safe interactive and explicit zero-touch installation

- [ ] Write `install_script_test.sh` first. Fake block devices, OEM directory, and `HADRON_INSTALL_CMD`. Cover: no seed opens the prompt; rejection loops; `yes` writes the selected explicit disk; missing auto device fails; `install.auto=false` does not install; auto device plus `hadron_agent.enabled=true` installs; auto without enabled fails; existing unrelated cloud-config does not authorize wiping.

- [ ] `hadron-agent-install` asks only hostname and disk, displays the exact device in red, and requires typed `yes`. It writes `99_hadron-agent-install.yaml` containing explicit install device, auto/reboot, locked `agent` user, approved groups, Ly session, and `hadron_agent.enabled=true`.

- [ ] For unattended mode, use `hadron-agent provision inspect-seed --require-auto-install` to parse all OEM YAML. Proceed only when `install.auto=true`, `install.device` is a non-empty `/dev/...` path, and `hadron_agent.enabled=true`. Add the locked user fragment if the caller omitted it; never replace caller auth digest fields.

- [ ] In `91_agent_installer.yaml`, replace the existing desktop installer ExecStart only in install mode by writing `/etc/systemd/system/kairos-installer.service.d/20-agent.conf`; this is the interactive path.

- [ ] Add `hadron-agent-autoinstall.service` for programmatic CI on the ISO's normal default boot. Give it `ConditionKernelCommandLine=!install-mode`, `Before=hadron-agent-provision.service ly@tty1.service`, and `ExecCondition=/usr/bin/hadron-agent provision inspect-seed --require-auto-install --quiet`. Its sole ExecStart is `/usr/local/bin/hadron-agent-install --noninteractive`. Make provisioning and Ly start after this oneshot: when the condition is false it is skipped and ordinary non-destructive live mode proceeds; when true it remains active through `kairos-agent install` and reboot, so no live desktop races disk installation.

- [ ] Test the default-boot autoinstall unit with absent, malformed, disabled, device-less, and fully authorized seeds. Only the fully authorized seed may reach the fake install command. Then run:

```bash
bash test/agent/install_script_test.sh
sh -n rootfs-agent/usr/local/bin/hadron-agent-install
```

- [ ] Commit as `feat(agent): add safe interactive and seeded installation`.

## Task 9: Add user-facing build targets and prove profile isolation

- [ ] Extend `Makefile` with:

```make
AGENT_BASE_IMAGE ?= i3-desktop:dev
AGENT_IMAGE      ?= agent-desktop:dev
AGENT_WORK       := build/agent-desktop
AGENT_ISO_DIR    := $(AGENT_WORK)/iso

agent-image:
	$(MAKE) DESKTOP=i3 IMAGE=$(AGENT_BASE_IMAGE) image
	docker build -f Dockerfile.agent --target agent \
	  --build-arg BASE_IMAGE=$(AGENT_BASE_IMAGE) \
	  --build-arg VERSION=$(VERSION) \
	  -t $(AGENT_IMAGE) .

agent-iso: agent-image
	mkdir -p $(AGENT_ISO_DIR)
	rm -f $(AGENT_ISO_DIR)/*.iso
	docker run --rm --privileged \
	  -v /var/run/docker.sock:/var/run/docker.sock \
	  -v $(CURDIR)/$(AGENT_ISO_DIR):/output \
	  $(AURORA_IMAGE) build-iso --output /output/ docker:$(AGENT_IMAGE)
```

Guard an unset `VERSION` without emitting an empty build argument.

- [ ] Add `test/agent/profile_isolation.sh`. Build all three images and use `docker export` file lists to prove Sway/i3 lack `hadron-agent`, `cua-driver`, `/etc/hadron-agent`, `rootfs-agent` units, and agent Ly overrides, while the agent image contains all expected paths and its base session marker says i3/XLibre. Scan the exported generic agent filesystem and fail on an `hdn_u_`/`hdn_a_` bearer, a configured bearer digest, or a PEM private-key block.

- [ ] Document:

```bash
make DESKTOP=sway iso
make DESKTOP=i3 iso
make agent-iso VERSION=v1.2.3
```

Explain that tokens and keys are runtime/seed data, never Docker build args.

- [ ] Run `make -n agent-iso VERSION=v1.2.3`, `bash test/agent/profile_isolation.sh`, and existing `MILESTONE=M7 test/run.sh`. Commit as `build(agent): add separate agent image and ISO targets`.

## Task 10: Boot the live appliance manually before CI automation

- [ ] Build `make agent-iso VERSION=v0.1.0-dev` and boot the ISO with `virtio-vga` plus VNC using the Phase 1 harness.

- [ ] Verify live boot creates no writes to an attached blank disk, auto-logs into one XLibre session, displays the one-shot token/fingerprint, becomes ready on 7443, advertises mDNS, and lists exactly seven tools.

- [ ] Verify `id` through `terminal` reports `agent` and no forbidden groups. Verify admin calls are unavailable. Toggle `Super+Shift+Escape` while a long process runs; the call must cancel, process group must die, bar must show Paused, and local i3 must remain usable. Toggle again and verify readiness.

- [ ] Reboot the live ISO and confirm a new ephemeral bearer/certificate is generated. Generated proof stays under ignored `test/agent/artifacts`.

- [ ] Run `git status --short`. Expected: only the user's pre-existing untracked ISO remains. Commit any necessary fixes before Phase 4.
