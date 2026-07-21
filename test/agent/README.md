# The Hadron Cua agent VM fixture

`test/agent/run.sh fixture` boots the agent appliance as a **long-lived,
VNC-visible virtual machine** and hands you a small JSON descriptor pointing at
its MCP gateway, its VNC console, and its QMP control socket. `run.sh stop`
powers it back down and keeps the artifacts.

The point of the fixture is that a *downstream* repository — a UI, an agent
framework, anything that wants to drive a real desktop — can get a real,
graphical Linux session under test without knowing anything about Kairos,
AuroraBoot, QEMU, or how the appliance authenticates. All it needs is the
descriptor and the seven public MCP tools.

Everything here drives the DRM-backed XLibre/i3 session that QEMU actually
scans out. There is no headless mode and no nested display: if a test passes,
a human watching the VNC console saw it happen.

---

## Prerequisites

The `fixture` subcommand shells out to exactly these host commands (see
`_agent_require_bins` in `run.sh` and the dependency check at the top of
`make-seed.sh`):

| Command | Why |
|---|---|
| `qemu-system-x86_64`, `qemu-img` | boots the guest, creates its qcow2 disk |
| OVMF / `edk2-ovmf` firmware | UEFI is **mandatory**; there is no BIOS fallback |
| `docker` | runs AuroraBoot to build the per-VM provisioning ISO |
| `openssl` | mints the bearers and captures the TLS leaf |
| `curl` | polls `/readyz` through the pinned CA |
| `python3` | JSON emission, port allocation, TCP probes |
| `xorriso` | builds the `cidata` seed in `make-seed.sh` |

Optional:

- `python3-pil` — lets `_agent_screenshot` convert QMP `.ppm` screendumps to
  `.png`. Without it you still get the `.ppm`.
- `websockify` plus a noVNC checkout — only needed when you set `NOVNC=1`.
  `hdn_fixture_novnc_root` looks for `vnc.html` under `~/novnc`,
  `/usr/share/novnc`, or `/usr/share/webapps/novnc`.
- A Go toolchain — only needed to build the `mcp-smoke` client. The fixture
  itself never invokes `go`.

On Debian/Ubuntu:

```bash
sudo apt-get install -y qemu-system-x86 qemu-utils ovmf xorriso jq \
                        openssl curl python3 python3-pil websockify novnc
```

KVM is used when `/dev/kvm` exists and multi-threaded TCG otherwise, so the
fixture works on a runner without nested virtualization — just slowly.

---

## Starting and stopping

```bash
# Boot the appliance and print ONLY the descriptor path on stdout.
descriptor="$(test/agent/run.sh fixture)"

# ... drive the VM through the descriptor ...

# Graceful ACPI powerdown, escalating after 20s. Artifacts are kept.
test/agent/run.sh stop
```

`fixture` blocks while the VM runs, so in CI or a shell you want it in the
background:

```bash
test/agent/run.sh fixture > /tmp/descriptor.path &
```

Its **stdout carries nothing but the descriptor path** — all progress goes to
stderr — precisely so a caller can capture it without parsing logs.

`fixture` takes an optional container image argument (default
`$AGENT_IMAGE`, itself defaulting to `agent-desktop:dev`, which
`make agent-image` produces). It builds a fresh provisioning ISO from that
image on every boot, because the per-VM bearer digests have to be *inside*
the ISO — see [Secret handling](#secret-handling).

### Knobs

All are environment variables read by `cmd_fixture`:

| Variable | Default | Meaning |
|---|---|---|
| `AGENT_IMAGE` | `agent-desktop:dev` | container image to build the ISO from |
| `FIXTURE_PROV_ISO` | *(unset)* | reuse a prebuilt provisioning ISO; this run's cloud-config is injected into a copy of it |
| `FIXTURE_RUNTIME` | `test/agent/runtime/fixture` | per-VM scratch (disk, seed, PID files, QMP socket) |
| `FIXTURE_ART` | `test/agent/artifacts/fixture` | artifact directory named in the descriptor |
| `FIXTURE_VNC` | `25` | VNC display number, so the port is `5925` |
| `NOVNC` | `0` | set to `1` to also start a websockify/noVNC console |
| `FIXTURE_NOVNC_PORT` | `6090` | noVNC HTTP port |
| `FIXTURE_DISK_SIZE` | `32G` | guest disk size. 24G was not enough to install a browser plus its Flatpak runtimes: flatpak reported "not enough disk space" while the host had 45G free. |
| `FIXTURE_BOOT_TIMEOUT` | `900` | seconds to wait for gateway readiness |
| `MEM` / `CPUS` | `4096` / `4` | guest memory (MiB) and vCPUs |

Overriding `FIXTURE_RUNTIME` and `FIXTURE_ART` together is what lets you run
several fixtures side by side; `stop` reads the same two variables to find the
VM it should power down.

The MCP host port is **allocated dynamically** (`hdn_agent_alloc_port` binds an
ephemeral loopback port and reads back the number), so never hardcode it — read
`mcp_url` from the descriptor.

Everything binds to `127.0.0.1`. Nothing the fixture starts is reachable off
the host.

---

## The descriptor

The descriptor is frozen by `test/agent/fixture.schema.json`: **exactly eight
string fields, no others** (`additionalProperties: false`). It is written
atomically at mode `0600` by `hdn_fixture_emit_descriptor`, which refuses to
write at all if any value looks like a bearer token.

```json
{
  "mcp_url": "https://127.0.0.1:41573/mcp",
  "bearer_token_file": "/abs/path/test/agent/runtime/fixture/seed/user.token",
  "ca_certificate_file": "/abs/path/test/agent/artifacts/fixture/ca.pem",
  "tls_fingerprint": "3f2a…64 lowercase hex chars…",
  "vnc_address": "127.0.0.1:5925",
  "novnc_url": "",
  "qmp_socket": "/abs/path/test/agent/runtime/fixture/qmp.sock",
  "artifact_directory": "/abs/path/test/agent/artifacts/fixture"
}
```

Invariants the schema enforces, and that consumers may rely on:

- `mcp_url` is `https://` and ends in `/mcp`.
- `tls_fingerprint` is 64 lowercase hex characters — the SHA-256 of the
  **DER** form of the leaf.
- `bearer_token_file`, `ca_certificate_file`, `qmp_socket`, and
  `artifact_directory` are absolute paths.
- `novnc_url` is **present but empty** when noVNC is disabled. It is never
  omitted; a consumer can read it unconditionally.

`test/agent/descriptor_test.sh` is the executable statement of this contract.
It rejects a missing field, an extra field, a relative path, an `http://` MCP
URL, an `mcp_url` not ending in `/mcp`, a malformed fingerprint, a
world-readable descriptor, a world-readable bearer file, and a descriptor whose
text contains token-shaped content. Run it with no VM:

```bash
bash test/agent/descriptor_test.sh
```

The same eight fields are the Go `smoke.Descriptor` struct, loaded with
`DisallowUnknownFields` so a drifted producer fails loudly rather than
silently ignoring a field.

---

## TLS trust

The appliance serves MCP over HTTPS with a **self-signed leaf it generates on
first boot**. There is no CA to pre-trust, so the fixture does
trust-on-first-use at a moment when nothing else has spoken to that port yet:

```bash
openssl s_client -connect "127.0.0.1:$MCP_PORT" -showcerts </dev/null \
  2>/dev/null | openssl x509 -out "$CA_CERT"
openssl x509 -in "$CA_CERT" -outform DER | sha256sum | awk '{print $1}' \
  > "$FINGERPRINT_FILE"
```

The captured PEM becomes `ca_certificate_file` (mode `0600`) and its DER digest
becomes `tls_fingerprint`.

Capturing the certificate is **not** the same as disabling verification. Every
subsequent request verifies against that one pinned certificate, with hostname
checking intact — the leaf carries a `127.0.0.1` SAN, so `--cacert` alone is
sufficient and `curl -k` is never used:

```bash
curl --fail --cacert "$CA_CERT" "https://127.0.0.1:$MCP_PORT/readyz"
```

A Go consumer does the same thing by loading *only* that file into an otherwise
empty `x509.CertPool`. `NewTLSClient` never sets `InsecureSkipVerify` and pins
`MinVersion: tls.VersionTLS13`. If you write your own client, do both.

Compare `tls_fingerprint` against the certificate you actually negotiated if
you want defense against a port collision handing you somebody else's TLS
service.

---

## Secret handling

`make-seed.sh` mints two bearers per VM: a user-class `hdn_u_…` and an
admin-class `hdn_a_…`, each 32 crypto-random bytes in unpadded base64url,
mirroring `agent/internal/auth.Generate` exactly.

The rules the whole harness obeys, and that you should keep obeying:

- **Only digests travel.** The cloud-config embedded in the ISO carries
  `sha256:<hex>` digests of the complete bearer strings. Plaintext never enters
  the ISO, the guest image, or any config file. `test/agent/make_seed_test.sh`
  proves this by reading `/user-data` back out of the built ISO.
- **Plaintext lives in exactly two files**, `user.token` and `admin.token`
  under the seed directory, both mode `0600`. `lib/common.sh` sets `umask 077`
  as its first executable statement, so this holds even for files created
  before anyone thinks about permissions.
- **A bearer is never an argv element of an external command**, so it cannot
  appear in `ps`. It flows only through shell variables, builtins, and pipes.
- **The descriptor points at the token file; it never contains the token.**
  The emitter refuses to write a descriptor matching `hdn_[ua]_[A-Za-z0-9_-]{8,}`.
- **Nothing re-enables shell tracing** after a token exists. `set -x` would
  route a bearer straight to a CI console and defeat every `0600` above.
- **Anything you print or upload goes through `hdn_agent_redact`** (stream) or
  `hdn_agent_redact_str` (single argument), which rewrite both token classes to
  `hdn_u_<redacted>` / `hdn_a_<redacted>`.

Where they live for a default fixture:

```text
test/agent/runtime/fixture/seed/user.token    # descriptor's bearer_token_file
test/agent/runtime/fixture/seed/admin.token   # admin bearer, NOT in the descriptor
```

The admin bearer is deliberately outside the descriptor: the descriptor is the
*ordinary* credential handed to a downstream consumer, and privilege-boundary
tests must be able to prove that credential cannot reach root. Tools that need
both take the admin file as a separate explicit argument
(`mcp-smoke --admin-bearer-file`).

The whole `test/agent/runtime/` and `test/agent/artifacts/` trees are
`.gitignore`d. Never upload `runtime/` from CI — that is where the plaintext
tokens are.

---

## Uploading and launching an application

There is no separate upload channel. You put an application into the guest with
the same public tools you use for everything else.

**Upload** with `write_file`. It takes `encoding: "base64"`, an explicit
`mode`, and `create_parents`, which together are enough to land an executable:

```json
{
  "path": "/home/agent/e2e/bin/myapp",
  "content": "<base64 of the binary>",
  "encoding": "base64",
  "mode": "0755",
  "create_parents": true
}
```

`/home/agent/e2e` is the conventional working directory for fixture material;
it is owned by the unprivileged `agent` user, which is also the user that owns
the graphical session.

For anything larger than a small binary, prefer writing an archive and
unpacking it with `terminal`, rather than one enormous base64 payload.

**Launch** with `process`, not `terminal`. `terminal` runs a command to
completion and returns its output; a GUI application never completes.
`process` with `action: "start"` returns a `process_id` you can `poll`,
`write` to, and `terminate`:

```json
{ "action": "start", "command": "/home/agent/e2e/bin/myapp", "args": [] }
```

Each started process gets its own cgroup leaf named `hadron-proc-<id>`, which
is the authoritative kill boundary: `terminate` kills the leaf, so nothing the
application spawned — not even a `setsid` grandchild — survives it. The
contract suite proves exactly this.

The application appears in the **same visible XLibre/i3 session** the VNC
console shows. Use `computer_use` with `action: "list_applications"` to find
it and `"focus_application"` to bring it forward before driving it.

---

## Public computer-use assertions

Everything a downstream test does goes through the seven public MCP tools:

```text
computer_use   terminal   process   read_file
search_files   write_file patch     browser
```

`browser` is the eighth, added by the 2026-07-20 browser-tool amendment. The
authoritative list is `api.ToolNames()`; the contract suite asserts that both
the user and the admin credential classes list exactly these eight names and
that the two lists are identical. There is no private or admin-only tool.

`computer_use` actions available for driving and asserting:

| Action | Use |
|---|---|
| `capture` | screenshot the screen or one window (`scope`, `pid`, `window_id`) |
| `accessibility` | read the AT-SPI tree — the assertion surface for native apps |
| `click`, `double_click` | pointer, at absolute `x`/`y`, `button` selectable |
| `drag` | `from_x`/`from_y` to `to_x`/`to_y` |
| `scroll` | `direction` plus `amount` in notches |
| `type` | literal text |
| `key` | a named key or chord |
| `wait` | settle between gestures |
| `list_applications`, `focus_application` | find and raise a window |

How to assert, in order of preference:

1. **Accessibility state, re-read after the gesture.** Never reuse an
   `element_index` from before an interaction — the tree may have been rebuilt.
2. **The application's own output** — a JSON file it writes, read back with
   `read_file`. State the app itself vouches for is stronger than state you
   inferred from pixels.
3. **Pixels**, via `capture`. Use these to prove a gesture reached the visible
   session, and cross-check against an independent QMP `screendump` of the same
   moment when it matters: two captures of the same framebuffer taken through
   two unrelated paths is what proves the desktop under test is the desktop the
   VM is scanning out.

For web content, prefer `browser` over `computer_use`. Flatpak Chromium
publishes no AT-SPI DOM tree — `computer_use accessibility` scoped to a
Chromium window returns just the window node — so driving a page by
coordinates silently depends on window geometry, viewport width, and
responsive breakpoints. `browser` addresses elements by ref from the page's own
accessibility tree instead, with actions `navigate`, `snapshot`, `click`,
`type`, `press`, `scroll`, `back`, and `text`.

`browser` reporting `BROWSER_UNAVAILABLE` is a **valid, passing** outcome for a
plumbing check: the appliance does not ship a running browser, and that code is
itself the proof that the window-resolve path executed. Only demand a live page
in a suite that opened one.

---

## Artifact paths

Each gate writes under `test/agent/artifacts/<gate>/`, and the fixture's own
directory is exactly what the descriptor's `artifact_directory` points at, so a
downstream test can drop its own files beside the harness's:

```text
test/agent/artifacts/fixture/
  fixture.json          # the descriptor (0600)
  ca.pem                # TOFU-captured leaf (0600)
  tls-fingerprint.txt   # its DER SHA-256
  serial.log            # guest serial console
  qemu-launch.log       # tools/vm.sh + QEMU stdout/stderr
  make-seed.log         # make-seed.sh stderr
  novnc.log             # only when NOVNC=1
  mcp-smoke.json        # written by mcp-smoke contract mode
```

Other gates use the same layout under `artifacts/contract/`,
`artifacts/install/`, `artifacts/generic/`, and `artifacts/compatibility/`,
adding `.ppm`/`.png` QMP screendumps at their decision points (for example
`before-install.ppm`, `installed-ready.ppm`, `after-reboot.ppm`, and `fail.ppm`
on any failure).

`mcp-smoke` names its results per mode — `mcp-smoke.json`,
`mcp-persist-write.json`, `mcp-persist-verify.json` — so several invocations
sharing one artifact directory do not clobber each other. Those files carry
check names, pass/fail, and redacted detail strings only: never a bearer, a
command line, or file contents.

`runtime/` is not an artifact directory. It holds the tokens, the seed, the
qcow2 disk, and the QMP socket. Upload `artifacts/`, never `runtime/`, and pipe
text artifacts through `hdn_agent_redact` first.

---

## A minimal Go client invocation

The ready-made client is `test/agent/cmd/mcp-smoke`. Its `main` package lives
outside the agent Go module so it can sit under `test/agent`, and all its
assertion logic lives in `agent/internal/smoke` — so it must be copied into the
module tree before building (which is what `_agent_build_mcp_smoke` does):

```bash
mkdir -p agent/cmd/mcp-smoke
cp test/agent/cmd/mcp-smoke/main.go agent/cmd/mcp-smoke/main.go
(cd agent && CGO_ENABLED=0 go build -trimpath -o /tmp/mcp-smoke ./cmd/mcp-smoke)
rm -rf agent/cmd/mcp-smoke

/tmp/mcp-smoke \
  --descriptor test/agent/artifacts/fixture/fixture.json \
  --admin-bearer-file test/agent/runtime/fixture/seed/admin.token
```

Flags: `--descriptor` (required), `--admin-bearer-file` (required),
`--mode` (`contract`, `persist-write`, `persist-verify`; default `contract`),
`--marker` (required by the two persist modes), `--ready-timeout` (default
`60s`), `--call-timeout` (default `30s`).

Exit codes are deterministic and are the contract with whatever runs it:

| Code | Meaning |
|---|---|
| `0` | every assertion held |
| `1` | an assertion failed |
| `2` | descriptor or arguments were invalid |
| `3` | transport or readiness failure — the assertion never got evaluated |

Distinguishing 1 from 3 is the point: a red gate that is really a boot timeout
should not read as a privilege-boundary regression.

To write your own client, the whole integration is five steps — read the
descriptor, pin the CA, inject the bearer, connect, call:

```go
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type descriptor struct {
	MCPURL            string `json:"mcp_url"`
	BearerTokenFile   string `json:"bearer_token_file"`
	CACertificateFile string `json:"ca_certificate_file"`
	TLSFingerprint    string `json:"tls_fingerprint"`
	VNCAddress        string `json:"vnc_address"`
	NoVNCURL          string `json:"novnc_url"`
	QMPSocket         string `json:"qmp_socket"`
	ArtifactDirectory string `json:"artifact_directory"`
}

// bearerRT injects the bearer on every request. It must never log the value.
type bearerRT struct {
	base   http.RoundTripper
	bearer string
}

func (rt bearerRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+rt.bearer)
	return rt.base.RoundTrip(req)
}

func main() {
	// HADRON_AGENT_FIXTURE is the descriptor path the fixture exports.
	raw, err := os.ReadFile(os.Getenv("HADRON_AGENT_FIXTURE"))
	if err != nil {
		log.Fatal(err)
	}
	var d descriptor
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields() // fail loudly if the producer drifted
	if err := dec.Decode(&d); err != nil {
		log.Fatal(err)
	}

	// Trust ONLY the captured leaf. Never InsecureSkipVerify.
	pem, err := os.ReadFile(d.CACertificateFile)
	if err != nil {
		log.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		log.Fatal("no usable certificate in ", d.CACertificateFile)
	}
	tok, err := os.ReadFile(d.BearerTokenFile)
	if err != nil {
		log.Fatal(err)
	}

	httpClient := &http.Client{Transport: bearerRT{
		base: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS13,
		}},
		bearer: strings.TrimSpace(string(tok)),
	}}

	client := mcp.NewClient(&mcp.Implementation{Name: "my-ui-e2e", Version: "v1"}, nil)
	sess, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   d.MCPURL,
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		log.Fatal(err)
	}
	defer sess.Close()

	tools, err := sess.ListTools(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}
	for _, t := range tools.Tools {
		log.Println(t.Name) // the eight public tools
	}
}
```

`test/agent/examples/descriptor-smoke.sh` is the shell equivalent: it validates
a descriptor against the schema and runs the `mcp-smoke` contract suite against
it.

---

## Downstream CI

`.github/workflows/reusable-agent-ui-e2e.yml` packages all of the above as a
reusable workflow. It requests `contents: read` and nothing else, and takes no
secrets, so calling it cannot grant this fixture write access to your
repository.

```yaml
jobs:
  agent-ui-e2e:
    uses: mudler/hadron-desktop/.github/workflows/reusable-agent-ui-e2e.yml@main
    with:
      agent_iso_url: https://github.com/.../hadron-desktop-agent-v0.1.0-amd64.iso
      agent_iso_sha256: 3f2a…            # mandatory; an unverified ISO fails the job
      hadron_ref: v0.1.0
      test_command: go test ./e2e/... -run TestAgentUI
```

Your `test_command` runs with `HADRON_AGENT_FIXTURE` set to the descriptor
path, in your repository's checkout. The workflow always stops the VM
(`if: always()`) and uploads redacted artifacts whether you pass or fail.

### How a prebuilt ISO gets this VM's credentials

The appliance learns its bearer digests from a cloud-config that AuroraBoot
writes to the ISO root as `/config.yaml`; on boot,
`rootfs-agent/system/oem/89_agent_cloudconfig.yaml` publishes it into `/oem`
where the provisioner reads it. A released ISO — the kind `agent_iso_url` points
at — carries none, because `make agent-iso` builds the appliance without one.

Left alone, that fails in the worst possible way: the guest boots, mints a
throwaway first-run token, and reports `/readyz` 200, while the descriptor's own
bearer gets 401. Everything looks healthy except your calls.

So `FIXTURE_PROV_ISO` does not use the ISO as-is. `fixture` remasters a per-run
copy, mapping this VM's freshly minted cloud-config to `/config.yaml` at the ISO
root (`_agent_inject_cloud_config`, using `xorriso -boot_image any replay` so
the El Torito BIOS/UEFI records and the MBR survive — without that replay the
output is a valid filesystem no firmware will boot). The original ISO is never
modified.

The reusable workflow still preflights the credential against `/mcp` before
running your suite, so if this ever regresses you get one clear message instead
of a confusing failure inside your own tests.
