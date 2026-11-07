# System extensions as a first-class feature

## Intent

A Hadron Desktop user cannot change `/usr`: it is the signed, immutable
image. Flatpak covers desktop apps, and Distrobox covers a mutable Linux in
a container. Neither covers software that must run on the host: a VPN
daemon (Tailscale), firmware updates (fwupd), a kernel module (ZFS), or a
tool that must see the real system.

Kairos already has the mechanism for this: systemd system extensions
(sysexts), which `kairos-agent sysext` downloads, enables and merges over
`/usr`. This design makes them something a desktop user can find and use
without reading Kairos documentation:

- browse the extensions that a catalog publishes, and install one by name;
- install one by URL (`oci://`, `https://`), as Kairos supports;
- see what is installed, and turn each one on, off, or remove it.

## What Kairos gives us (read from kairos-io/kairos master)

- `kairos-agent sysext install [--catalog URL]... [--version V] URI|NAME`,
  plus `list`, `enable`, `disable`, `remove`, each with
  `--active|--passive|--recovery|--common` and `--now`
  (`agent/pkg/cmd/root.go`).
- A catalog is a JSON document (`sdk/extensions/catalog.go`):
  `{repo, layers: [{name, latest, tags: [{tag, sysext: {amd64: {oci}}}]}]}`.
  Every reference is pinned by digest. `--version` takes an exact tag or a
  semver constraint. The built-in default catalog is
  `https://kairos-io.github.io/hadron-layers/releases.json`. That catalog also
  carries `title` and `description`, which Kairos ignores and we can show.
- `extensions.catalogs` in the cloud config replaces the default list.
  `extensions.ignore_signatures` merges unsigned extensions; without it,
  systemd refuses them (`sdk/types/extensions/extensions.go`).

## What the published catalog contains today (checked 2026-11-07)

hadron-layers publishes six names: drbd, fwupd, git, gpg, tailscale and zfs.
They are unsigned GPT images with an erofs root and verity partitions, and
`ID=_any`, `ARCHITECTURE=x86-64`. I opened three of them (reported upstream as
kairos-io/kairos#6020):

- **tailscale** is self-contained: two binaries, a unit and a tmpfiles file.
- **gpg** ships `libgcrypt.so.20`, `libgpg-error.so.0` and `libsqlite3.so.0`.
  The desktop image already has all three (gnome-keyring links libgcrypt). An
  overlay puts the extension's copy over the desktop's, so enabling gpg
  replaces gnome-keyring's crypto library with a build from another toolchain.
- **fwupd** contains only its extension-release file and a metainfo XML. It
  has no binary. This is an upstream bug to file on kairos-io/kairos.

So the catalog works, but installing blindly can break the desktop. The
conflict check below is not optional.

## Design

### One privileged path: hadron-updated

Installing an extension changes what runs as root, so it goes through the
same root daemon, D-Bus API and polkit rule as upgrades:

- New bus methods on `hadron-updated`:
  - `Extensions()`: the installed extensions, their state and their source.
  - `Catalog()`: the merged catalog entries, each marked installed or not.
  - `InstallExtension(nameOrURI, version)`, `SetExtensionEnabled(name, on)`
    and `RemoveExtension(name)`.
  - They report progress on the existing `Progress` signal.
- New polkit action `io.kairos.hadron-update.extensions`. The existing rule
  already gives every `io.kairos.hadron-update.*` action to the `admin` group
  only. Flatpak-sandboxed callers are refused, as for `Apply`.
- The daemon runs `kairos-agent sysext`. It never writes
  `/var/lib/kairos/extensions` itself.
- One job at a time, shared with upgrades: an extension install waits for, or
  refuses during, an upgrade (the existing `Busy()`).

### Where an extension lives across upgrades (my call)

Install to `--common` and enable with `--common --now`. The extension is
merged at once, survives A/B upgrades, and stays in the previous system after
a rollback. The cost: an extension that depends on a library the next release
drops breaks after that upgrade. The conflict check runs again on the new
system (see "After an upgrade").

### Conflict check

Before enabling, the daemon lists the extension's files (`systemd-dissect
--list`, or a read-only loop mount of the erofs partition when that is
missing) and compares them with the running `/usr`:

- a file the base image does not have: fine;
- a file the base has with identical content: fine;
- a file the base has with **different** content: refuse, and name the files.
  `--force` (CLI only, with a warning) overrides this.
- an `ARCHITECTURE` other than x86-64, or an `ID` other than `_any`/`hadron`:
  refuse.

The check runs again for every enabled extension at the first boot after an
upgrade. A conflicting extension is disabled for the new system and the user
gets a notification. Nothing is removed.

### Catalogs

`/etc/hadron-desktop/extensions.conf`, one `catalog=URL` per line, searched in
order. The default is our own catalog (below) first, then hadron-layers. The
daemon passes them as `--catalog` flags, so a system cloud config is never
edited.

Installing by URL (`oci://`, `https://`, a local path) needs advanced mode,
the same `advanced=` switch in `update.conf` that allows any OCI image for
upgrades (on by default). The CLI and the menu say plainly that the extension
runs as root and that nobody checked it.

### Signatures (decided)

The hadron-layers images are unsigned, so they merge only with
`extensions.ignore_signatures: true`. This design sets that in the image's
Kairos config for the normal (non-Trusted-Boot) install. It relies on
digest-pinned catalog references, admin-only installs and the conflict
check. A Trusted Boot line, if we ship one, keeps signatures on and uses only
a catalog we sign.

### Commands

`hadron-update ext` (also installed as `hadron-ext`):

```
hadron-ext                     # installed extensions and their state
hadron-ext browse [WORD]       # catalog entries, with installed marks
hadron-ext info NAME           # description, versions, files it adds
hadron-ext install NAME[@VER]  # or a URL; asks, checks conflicts, merges
hadron-ext disable|enable NAME
hadron-ext remove NAME
```

### Desktop UI, on every variant

- The Hadron menu gets an **Extensions** entry, shown through `hadron-pick`
  like the other entries: the catalog with ✓ on the installed ones. Picking one
  runs the install or remove in a terminal, as the Update entry does.
- The tray's menu gets an **Extensions…** item that opens the same thing.
- The shortcut sheet and the AGENTS.md feature table get a row.
- The agent overlay removes the command and the bus methods' polkit action:
  an appliance must not take root code from a menu.

### Our own catalog (phase 2)

`release.yml` builds desktop extensions from stages of our own Dockerfile,
against the same toolchain as the release, with `auroraboot sysext`. Then it
pushes them to `ghcr.io/kairos-community/hadron-desktop/sysext/<name>` and
publishes `extensions.json` in the Kairos catalog format to GitHub Pages.
Because these extensions are built with the release, they share its
libraries, so they cannot shadow them with other builds. The first ones are fwupd,
tailscale and developer tools (git, gpg, build tools).

## Phases

1. The daemon, CLI, conflict check and menu and tray entries, against
   hadron-layers. Go unit tests with a fake `kairos-agent`, and a test script.
2. Our own catalog in the release pipeline.
3. Re-checking extensions at the first boot after an upgrade.

## Decisions (owner, 2026-11-07)

1. `extensions.ignore_signatures: true` is set on the normal install, not on
   a Trusted Boot line.
2. Our catalog publishes fwupd, tailscale and developer tools (git, gpg and
   build tools for the host), all built against the release's own libraries.
3. The hadron-layers findings are filed upstream as kairos-io/kairos#6020.
