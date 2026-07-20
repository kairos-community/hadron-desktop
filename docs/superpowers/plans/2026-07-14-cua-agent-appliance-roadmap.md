# Cua Agent Appliance Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a separate XLibre/i3-based Hadron agent ISO that boots a visible desktop, exposes exactly eight authenticated MCP tools, installs safely in interactive or CI mode, and supplies a reusable visible-VM UI test gate.

**Architecture:** Implement the feature in four release-blocking checkpoints. First prove native Cua/XLibre compatibility in a real graphical QEMU VM. Then build one static Go service binary with gateway, session-broker, root-helper, provisioning, and local-control subcommands. Add those components as a separate overlay on the existing i3 image. Finally turn the graphical VM into a stable downstream fixture and make all compatibility, privilege, install, recovery, and UI checks gate the third release ISO.

**Tech Stack:** XLibre/i3, Cua Driver `3cadb5f82e7d2ed071a2082764276ec872a52135`, AT-SPI 2.54.0, libXtst 1.2.5, Go 1.25, official MCP Go SDK 1.6.1, POSIX shell, systemd, Kairos OEM cloud-config, QEMU/QMP/VNC/noVNC, GitHub Actions.

## Global Constraints

- The normal Sway and i3 images must not contain an agent binary, agent service, bearer digest, TLS key, autologin override, or Cua runtime.
- The agent image must be built from `DESKTOP=i3`; no task may add Xvfb, a nested compositor, a headless X server, or a second graphical session.
- Cua must be compiled from the pinned source revision for musl. Never copy the upstream glibc release binary.
- The only public action endpoint is MCP Streamable HTTP at `/mcp`; it advertises exactly `computer_use`, `terminal`, `process`, `read_file`, `search_files`, `write_file`, and `patch`.
- LAN-facing MCP is HTTPS-only. Plain HTTP is accepted only with the explicit loopback development flag.
- The ordinary identity is the locked-password `agent` user and is never a member of `admin`, `sudo`, or `docker`.
- Administrator support is disabled unless a distinct `hdn_a_` digest is provisioned. The root helper must validate the raw administrator bearer itself.
- The generic ISO contains no bearer plaintext, bearer digest, or private TLS key. Machine state is generated at boot or supplied through a separate `cidata` seed.
- Every graphical acceptance check must exercise the DRM-backed XLibre session shown by QEMU VNC. A test that passes only in the existing Sway headless harness is not evidence for this feature.
- Keep `kairos-hadron-main-core-amd64-generic-v0.0.0.iso` and all unrelated working-tree changes untouched.

---

## Delivery sequence

The phase plans are sequential. Each phase is a usable checkpoint and must be green before work starts on the next phase.

| Phase | Plan | Exit condition |
|---|---|---|
| 1 | [Native XLibre/Cua compatibility](2026-07-14-cua-agent-phase-1-xlibre-compatibility.md) | Pinned Cua controls GTK and Chromium on the real XLibre display, and its framebuffer agrees with QMP. |
| 2 | [MCP services and privilege boundary](2026-07-14-cua-agent-phase-2-mcp-services.md) | The static service binary passes unit, race, MCP contract, auth, privilege, pause, and process-lifecycle tests. |
| 3 | [Agent appliance profile](2026-07-14-cua-agent-phase-3-appliance-profile.md) | `make agent-iso` produces an opt-in live/installable appliance with autologin, provisioning, TLS, status, and emergency control. |
| 4 | [UI E2E fixture and release gates](2026-07-14-cua-agent-phase-4-e2e-release.md) | A clean disk installs from `cidata`, survives recovery/reboot tests, emits the fixture descriptor, and the release workflow publishes exactly three gated ISOs. |

## Task 1: Complete Phase 1

- [ ] Execute every checkbox in the Phase 1 plan.
- [ ] Record the successful Cua revision, Cua version, XLibre version, AT-SPI version, screenshot dimensions, and QMP dimensions in `test/agent/artifacts/compatibility.json`.
- [ ] Stop if the real-XLibre compatibility gate fails; do not replace it with a hidden display backend.

## Task 2: Complete Phase 2

- [ ] Execute every checkbox in the Phase 2 plan after Phase 1 is green.
- [ ] Confirm `go test -race ./...` passes from `agent/`.
- [ ] Confirm a built binary has no dynamic dependencies and its MCP client lists exactly the eight approved names.

## Task 3: Complete Phase 3

- [ ] Execute every checkbox in the Phase 3 plan after Phase 2 is green.
- [ ] Build all three local variants and inspect their file lists to prove profile isolation.
- [ ] Boot the agent ISO through VNC and manually exercise first-run display plus pause/resume once before moving to release automation.

## Task 4: Complete Phase 4

- [ ] Execute every checkbox in the Phase 4 plan after Phase 3 is green.
- [ ] Run the native compatibility, MCP/privilege, install/recovery, GTK, and Chromium gates from a clean workspace.
- [ ] Verify the downstream fixture documentation by running its sample test against the emitted descriptor.
- [ ] Verify release asset counts, checksums, SBOMs, and manifests before marking the feature complete.

## Final verification

Run from the repository root:

```bash
make DESKTOP=sway image
make DESKTOP=i3 image
make agent-image
(cd agent && go test -race ./...)
test/agent/run.sh compatibility
test/agent/run.sh contract
test/agent/run.sh install
test/agent/run.sh recovery
test/agent/run.sh ui
```

Expected: every command exits zero; the two standard images contain no path matching `/usr/bin/hadron-agent`, `/usr/bin/cua-driver`, `/etc/hadron-agent`, or `/usr/lib/systemd/*/hadron-agent*`; the agent fixture descriptor contains all eight documented fields and is mode `0600`.
