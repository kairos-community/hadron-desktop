# Encrypt your files (passphrase at every boot)

## Intent

The installer can encrypt the partition that holds the user's files with a
passphrase the user chooses. The machine asks for it at every boot. This is
the classic laptop disk encryption: it protects a stolen laptop or disk that
is switched off.

Hadron Desktop has two encryption modes, and this is the one for the normal
edition:

1. Normal edition (this note): LUKS2 with a passphrase, through kcrypt
   `method: passphrase` (kairos-io/kairos#6022).
2. Trusted Boot edition (#61): a TPM key sealed to PCRs, Kairos-native, with
   kcrypt's `passphrase_fallback`.

kcrypt's non-UKI default, a random key in TPM NV memory, is not offered. It
has no PCR policy and an empty key password, so any system booted on the
machine (a USB stick, say) can read the key, and the encryption only protects
a disk taken out of the machine.

## What the user sees

The step "Encrypt your files?" comes after the target disk on every machine.
It does not look for a TPM. "no" is selected by default. On "yes" the user
types a passphrase (12 characters or more, with a strength hint under the
field) and then types it again. The review screen shows
`encrypt on (passphrase at every start)`.

The step says, in plain words:

- what is encrypted: the partition with /home and /var;
- that the computer asks for the passphrase every time it starts;
- what it protects: the files on a laptop or disk stolen while switched off,
  and that the system itself is not encrypted (it holds nothing personal);
- that a forgotten passphrase means the files are gone, and nobody can
  recover them.

Non-interactive installs use `HADRON_ENCRYPT=1` and `HADRON_DISK_PASSPHRASE`.

The step is not shown on a Trusted Boot install medium (immucore writes
`/run/cos/uki_install_mode` there): that edition encrypts with its TPM-sealed
key, and `kcrypt.method: passphrase` would replace it. `HADRON_ENCRYPT=1` is
refused there.

## How it works

1. The rendered cloud-config (`/oem/99_hadron-user.yaml` on the live system,
   or `HADRON_CONFIG_OUT`) gets

   ```yaml
   install:
     reboot: false
     encrypted_partitions:
       - COS_PERSISTENT
   kcrypt:
     method: passphrase
     passphrase_file: /run/hadron-install/passphrase
   ```

   and an initramfs stage that writes the marker (below). The passphrase is
   not in it: kairos-agent copies this file to the unencrypted COS_OEM
   (`/oem/90_custom.yaml` on the installed system).
2. Just before kairos-agent runs, the installer writes the passphrase to
   `/run/hadron-install/passphrase`: a tmpfs, a 0700 directory, a 0600 file,
   no trailing newline. This is the hand-off kcrypt reads
   (`kcrypt.passphrase_file`).
3. kairos-agent encrypts COS_PERSISTENT with kcrypt's passphrase encryptor: a
   LUKS2 container labelled `COS_PERSISTENT_LUKS`, one keyslot, argon2id with
   2 s of work (`--pbkdf argon2id --iter-time 2000`), LUKS2 subsystem
   `kcrypt:passphrase`. It opens the container with the same file to make the
   filesystem, and closes it at the end of its hook.
4. When kairos-agent exits, the installer deletes the file and its directory
   (also when the install failed), then checks the disk before it reboots:
   - `lsblk` finds the crypto_LUKS partition labelled `COS_PERSISTENT_LUKS`
     (or `COS_PERSISTENT` on kcrypt before kairos-io/kairos#4403);
   - `blkid -s SUBSYSTEM` is `kcrypt:passphrase`. A kairos-agent without
     #6022 ignores `kcrypt.method` and would have used the TPM, so this check
     stops such an install with an error rather than reboot into it;
   - `cryptsetup luksDump --dump-json-metadata`: every keyslot is argon2id;
   - `cryptsetup open --test-passphrase --key-file -`, the passphrase on
     stdin, opens it.
   Any failure stops on the "install halted" screen with the error in the log.
   On success the installer reboots.
5. `install.reboot: false` makes `kairos-agent install` end with a "press
   enter" prompt and a getty on tty1, where the installer is drawing. So the
   encrypted install runs `kairos-agent manual-install --use-default-dirs
   /oem/99_hadron-user.yaml`, which reads the same config directories and
   returns. An install without encryption still runs `kairos-agent install`.
6. At every boot, `hadron-unlock` in the initramfs (#60) sees the
   `kcrypt:passphrase` subsystem, stops the splash, asks for the passphrase on
   the console, and opens the container before immucore. kcrypt (#6022) would
   ask the same way if the hook were not there, because `kcrypt.method:
   passphrase` is in `/oem/90_custom.yaml`.

## Interface shared with #60

- **Container.** LUKS2, label `COS_PERSISTENT_LUKS` (`COS_PERSISTENT` before
  kairos-io/kairos#4403), GPT partition label `persistent`, LUKS2 subsystem
  `kcrypt:passphrase`. Opened as `/dev/mapper/<kernel name>`, the name kcrypt
  checks.
- **Marker.** `/etc/hadron-desktop/encryption`, written by an initramfs stage
  of the installer's cloud-config at every boot, no secrets:

  ```
  mode=passphrase
  ```

  The initramfs hook does not read it (the root is not mounted yet); it keys
  on the LUKS2 subsystem. The marker is for the running system.

## Blocked on

kairos-io/kairos#6022 (kcrypt `method: passphrase`). Until a kairos-agent and
an immucore with it are in the image, the check in step 4 fails the install.
This change and #60 must merge together, after #6022.

## Not covered

- The agent appliance. Its installer runs `hadron-install-ui --progress-only`,
  which never runs the wizard, so it never offers encryption.
  `test/agent/install_script_test.sh` checks its cloud-config has none.
- Seeded installs (a cloud-config already in /oem) skip the wizard; they can
  set `encrypted_partitions` and the kcrypt block themselves.
- Changing the passphrase after the install, and a second passphrase.
- `kairos-agent reset` of an encrypted install: kcrypt would need the
  passphrase file again.

## Test

Unit tests: `cd install-ui && go test ./... && go vet ./...`.

Real boot (2026-11-07): the v0.0.7 sway ISO in QEMU with OVMF and no TPM.
Into the live system went the installer from this branch, a kairos-agent
built from kairos-io/kairos#6022 (over `/usr/bin/kairos`), and, for the
installed system, an initramfs rebuilt with #60's module and the #6022
immucore. The installer ran non-interactively with `HADRON_ENCRYPT=1` and
`HADRON_DISK_PASSPHRASE` under `script` on the serial console and exited 0,
so its own check passed. Then:

- `/run/hadron-install` was gone; the live `/oem/99_hadron-user.yaml` had the
  `kcrypt` block and `mode=passphrase`, not the passphrase;
- `blkid /dev/vda5`: `LABEL="COS_PERSISTENT_LUKS"`,
  `SUBSYSTEM="kcrypt:passphrase"`; `luksDump`: one keyslot, argon2id;
- the target COS_OEM (`90_custom.yaml`) did not contain the passphrase
  (`grep -r` found nothing);
- the first boot asked for the passphrase on the console, refused a wrong
  one, and came up to the login screen with the right one;
  `/etc/hadron-desktop/encryption` held `mode=passphrase` and `/home` was on
  `/dev/mapper/vda5`.
