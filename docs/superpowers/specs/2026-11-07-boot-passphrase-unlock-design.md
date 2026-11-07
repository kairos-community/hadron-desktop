# Boot passphrase unlock

## Intent

Hadron Desktop supports two disk encryption modes:

1. Normal edition: COS_PERSISTENT (which holds `/home` and `/var`) is a LUKS2
   container opened by a passphrase the user types at install and at every
   boot. The installer side is #63; kcrypt's `method: passphrase`
   (kairos-io/kairos#6022) does the encryption.
2. Trusted Boot edition (#61): a TPM key sealed to PCRs, Kairos-native, with
   kcrypt's `passphrase_fallback` (also #6022) for when the TPM cannot unseal.

kcrypt's non-UKI default, a random key in TPM NV memory, is not used. It has
no PCR policy and an empty key password, so any system booted on the machine
can read the key.

This note covers the boot side of mode 1: the prompt, how it gets past the
splash, and the Kairos changes it needs.

## What happens today

All references are to `kairos-io/kairos` at `up/master` 293018ae8.

- immucore unlocks in one DAG step, `RunKcrypt`
  (`immucore/pkg/state/steps.go:209`), which calls
  `kcrypt.UnlockAllEncryptedPartitions` (`steps_shared.go:887`,
  `sdk/kcrypt/unlock.go:196`). On a non-UKI boot it runs after the OEM mount
  (`immucore/pkg/dag/dag_normal_boot.go:80`).
- `GetEncryptorFromConfig` (`sdk/kcrypt/encryptor.go:578`) picks a key
  server, a PCR-sealed TPM key (UKI) or the TPM NV key. None asks a person,
  and every one needs a TPM or a key server: without one,
  `validateTPMDevice` (`encryptor.go:688`) fails. So an install with
  `install.encrypted_partitions` on a machine with no TPM fails, and a disk
  whose key cannot be found stops the boot at the custom mounts
  (`steps_shared.go:502`); with `rd.emergency=reboot` the machine reboots in
  a loop behind the splash.
- `luksUnlock` (`unlock.go:96`) only tries keyslot 0.
- `luksFormatArgs` (`sdk/kcrypt/lock.go:216`) uses `--iter-time 5`: right for
  a random 32 character key, far too weak for a passphrase a person chose.
- What helps: `findEncryptedPartitions` (`unlock.go:152`) skips a partition
  whose `/dev/mapper/<kernel name>` already exists (`unlock.go:169`). A
  container opened under that name before immucore is left alone and mounted.
- The initramfs (dracut, systemd 262) already has `systemd-ask-password`,
  `systemd-tty-ask-password-agent`, `cryptsetup` and
  `systemd-ask-password-console.path`, enabled in `sysinit.target`. kairos-init
  omits plymouth on purpose (`kairos-init/pkg/bundled/bundled.go:351`).
- `kairos-splash` runs on tty1 in graphics mode from udev trigger until
  switch-root (`bundled.go:400`). Text written to the console while it runs is
  not visible.

## Design

### Kairos: kcrypt passphrase method and fallback (kairos-io/kairos#6022)

In `sdk/kcrypt`:

- `kcrypt.method: passphrase` selects `PassphraseEncryptor`, checked first in
  `GetEncryptorFromConfig`, before any TPM validation.
  - `Encrypt` reads the passphrase from `kcrypt.passphrase_file`, never from
    the configuration, which is copied to the plain OEM partition. It calls
    `luksifyWithPassphrase` (`lock.go:48`) with `--pbkdf argon2id
    --iter-time 2000 --subsystem kcrypt:passphrase`; cryptsetup keeps the last
    `--iter-time`, so this replaces the 5 ms. The container keeps the label
    kcrypt always gives it, `COS_PERSISTENT_LUKS`.
  - `Unlock` uses the file while it still exists (right after `Encrypt`, in
    the install hook, `agent/internal/agent/hooks/encrypt.go:61`), and
    otherwise asks until the passphrase is right.
- `kcrypt.passphrase_fallback: true` (and `kcrypt.passphrase_timeout`,
  default 300 s), for Trusted Boot: when the configured encryptor fails, ask
  for a passphrase for each partition that is still locked, then return the
  original error if nobody answers.
- The prompt is `systemd-ask-password --no-tty --id=kcrypt:<name>` (the
  package variable `AskPasswordCommand`, so an image can replace it). It opens
  with `cryptsetup open`, so every keyslot is tried and the kernel does the
  argon2 work.
- The keys are in `sdk/schema/kcrypt_schema.go`.

With #6022 alone, a passphrase disk boots: immucore asks. The Hadron side
makes the prompt visible and asks before immucore.

### Hadron: the `hadron-unlock` dracut module

`rootfs/usr/lib/dracut/modules.d/45hadron-unlock/`, pulled in by
`rootfs/etc/dracut.conf.d/45-hadron-unlock.conf`. kairos-init builds the
initramfs after `COPY rootfs/ /`, so it is in every variant (sway, i3,
cosmic, niri) and in the agent image, which inherits i3's initramfs (the agent
installer never encrypts, so there it finds nothing to do).

- A drop-in for `systemd-ask-password-console.service`:
  `Conflicts=kairos-splash.service` and `After=kairos-splash.service`. The
  console agent starts only when something asks for a password, so a boot
  without a question keeps its splash. When a question is asked, systemd stops
  the splash first (it puts the console back in text mode on exit), then the
  agent asks on tty1 and on ttyS0. This also makes kcrypt's own prompt
  visible, the Trusted Boot fallback included.
- `hadron-unlock.service`: a oneshot in `initrd.target.wants`, ordered
  `After=systemd-udev-settle.service` and `Before=immucore.service`. It skips
  recovery, the state reset and live media, with the same command line tests
  as the splash.
- `hadron-unlock` (the script) finds each LUKS container labelled
  `COS_PERSISTENT_LUKS` (or `COS_PERSISTENT`, or GPT label `persistent`) that
  is not open yet. If its LUKS2 subsystem is `kcrypt:passphrase`, it asks with
  `systemd-ask-password --no-tty --timeout=0`, opens it with `cryptsetup open`
  as `/dev/mapper/<kernel name>`, and says "Wrong passphrase. Try again" on a
  wrong one. It waits forever: nothing else can open the disk, so a timeout
  could only end in a failed boot. Any other container is left to immucore.
  `hadron.unlock=0` turns it off (kcrypt then asks).

The hook keys on the LUKS2 header, not on the marker
`/etc/hadron-desktop/encryption` (`mode=passphrase`, written by #63), because
the root filesystem is not mounted yet when it runs.

### Interface shared with #63

- Container: LUKS2, label `COS_PERSISTENT_LUKS` (`COS_PERSISTENT` before
  kairos-io/kairos#4403), GPT label `persistent`, subsystem
  `kcrypt:passphrase`, opened as `/dev/mapper/<kernel name>`.
- Marker: `/etc/hadron-desktop/encryption` holds `mode=passphrase`.

## Blocked on

kairos-io/kairos#6022. Without it no install can make a passphrase disk, so
this module has nothing to open. #63 and this change must merge together,
after #6022 is in the kairos-agent and immucore the image ships.

## Tests

QEMU with OVMF, headless, the released `hadron-desktop-sway-v0.0.7-amd64.iso`.
The module was put into the installed system by rebuilding its initramfs with
its own dracut (`dracutbasedir=<copy with the module>`) and copying it into
`active.img`; the #6022 build of the multi-call `kairos` binary (immucore,
kairos-agent) was appended to that initramfs as an extra cpio and run from the
live ISO for the install.

1. Baseline, before this change: an encrypted install whose key could not be
   found reboots in a loop behind the splash, with no message.
2. Passphrase-only install with no TPM (kairos-agent from #6022): the header
   has subsystem `kcrypt:passphrase` and an argon2id keyslot.
3. Boot with no TPM: the splash stops, the prompt is on tty1 (and ttyS0). It
   was still waiting after 6.5 minutes with nobody typing. A wrong passphrase
   is refused, the right one opens `/dev/mapper/vda5`, immucore logs "Device
   already unlocked, skipping", and the login screen comes up with `/home` on
   the mapper.
4. `hadron.unlock=0`: kcrypt's own prompt asks, refuses a wrong passphrase,
   and boots with the right one.
5. Trusted Boot's fallback path was tested on a non-UKI disk with a fresh
   swtpm and `passphrase_fallback`: kcrypt asks after the TPM unlock fails.
6. End to end with the installer from #63, no TPM: the installer encrypted
   the disk and exited 0, the target `/oem` held no passphrase, and the first
   boot showed this module's prompt, refused a wrong passphrase and reached
   the login screen with the right one.

## Risks

- argon2id memory: cryptsetup lowers the memory cost when the machine has
  little free memory. The live ISO, which keeps its root in RAM, got 83 MiB
  with a higher time cost instead of 1 GiB.
- The console agent shows the prompt on the consoles in `console=` only.
- Recovery boots do not run the hook; recovery does not mount COS_PERSISTENT.
- A reset (`kairos-agent reset`) of a passphrase install would need the
  passphrase file again. Not handled.
