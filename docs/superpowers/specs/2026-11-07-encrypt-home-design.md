# Encrypt your files (TPM key and recovery passphrase)

## Intent

The installer can encrypt the partition that holds the user's files. The key
is kept in the TPM, so the machine boots without a prompt, and the user sets a
recovery passphrase that opens the disk when the TPM no longer can.

## What the user sees

The step "Encrypt your files?" comes after the target disk, only when the
machine has a TPM 2.0 (`/dev/tpmrm0`, and `/sys/class/tpm/*/tpm_version_major`
is 2 when sysfs has it). "no" is selected by default. On "yes" the user types
a recovery passphrase (12 characters or more, with a strength hint) and then
types it again. The review screen shows `encrypt on (TPM key and recovery
passphrase)`.

The step says:

- what is encrypted: COS_PERSISTENT, which holds /home and /var;
- what it protects: the files on a disk taken out of the computer;
- what it does not protect: someone with the whole computer. The key sits in
  TPM NV memory with no PCR policy, and kairos-agent reads it back with
  `kairos-agent kcrypt readnv` (`agent/pkg/action/kcrypt.go`). Nothing ties
  the key to the system that boots, so another system started on the same
  computer can likely read it too. This is an inference from the code, not a
  tested attack, so the text says "likely";
- that the recovery passphrase is the only way back in when the TPM is
  cleared or reset, or the mainboard is replaced.

Non-interactive installs use `HADRON_ENCRYPT=1` and
`HADRON_RECOVERY_PASSPHRASE`. The install fails when there is no TPM 2.0.

## How it works

1. The rendered cloud-config gets

   ```yaml
   install:
     reboot: false
     encrypted_partitions:
       - COS_PERSISTENT
   ```

   and an initramfs stage that writes the marker (below). The recovery
   passphrase is not in it.
2. kairos-agent encrypts COS_PERSISTENT. On a GRUB install kcrypt picks the
   local TPM NV method: a random passphrase in TPM NV index 0x1500000, keyslot
   0, created with `--iter-time 5`. immucore unlocks with it at every boot.
3. kairos-agent locks the container again at the end of its encryption hook
   (`hooks/finish.go`), before the after-install stage. A yip stage could add
   a key, but non-strict hooks drop their errors (`action.Hook`), so a failure
   would be silent. The installer adds the key itself, after kairos-agent
   exits:
   - `lsblk` finds the crypto_LUKS partition labelled `COS_PERSISTENT_LUKS`
     (or `COS_PERSISTENT`, before kairos-io/kairos#4403) on the target disk;
   - `kairos-agent kcrypt readnv` gives the TPM passphrase (the last line:
     kairos-agent prints its start-up log lines to stdout first);
   - `cryptsetup luksAddKey --pbkdf argon2id --force-password --key-file -
     --new-keyfile /dev/fd/3` adds the recovery passphrase. Both passphrases
     go through pipes, never through a file or an argument. No `--iter-time`:
     cryptsetup benchmarks argon2id for its default 2 s (on a 4 GiB QEMU
     guest: time cost 8, 1 GiB memory);
   - `cryptsetup token import` adds the `hadron-recovery` token;
   - `cryptsetup open --test-passphrase --key-slot N` proves the passphrase
     opens the new keyslot.
   Any failure stops on the "install halted" screen with the error in the log.
   On success the installer reboots.
4. `install.reboot: false` makes `kairos-agent install` end with a "press
   enter" prompt and a getty on tty1, where the installer is drawing. So the
   encrypted install runs `kairos-agent manual-install --use-default-dirs
   /oem/99_hadron-user.yaml`, which reads the same config directories and
   returns. An install without encryption still runs `kairos-agent install`.

No Kairos change is needed.

## Interface for the boot-time passphrase prompt

- **Keyslot.** The recovery passphrase is a normal LUKS2 passphrase keyslot
  (argon2id) on the COS_PERSISTENT LUKS2 container, the partition with LUKS
  label `COS_PERSISTENT_LUKS` (`COS_PERSISTENT` on installs older than
  kairos-io/kairos#4403). The passphrase bytes are exactly what the user
  typed, with no trailing newline, so `cryptsetup open` reading a typed line
  from stdin matches.
- **Token.** A LUKS2 token of type `hadron-recovery` names the keyslot:
  `{"type":"hadron-recovery","keyslots":["1"]}`. Find it with
  `cryptsetup luksDump --dump-json-metadata <dev>` (the token whose `type` is
  `hadron-recovery`; its `keyslots[0]` is the slot) and pass
  `--key-slot <slot>` to `cryptsetup open`. It holds no key material. The slot
  is 1 on a fresh install, but read the token, do not assume 1.
- **Marker.** `/etc/hadron-desktop/encryption`, written by an initramfs stage
  of the installer's cloud-config at every boot, no secrets:

  ```
  mode=tpm
  recovery=1
  ```

  The cloud-config lives on COS_OEM, which is not encrypted, so the same
  values can be read from `/oem` before COS_PERSISTENT is unlocked.

## Not covered

- The agent appliance. Its installer runs `hadron-install-ui --progress-only`,
  which never runs the wizard, so it never offers encryption.
  `test/agent/install_script_test.sh` checks its cloud-config has none.
- Seeded installs (a cloud-config already in /oem) skip the wizard; they can
  set `encrypted_partitions` themselves, without a recovery keyslot.
- UKI installs. kcrypt binds the key to PCRs there and removes the passphrase
  keyslot, so `readnv` has nothing to read. Hadron Desktop ships GRUB images.
- A key bound to the boot chain (PCR policy) on GRUB installs, which would
  close the "whole laptop" gap. That needs a kcrypt change.
- Changing the recovery passphrase after install.

## Test

Unit tests: `cd install-ui && go test ./... && go vet ./...`.

Real boot (2026-11-07): the v0.0.7 i3 ISO in QEMU with OVMF and swtpm, the
installer from this branch copied into the live system and run with
`HADRON_ENCRYPT=1`. The install finished, the installed system booted to the
login screen with `/dev/mapper/vda5` on /home, `luksDump` showed keyslot 1
argon2id (time 8, 1048576 KiB) and token 0 `hadron-recovery` → keyslot 1,
`cryptsetup open --test-passphrase --key-slot 1` accepted the recovery
passphrase and refused a wrong one, and `/etc/hadron-desktop/encryption` held
`mode=tpm` and `recovery=1`. `/oem/90_custom.yaml` did not contain the
passphrase.
