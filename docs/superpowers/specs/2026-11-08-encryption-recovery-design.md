# Encryption recovery: a forgotten passphrase, and the OEM partition

## Intent

The normal edition encrypts COS_PERSISTENT (`/home`, `/var`) with a
passphrase typed at install (#63) and at every boot (#60, kcrypt
`method: passphrase` from kairos-io/kairos#6022). The passphrase is the only
key. This note covers what happens when the person forgets it, and whether
COS_OEM should be encrypted too.

All Kairos references are to the branch `feat/kcrypt-recovery-wipe`
(kairos-io/kairos#6022 plus this change) unless they say otherwise.

## A forgotten passphrase, before this change

1. The recovery entry boots `recovery.img` with `root=LABEL=COS_SYSTEM`.
   That is not live media (`immucore/internal/utils/common.go`,
   `DisableImmucore`), so immucore runs its normal graph
   (`immucore/pkg/cmd/root.go`, the `default:` case that calls
   `RegisterNormalBoot`), with the kcrypt unlock step
   (`immucore/pkg/dag/dag_normal_boot.go:80`). The configuration in the plain
   OEM says `kcrypt.method: passphrase`, and the header says
   `kcrypt:passphrase`, so kcrypt asks for the passphrase and waits for ever.
   Recovery never reaches a login prompt.
2. Hadron's own hook does not run in recovery. `hadron-unlock.service` has
   `ConditionKernelCommandLine=!root=LABEL=COS_SYSTEM`,
   `!root=live:LABEL=COS_RECOVERY` and `!kairos.reset`, the same words that
   keep `kairos-splash` off recovery (`kairos-init/pkg/bundled/bundled.go`,
   `SplashServiceDracut`). The QEMU test below confirms it: the hook's unit is
   skipped and the prompt in recovery is kcrypt's. So the recovery prompt is
   kcrypt's, on the console password agent, with no splash.
3. Past the prompt, `kairos-agent reset` refused the locked container twice.
   `NewResetSpec` (`agent/pkg/config/spec.go`, `lockedContainerError`) found
   no COS_PERSISTENT, because the plaintext label is inside the container, and
   said "encrypted container with no open mapper". Behind it,
   `PreflightResetFormat` (`agent/pkg/action/reset_encrypt.go`) refused a raw
   LUKS container. A forgotten passphrase meant a reinstall from USB.

## Design

### Recovery may go on without the passphrase (Kairos)

`unlockByPrompt` (`sdk/kcrypt/passphrase.go`) checks the boot state with
`state.DetectBoot`, which reads the same command line words immucore reads.
In `Recovery` and `AutoReset` (the "state reset (auto)" entry):

- the prompt adds "Press Enter with nothing typed to leave the encrypted
  files locked — a reset will erase them";
- an empty answer, or a cancelled question, returns `ErrLeftLocked`. The
  shared prompt loop then asks nothing more, and the unlock step returns nil,
  so recovery boots to its login prompt with the partition locked;
- a right passphrase still opens the partition, and a wrong one is asked
  again, with the same hint.

A normal boot does not change: an empty answer is a wrong passphrase and the
prompt keeps asking.

The way out is in the password agent protocol, not in the drawing: the reply
is empty (`+` with nothing after it) or a cancel (`-`). Every agent can offer
it:

- The console agent (`systemd-tty-ask-password-agent`) sends an empty answer
  on Enter, and a cancel on Ctrl-D. It shows the whole message, hint
  included. This is the agent that asks in recovery today.
- kairos-splash as a password agent (#64): Enter with an empty field already
  sends an empty answer, so the skip works. Its panel title is the generic
  "Enter the disk passphrase" for a long message, so the hint is not drawn,
  and its Esc clears the field (Ctrl-C cancels). The splash does not run in
  recovery today (same command line conditions as above), so nothing is
  missing yet. If it ever does, it needs one small change in `splash/main.c`:
  when `Message=` contains "a reset will erase them", draw "Enter with
  nothing typed: leave the files locked" on the note line instead of
  "Enter to unlock · Esc to clear", and make Esc on an empty field send the
  cancel (`prompt_submit(1)`).

### A reset recreates a passphrase container left locked (Kairos)

When the reset must format a locked container, `NewResetSpec` takes the raw
container into the spec instead of refusing, under the same rule as below
(`lockedPassphraseContainerFn`). `PreflightResetFormat` still refuses a raw
LUKS container, except when:

- the node is not UKI, and
- the LUKS2 header says `kcrypt:passphrase`, or has no subsystem and the
  configuration says `kcrypt.method: passphrase`.

Then the preflight (`agent/pkg/action/reset_wipe.go`):

1. logs that the reset erases the container and everything in it, and that
   the old passphrase will not open the new one;
2. gets the new passphrase: from `kcrypt.passphrase_file` when it is
   readable (an unattended reset), or by asking twice with
   `systemd-ask-password` (at least 8 characters, five tries, five minutes
   for each question). On a terminal it asks there; in a service it asks
   through the password agents. A typed passphrase goes to a 0600 file in
   `/run/kairos-reset/`, which the reset deletes when it ends;
3. points the reset at the plaintext label (`COS_PERSISTENT`), an ext4
   filesystem, `install.encrypted_partitions`, and the passphrase file.

Nothing is written before every partition passed its preflight. Then the
reset runs `wipefs --all` on the raw container (the ext4 format alone leaves
the second LUKS2 header copy, which blkid still finds), formats, and
`EncryptFormattedPartition` creates the new container through
`PassphraseEncryptor`: argon2id, two seconds, subsystem `kcrypt:passphrase`,
label `COS_PERSISTENT_LUKS`. A container with a TPM, PCR or key server key
is still refused: its key may come back, and erasing it would destroy data
that can still be read.

How a person uses it: GRUB, "Hadron Desktop recovery", press Enter at the
passphrase prompt, log in, `sudo kairos-agent reset --unattended` (or the
"state reset (auto)" entry, which runs the same reset on tty1 with no login),
type the new passphrase twice. `--unattended` only skips the 60 second
"press a key to cancel" wait; the new passphrase is still asked.

## The OEM partition

### What reads or writes COS_OEM before Linux unlocks anything

GRUB reads every partition it can with `search --file`. It cannot open an
argon2id LUKS2 container, so on an encrypted OEM each of these finds nothing
there.

| Who | What | Where | Partition |
|---|---|---|---|
| GRUB, every boot | `load_env` of `/grub_oem_env` (`default_menu_entry`) | `kairos-init/pkg/bundled/bundled.go:644,655` | COS_STATE: `SetDefaultGrubEntry` writes it to the state mount point (`agent/pkg/elemental/elemental.go:936`, called from `install.go:213`, `reset.go:243`, `upgrade_finalize.go:237`), despite its name |
| GRUB, every boot | `load_env` of `/grubenv` (`saved_entry`, `next_entry`, install `grub_options`) | `bundled.go:640,659` | COS_OEM, or COS_STATE when `install.encrypted_partitions` lists COS_OEM (see the next rows) |
| GRUB, a boot after `next_entry` was set | `save_env next_entry` (clears it) | `bundled.go:663-667` | the partition where `/grubenv` was found |
| GRUB, boot assessment | `load_env`/`save_env boot_assessment_tentative` on `/boot_assessment` | `kairos-init/pkg/bundled/cloudconfigs/08_grub.yaml:83-101` | COS_STATE |
| Linux, `boot.before` | clears the boot assessment sentinels | `08_grub.yaml:28-35`, after-upgrade at `:128-129` | COS_STATE |
| `kairos-agent bootentry --select` (the tray's "boot previous") | `next_entry` | `agent/pkg/action/bootentries.go:112-129` | COS_STATE when the config lists COS_OEM as encrypted, else `/oem/grubenv` |
| install hook `grub_options` | the options | `agent/internal/agent/hooks/gruboptions.go:75-112,357-363` | same rule |
| `kairos-agent grubedit` | any variable | `agent/pkg/cmd/root.go:1167`, default `--grub-file /oem/grubenv` | COS_OEM, always |

So the boot assessment never touches OEM, and next-boot selection already
moves to COS_STATE when OEM is encrypted. The Kairos docs sentence
(`docs/advanced/partition_encryption.md`, "only partly supported ... boot
assessment and next-boot selection write to OEM") is out of date for these.
What is still wrong on an encrypted OEM:

- `kairos-agent grubedit` writes to `/oem/grubenv`, which GRUB cannot read.
- `bootentries.go` and `gruboptions.go` decide from `install.encrypted_partitions`,
  not from the disk. A node whose OEM is encrypted any other way writes where
  GRUB does not look.

None of this is in Hadron. Hadron's `10_hadron_grub_env.yaml` already skips a
LUKS OEM.

### What really breaks with an encrypted OEM

The blockers are after GRUB, in recovery and the reset:

- OEM holds the users. In recovery with OEM left locked there is no `hadron`
  user, so nobody can log in, and `kairos-agent reset` refuses without an
  admin user in the configuration (`agent/pkg/config/config.go:106`,
  `CheckConfigForUsers`). A forgotten passphrase is back to a USB reinstall,
  which is what this change removes.
- A reset without `--reset-oem` leaves a locked OEM with the old passphrase,
  and the next boot asks for it. With `--reset-oem` the users are gone.
- `hadron-unlock` only opens the persistent container. kcrypt now finds OEM by
  its header and asks once for both (kairos-io/kairos#6022, "find passphrase
  containers by their LUKS2 header"), so this part would work.

### Decision: keep OEM plain, and take the password hash out of it

Do not encrypt OEM on the normal edition. Moving the GRUB writes is not
needed (they are already on COS_STATE), and encrypting OEM breaks recovery.

What OEM holds that matters, from `install-ui/config.go`: `hostname`, the
keyboard and timezone, the encryption marker, the kcrypt block (no secret),
and the user with `passwd`, a SHA-512 crypt(3) hash from
`openssl passwd -6` (5000 rounds). That hash is the problem: anyone with the
disk can test login passwords against it far faster than against the argon2id
keyslot, and people reuse a password as the disk passphrase.

Proposal, for a follow-up change to the installer:

1. With encryption on, `RenderCloudConfig` writes the user to
   `/oem/90_custom.yaml` without `passwd` (name, groups, keys only), so the
   user still exists in recovery and the reset's admin check passes.
2. The installer writes the hash to a second file on COS_PERSISTENT,
   `/usr/local/cloud-config/90_hadron_password.yaml`, after the install (in an
   `after-install` stage, which has the unlocked mapper). Kairos reads
   `/usr/local/cloud-config/` at boot (`immucore/internal/constants/constants.go:19`),
   after the unlock, and yip sets the password.
3. Recovery has no password for the user: the console login is closed, and
   the way back from a forgotten passphrase is the "state reset (auto)" entry,
   which needs no login and asks for the new passphrase on tty1.

This is not small (installer, a new file on the encrypted partition, the
recovery login, and a boot test on every variant), so it is not implemented
here.

## Tests

Go unit tests, run with `go test` on `sdk/kcrypt`, `agent/pkg/action`,
`agent/pkg/uki` and `immucore/pkg/state`:

- recovery: an empty answer and a cancel leave the partition locked; the
  right passphrase still opens it; the hint is in the prompt; a normal boot
  keeps asking with the old prompt; the Trusted Boot fallback offers the same
  skip; `AskNewPassphrase` retries a short or mismatched passphrase and gives
  up after five tries;
- reset: a `kcrypt:passphrase` container is prepared for a new container, with
  a new passphrase asked once even for OEM and persistent; the passphrase file
  is used without asking; a header without a subsystem passes only with
  `method: passphrase`; another subsystem, a UKI node, an unreadable header or
  no answer are refused with nothing formatted; `WipeLockedContainer` runs
  `wipefs --all` only on a raw LUKS container.

QEMU with OVMF, no TPM, the released `hadron-desktop-sway-v0.0.7-amd64.iso`
(v0.0.8 and v0.0.9 have no ISO assets). The `kairos` multi-call binary built
from this branch ran the install from the live ISO (as #63 does, a 0600
passphrase file in `/run`), and was put into `active.img` and `recovery.img`
with #60's module as an extra initramfs cpio.

1. The install made an argon2id LUKS2 container with the subsystem
   `kcrypt:passphrase`; `/oem` held no passphrase. The normal boot showed
   `hadron-unlock`'s prompt, the passphrase opened `/dev/vda5`, and a file was
   written to `/home/hadron`.
2. Recovery: the journal says "Ask for the Hadron disk passphrase skipped,
   unmet condition check ConditionKernelCommandLine=!root=LABEL=COS_SYSTEM".
   kcrypt's prompt showed the hint. Enter with nothing typed: "Left locked on
   request", and recovery reached its login with `vda5` locked.
3. `kairos-agent reset --unattended` in recovery: the warning that the reset
   erases the container, the new passphrase asked twice (a mismatched
   confirmation was asked again), `wipefs`, a new container with the same label
   and subsystem and an argon2id keyslot. The old passphrase was refused by
   `cryptsetup`, the new one accepted, `/run/kairos-reset/` was empty.
4. The next normal boot refused the old passphrase ("Wrong passphrase. Try
   again"), opened with the new one, and `/home/hadron` no longer had the file.

The first reset attempt failed at `NewResetSpec` ("no open mapper"), which is
how the second refusal above was found.

## Risks

- kcrypt's minimum for a new passphrase is 8 characters; Hadron's installer
  asks for 12. A reset can set a shorter one.
- The skip relies on the command line words of the recovery entries. A custom
  GRUB entry that boots `recovery.img` without them would ask for ever, as
  before.
- Recovery boots with the partition locked; anything that expects
  `/usr/local` there (none in Hadron) does not get it.
- A reset with no one at the console and no `kcrypt.passphrase_file` stops
  after five minutes per question, with nothing formatted.
