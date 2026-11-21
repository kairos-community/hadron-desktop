// isotope is the command of an Isotope system: updates, system extensions,
// the disk's recovery key, and backups. isotope-update, isotope-ext and
// isotope-recovery are links to it.
//
// For updates it looks up the newest isotope release on GitHub, compares it with
// /etc/isotope/release, and upgrades by handing the matching OCI image to
// `kairos-agent upgrade`. The image is A/B: the running system is untouched
// until the reboot, and the previous one stays selectable in the boot menu.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Exit codes. `check` exits exitUpdate when an update is available, so a script
// can tell "nothing to do" from "failed to look".
const (
	exitOK     = 0
	exitError  = 1
	exitUsage  = 2
	exitUpdate = 100
)

const usage = `Usage: isotope [command] [flags]

Commands:
  status            show the installed and the latest release (default)
  check             like status; exits 100 when an update is available
  upgrade           install the latest release, then offer to reboot; also
                    "isotope update"
      --version vX.Y.Z   install this release instead of the latest
      --image REF        install any OCI image (advanced mode only); with
                         REF "-", ask for the reference
      --reboot           reboot when the upgrade finishes, without asking
      --yes              do not ask for confirmation
  switch FLAVOR     install another desktop (sway, i3, cosmic, niri)
  switch [FLAVOR] --gpu LINE
                    install the hardware (full) or the virtual machine (vm)
                    image instead; later upgrades follow it
      --yes              do not ask for confirmation
      --reboot           reboot when the switch finishes, without asking
  reboot            restart the system into an installed update
  rollback          restart into the previous system, for the next boot only
      --yes              do not ask for confirmation
  ext [command]     system extensions: list, browse, info, install, enable,
                    disable, remove; see "isotope ext help"
  recovery [command]
                    the recovery key of the encrypted disk; see
                    "isotope recovery help"
  backup [command]  back up this machine to a USB disk or a server; see
                    "isotope backup help"
  restore           restore a backup; see "isotope restore help"
  welcome [PAGE]    Isotope Welcome, the first-login setup, again; or one
                    of its pages: apps, language, network, appearance,
                    accessibility, shortcuts
  help              this text

Started by the desktop session and by systemd, not by hand:
  wallpaper-follow  on COSMIC, switch the Isotope wallpaper with light and dark
  tray              the update icon in the panel, with notifications
      --no-icon          the same, without the icon
  notify            the same as tray --no-icon
      --once             check once and exit
  daemon            the root update service on the system bus
  efi-boot-entry    at the end of an install: a UEFI boot entry for the
                     installed disk, first in the boot order

isotope-update, isotope-ext and isotope-recovery are the same command:
"isotope-ext X" is "isotope ext X", "isotope-recovery X" is
"isotope recovery X", and "isotope-update X" is "isotope X".
`

// kairosAgent is the upgrade backend. ISOTOPE_UPDATE_AGENT replaces it, so a
// test can assert what would have been run without touching a disk.
func kairosAgent() string {
	if p := os.Getenv("ISOTOPE_UPDATE_AGENT"); p != "" {
		return p
	}
	return "/usr/bin/kairos-agent"
}

func main() {
	// isotope-ext is a link to this binary.
	if filepath.Base(os.Args[0]) == "isotope-ext" {
		os.Exit(cmdExt(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	// So is isotope-recovery.
	if filepath.Base(os.Args[0]) == "isotope-recovery" {
		os.Exit(cmdRecovery(os.Args[1:], os.Stdout, os.Stderr))
	}
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "status", "check":
		return cmdStatus(cmd, args, stdout, stderr)
	case "switch":
		return cmdSwitch(args, stdin, stdout, stderr)
	case "reboot":
		return cmdReboot(args, stdout, stderr)
	case "rollback":
		return cmdRollback(args, stdin, stdout, stderr)
	case "upgrade", "update":
		return cmdUpgrade(args, stdin, stdout, stderr)
	case "tray":
		return cmdTray(args, stderr)
	case "notify":
		return cmdNotify(args, stderr)
	case "ext":
		return cmdExt(args, stdin, stdout, stderr)
	case "recovery":
		return cmdRecovery(args, stdout, stderr)
	case "backup":
		return cmdBackup(args, stdin, stdout, stderr)
	case "restore":
		return cmdRestore(args, stdin, stdout, stderr)
	case "welcome":
		return cmdWelcome(args, stderr)
	case "wallpaper-follow":
		return cmdWallpaperFollow(stderr)
	case "daemon":
		return cmdDaemon(args, stderr)
	case "efi-boot-entry":
		return cmdEFIBootEntry(args, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(stderr, "isotope: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("isotope "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	return fs
}

func cmdStatus(name string, args []string, stdout, stderr io.Writer) int {
	fs := newFlags(name, stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rel, err := LoadRelease(releaseFile())
	if err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}

	fmt.Fprintf(stdout, "Installed:  Isotope %s (%s, Hadron %s)\n",
		rel.Version, rel.Flavor, orUnknown(rel.HadronVersion))
	if c, err := loadBackupConfig(defaultBackupPaths()); err == nil {
		if w := backupWarning(c); w != "" {
			fmt.Fprintln(stdout, w)
		}
	}
	switch rel.Flavor {
	case flavorAgent:
	case flavorServer:
		if rel.TrustedBoot {
			fmt.Fprintf(stdout, "Image:      server edition (no desktop), %s\n", lineLabels[lineUKI])
		} else {
			fmt.Fprintln(stdout, "Image:      server edition (no desktop)")
		}
	default:
		fmt.Fprintf(stdout, "Image:      %s\n", lineLabels[rel.Line()])
	}

	if rel.Flavor != flavorAgent {
		fmt.Fprintf(stdout, "Pin:        %s\n", orNone(currentPin()))
	}

	latest, err := FetchLatest(context.Background(), rel.GitHubRepo, rel.TrustedBoot)
	if err != nil {
		fmt.Fprintln(stderr, "isotope: could not check for updates:", err)
		return exitError
	}
	if !Newer(latest.Tag, rel.Version) {
		fmt.Fprintf(stdout, "Latest:     %s, you are up to date.\n", latest.Tag)
		return exitOK
	}
	fmt.Fprintf(stdout, "Latest:     %s is available.\n", latest.Tag)
	fmt.Fprintf(stdout, "            What's new: %s\n", latest.URL)
	fmt.Fprintln(stdout, "            Run `isotope upgrade` to install it.")
	if name == "check" {
		return exitUpdate
	}
	return exitOK
}

// pauseOnError keeps a terminal the tray opened from closing over an error:
// with --pause-on-error (set by the tray, not in the usage text), a non-zero
// exit first waits for one line on stdin. EOF ends the wait too.
func pauseOnError(pause bool, code int, in *bufio.Reader, stdout io.Writer) {
	if !pause || code == exitOK {
		return
	}
	fmt.Fprintln(stdout, "Press Enter to close.")
	_, _ = in.ReadString('\n')
}

func cmdUpgrade(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	fs := newFlags("upgrade", stderr)
	version := fs.String("version", "", "release to install")
	imageFlag := fs.String("image", "", "any OCI image to install (advanced mode)")
	reboot := fs.Bool("reboot", false, "reboot when done")
	yes := fs.Bool("yes", false, "do not ask")
	// --as-root marks the sudo child of an interactive upgrade: the parent has
	// already shown the plan, asked, and checked the image, so the child only
	// runs kairos-agent. Not in the usage text.
	asRootChild := fs.Bool("as-root", false, "")
	pause := fs.Bool("pause-on-error", false, "")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	// One reader for every question: a second bufio.Reader over the same
	// stdin would lose whatever the first one had already buffered.
	in := bufio.NewReader(stdin)
	defer func() { pauseOnError(*pause, code, in, stdout) }()

	rel, err := LoadRelease(releaseFile())
	if err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}

	if *version != "" && *imageFlag != "" {
		fmt.Fprintln(stderr, "isotope: --version and --image cannot be combined")
		return exitUsage
	}
	if *version != "" {
		if _, ok := parseSemver(*version); !ok {
			fmt.Fprintf(stderr, "isotope: %q is not a release tag like v1.2.3\n", *version)
			return exitUsage
		}
	}
	if rel.Flavor != flavorAgent {
		return upgradeViaDaemon(rel, *version, *imageFlag, *yes, *reboot, in, stdout, stderr)
	}
	if *imageFlag != "" {
		fmt.Fprintln(stderr, "isotope: --image is not available on the agent flavor")
		return exitUsage
	}

	tag := *version
	if tag == "" {
		latest, err := FetchLatest(context.Background(), rel.GitHubRepo, rel.TrustedBoot)
		if err != nil {
			fmt.Fprintln(stderr, "isotope: could not check for updates:", err)
			return exitError
		}
		if !Newer(latest.Tag, rel.Version) {
			fmt.Fprintf(stdout, "Isotope %s is the latest release, nothing to do.\n", rel.Version)
			return exitOK
		}
		tag = latest.Tag
	} else if _, ok := parseSemver(tag); !ok {
		fmt.Fprintf(stderr, "isotope: %q is not a release tag like v1.2.3\n", tag)
		return exitUsage
	}

	image := rel.ImageFor(tag)
	if !*asRootChild {
		fmt.Fprintf(stdout, "Upgrading Isotope %s -> %s\n  image: %s\n", rel.Version, tag, image)
		fmt.Fprintln(stdout, "The running system is not touched; the new one is used from the next boot.")

		// Ask before escalating, so the question is asked once, as the user.
		if !*yes && !confirm(in, stdout, "Continue?", true) {
			fmt.Fprintln(stdout, "Cancelled.")
			return exitOK
		}

		if err := ImagePullable(context.Background(), image); err != nil {
			fmt.Fprintf(stderr, "isotope: cannot pull the %s image: %v\n", tag, err)
			fmt.Fprintln(stderr, "Releases before upgrade images were published have none; otherwise try again later.")
			return exitError
		}
	}

	if needRoot() {
		// The answer was already given, so the root copy runs with --yes and a
		// pinned --version: it must install exactly what the user agreed to,
		// even if a newer release lands in between. It never reboots; that
		// question is asked below, as the user.
		if err := asRoot("upgrade", "--as-root", "--yes", "--version", tag).Run(); err != nil {
			// The child already said what went wrong.
			return exitError
		}
	} else {
		agent := exec.Command(kairosAgent(), "upgrade", "--source", "oci:"+image)
		agent.Stdout, agent.Stderr = stdout, stderr
		if err := agent.Run(); err != nil {
			fmt.Fprintln(stderr, "isotope: the upgrade failed:", err)
			fmt.Fprintln(stderr, "Nothing changed on the running system; you can retry safely.")
			return exitError
		}
		fmt.Fprintf(stdout, "\nIsotope %s is installed and starts on the next boot.\n", tag)
	}

	if *reboot || (!*yes && confirm(in, stdout, "Reboot now?", false)) {
		if err := rebootCommand().Run(); err != nil {
			fmt.Fprintln(stderr, "isotope: reboot failed:", err)
			return exitError
		}
	}
	return exitOK
}

// needRoot is a variable so tests can run the root path as an ordinary user.
var needRoot = func() bool { return os.Geteuid() != 0 }

// asRoot runs this same binary under sudo, wired to the caller's terminal so
// sudo can ask for the password and kairos-agent's progress stays visible.
func asRoot(args ...string) *exec.Cmd {
	self, err := os.Executable()
	if err != nil {
		self = "/usr/bin/isotope"
	}
	c := exec.Command("sudo", append([]string{self}, args...)...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	return c
}

// rebootCommand is a variable so tests can observe a reboot instead of doing one.
var rebootCommand = func() *exec.Cmd {
	if needRoot() {
		return exec.Command("sudo", "systemctl", "reboot")
	}
	return exec.Command("systemctl", "reboot")
}

func confirm(in *bufio.Reader, stdout io.Writer, question string, def bool) bool {
	hint := "[y/N]"
	if def {
		hint = "[Y/n]"
	}
	fmt.Fprintf(stdout, "%s %s ", question, hint)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(stdout)
		return def
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "":
		return def
	case "y", "yes":
		return true
	default:
		return false
	}
}

// notifyInterval is how often a session has the service check. GitHub allows
// 60 unauthenticated API calls an hour per address; four a day is nothing.
const notifyInterval = 6 * time.Hour

// notifyDelay keeps the first check off the login path: the network may not be
// up yet, and nobody reads a notification in the first seconds of a session.
const notifyDelay = 2 * time.Minute

// cmdNotify is the old name of `tray --no-icon`, kept so existing session
// configs keep working; --once checks a single time.
func cmdNotify(args []string, stderr io.Writer) int {
	fs := newFlags("notify", stderr)
	once := fs.Bool("once", false, "check once and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if !*once {
		return runTray(true, stderr)
	}
	if err := notifyOnce(); err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}
	return exitOK
}

// notifyOnce has the service check, then announces a new release once.
func notifyOnce() error {
	rel, err := LoadRelease(releaseFile())
	if err != nil {
		return err
	}
	if rel.Flavor == flavorAgent {
		return nil
	}
	c, err := dialDaemon()
	if err != nil {
		return fmt.Errorf("the update service is not available: %w", err)
	}
	defer c.Close()
	st, err := c.Status()
	if err != nil {
		return fmt.Errorf("the update service is not available: %w", err)
	}
	if st.State == StateUnmanaged {
		return nil
	}
	if err := retryBusy(c.Check); err != nil {
		return err
	}
	if st, err = c.Status(); err != nil {
		return err
	}
	return announce(c, rel, st, true, Notify)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// lineWarning says what installing an image of line changes, for a system on
// the other line. Later upgrades then follow line: the installed image's own
// release file names it.
func lineWarning(line string) string {
	if line == gpuVM {
		return "This is the virtual machine image: it has no Intel or AMD GPU drivers and no firmware, so on a real machine Wi-Fi, Bluetooth and graphics may stop working. Later upgrades stay on the virtual machine image."
	}
	return "This is the hardware image: it adds Intel and AMD GPU drivers and firmware, about 600 MB more. Later upgrades stay on the hardware image."
}

// flavorAgent is the agent appliance: it has no daemon, so its CLI keeps the
// direct sudo + kairos-agent path.
const flavorAgent = "agent"

// flavorServer is the server edition: no desktop, one image line
// (server-<tag>). It has the daemon, so its CLI works as on a desktop, but it
// installs only server images, and a desktop never installs one.
const flavorServer = "server"

// switchable are the desktops `switch` can install.
var switchable = []string{"sway", "i3", "cosmic", "niri"}

// busyRetries and busyWait bound how long a client waits for the daemon's one
// network slot. Variables so tests do not sleep.
var (
	busyRetries = 5
	busyWait    = time.Second
)

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// currentPin asks the daemon, and reads the pin file when it is not there:
// `status` must keep working with the service stopped or absent.
func currentPin() string {
	if c, err := dialDaemon(); err == nil {
		defer c.Close()
		if st, err := c.Status(); err == nil {
			return st.Pin
		}
	}
	p, _ := LoadPin()
	return p
}

// retryBusy repeats a network call while the daemon says Busy.
func retryBusy(f func() error) error {
	err := f()
	for i := 0; i < busyRetries && daemonErrName(err) == "Busy"; i++ {
		time.Sleep(busyWait)
		err = f()
	}
	return err
}

// daemonFail prints what went wrong with a daemon call and returns the exit code.
func daemonFail(stderr io.Writer, what string, err error) int {
	fmt.Fprintf(stderr, "isotope: %s%s\n", what, describeDaemonError(err))
	return exitError
}

// netFail is daemonFail for the calls retryBusy wraps: a Busy that survived
// the retries means another check or inspection holds the service's network slot.
func netFail(stderr io.Writer, what string, err error) int {
	if daemonErrName(err) == "Busy" {
		fmt.Fprintln(stderr, "isotope: the update service is busy with another check; try again in a moment.")
		return exitError
	}
	return daemonFail(stderr, what, err)
}

func dialOrExplain(stderr io.Writer) (daemonClient, bool) {
	c, err := dialDaemon()
	if err != nil {
		fmt.Fprintln(stderr, "isotope: The update service is not available (isotope-updated).")
		return nil, false
	}
	return c, true
}

func upgradeViaDaemon(rel Release, version, image string, yes, reboot bool, in *bufio.Reader, stdout, stderr io.Writer) int {
	if image != "" && rel.TrustedBoot {
		fmt.Fprintln(stderr, "isotope: this system boots with Trusted Boot; it installs only the signed Trusted Boot images of Isotope releases, so --image is not available.")
		return exitUsage
	}
	c, ok := dialOrExplain(stderr)
	if !ok {
		return exitError
	}
	defer c.Close()

	st, err := c.Status()
	if err != nil {
		return daemonFail(stderr, "", err)
	}
	if image != "" {
		if !st.Advanced {
			fmt.Fprintln(stderr, "isotope: advanced mode is off; installing an arbitrary image is not allowed on this system.")
			return exitUsage
		}
		if image == "-" {
			// The tray's "Install an image…" lands here: ask for the reference.
			fmt.Fprint(stdout, "Image reference: ")
			line, _ := in.ReadString('\n')
			if image = strings.TrimSpace(line); image == "" {
				fmt.Fprintln(stdout, "Cancelled.")
				return exitOK
			}
		}
		return installFlow(c, rel, image, true, yes, reboot, in, stdout, stderr)
	}
	tag := version
	if tag == "" {
		newest, code := newestRelease(c, stderr)
		if code != exitOK {
			return code
		}
		if !Newer(newest, rel.Version) && st.Pin == "" {
			fmt.Fprintf(stdout, "Isotope %s is up to date.\n", rel.Version)
			return exitOK
		}
		tag = newest
	}
	ref, err := ReleaseRef(rel, rel.Flavor, tag)
	if err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}
	return installFlow(c, rel, ref.String(), false, yes, reboot, in, stdout, stderr)
}

// newestRelease is the highest release tag the daemon lists.
func newestRelease(c daemonClient, stderr io.Writer) (string, int) {
	var tags []string
	err := retryBusy(func() (err error) { tags, err = c.Releases(); return })
	if err != nil {
		return "", netFail(stderr, "could not list releases: ", err)
	}
	newest := ""
	for _, t := range tags {
		if _, ok := parseSemver(t); ok && (newest == "" || Newer(t, newest)) {
			newest = t
		}
	}
	if newest == "" {
		fmt.Fprintln(stderr, "isotope: no releases found.")
		return "", exitError
	}
	return newest, exitOK
}

func cmdSwitch(args []string, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	fs := newFlags("switch", stderr)
	yes := fs.Bool("yes", false, "do not ask")
	reboot := fs.Bool("reboot", false, "reboot when done")
	pause := fs.Bool("pause-on-error", false, "")
	gpu := fs.String("gpu", "", "image line: full or vm")
	// The flavor comes first on the command line; flags may follow it.
	var flavor string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		flavor, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if flavor == "" && fs.NArg() == 1 {
		flavor = fs.Arg(0)
	}
	in := bufio.NewReader(stdin)
	defer func() { pauseOnError(*pause, code, in, stdout) }()
	if (flavor == "" && *gpu == "") || fs.NArg() > 1 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	if *gpu != "" && *gpu != gpuFull && *gpu != gpuVM {
		fmt.Fprintf(stderr, "isotope: --gpu is %q; choose %s or %s.\n", *gpu, gpuFull, gpuVM)
		return exitUsage
	}
	rel, err := LoadRelease(releaseFile())
	if err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}
	if flavor == "" {
		flavor = rel.Flavor
	}
	line := *gpu
	if line == "" {
		line = rel.Line()
	}
	if rel.Flavor == flavorServer {
		fmt.Fprintln(stderr, "isotope: the server edition has no desktop to switch; it upgrades only to server images.")
		return exitUsage
	}
	// Trusted Boot has one line, the signed UKI. Another desktop stays on
	// it: line is rel.Line(), so the switch installs <flavor>-uki-<tag>.
	if rel.TrustedBoot && *gpu != "" {
		fmt.Fprintln(stderr, "isotope: this system boots with Trusted Boot; it has one image, the signed UKI, so --gpu cannot change it.")
		return exitUsage
	}
	known := false
	for _, f := range switchable {
		known = known || f == flavor
	}
	if !known {
		fmt.Fprintf(stderr, "isotope: %q is not a desktop flavor; choose one of %s.\n", flavor, strings.Join(switchable, ", "))
		return exitUsage
	}
	if rel.Flavor == flavorAgent {
		fmt.Fprintln(stderr, "isotope: the agent flavor has no desktop to switch.")
		return exitUsage
	}
	if flavor == rel.Flavor && line == rel.Line() {
		if *gpu != "" {
			fmt.Fprintf(stderr, "isotope: this system already runs the %s image.\n", lineLabels[line])
		} else {
			fmt.Fprintf(stderr, "isotope: this system already runs the %s flavor.\n", flavor)
		}
		return exitUsage
	}
	c, ok := dialOrExplain(stderr)
	if !ok {
		return exitError
	}
	defer c.Close()

	newest, rc := newestRelease(c, stderr)
	if rc != exitOK {
		return rc
	}
	ref, err := LineRef(rel, flavor, line, newest)
	if err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}
	if line != rel.Line() {
		fmt.Fprintln(stdout, lineWarning(line))
	}
	if flavor != rel.Flavor {
		fmt.Fprintln(stdout, "Your per-user desktop settings may not carry over, and the login screen may change.")
	}
	// The warning's own question replaces "Continue?", default No.
	if !*yes && !confirm(in, stdout, "Switch anyway?", false) {
		fmt.Fprintln(stdout, "Cancelled.")
		return exitOK
	}
	return installFlow(c, rel, ref.String(), false, true, *reboot, in, stdout, stderr)
}

func cmdReboot(args []string, stdout, stderr io.Writer) int {
	fs := newFlags("reboot", stderr)
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if rel, err := LoadRelease(releaseFile()); err == nil && rel.Flavor == flavorAgent {
		if err := rebootCommand().Run(); err != nil {
			fmt.Fprintln(stderr, "isotope: reboot failed:", err)
			return exitError
		}
		return exitOK
	}
	c, ok := dialOrExplain(stderr)
	if !ok {
		return exitError
	}
	defer c.Close()
	if err := c.Reboot(); err != nil {
		return daemonFail(stderr, "", err)
	}
	fmt.Fprintln(stdout, "Restarting.")
	return exitOK
}

// cmdRollback boots the previous system once, through the update service.
// The agent flavor has no service, so it is refused there with the commands
// to run by hand.
func cmdRollback(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := newFlags("rollback", stderr)
	yes := fs.Bool("yes", false, "do not ask")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "isotope rollback: unexpected argument %q\n", fs.Arg(0))
		return exitUsage
	}
	if rel, err := LoadRelease(releaseFile()); err == nil && rel.Flavor == flavorAgent {
		fmt.Fprintf(stderr, "isotope: rollback is not available on the agent flavor; run `sudo kairos-agent bootentry --select %s` and then `sudo systemctl reboot`.\n", previousEntry)
		return exitUsage
	}
	if !*yes && !confirm(bufio.NewReader(stdin), stdout, "Restart into the previous system? It is used for the next boot only.", false) {
		fmt.Fprintln(stdout, "Cancelled.")
		return exitOK
	}
	c, ok := dialOrExplain(stderr)
	if !ok {
		return exitError
	}
	defer c.Close()
	if err := c.RebootPrevious(); err != nil {
		var pending string
		if daemonErrName(err) == "RebootPending" {
			if st, serr := c.Status(); serr == nil {
				pending = st.Pending
			}
		}
		fmt.Fprintln(stderr, "isotope:", describeRollbackError(err, pending))
		return exitError
	}
	fmt.Fprintln(stdout, "Restarting into the previous system.")
	return exitOK
}

// installFlow is every install: inspect what the reference names, show it,
// ask, apply, follow the job to its end, offer the reboot. The questions go
// through one reader, as in the direct path. advanced says the user asked
// for an arbitrary image (--image): only then may an overridable refusal be
// lifted, and only by an explicit answer.
func installFlow(c daemonClient, rel Release, ref string, advanced, yes, reboot bool, in *bufio.Reader, stdout, stderr io.Writer) int {
	// Always inspect without the override, so the daemon reports the
	// refusal it would make and the user is asked about it.
	var info InspectInfo
	err := retryBusy(func() (err error) { info, err = c.Inspect(ref, false); return })
	if err != nil {
		return netFail(stderr, "cannot inspect the image: ", err)
	}

	repo := info.Ref
	if r, perr := ParseRef(info.Ref); perr == nil {
		repo = r.Repo()
	}
	fmt.Fprintf(stdout, "Image:    %s\nDigest:   %s\nFlavor:   %s\nVersion:  %s\n",
		repo, info.Digest, orUnknown(info.Flavor), orUnknown(info.Version))
	if info.GPU != "" {
		fmt.Fprintf(stdout, "Graphics: %s\n", lineLabels[info.GPU])
	}
	switch {
	case info.Trusted:
		fmt.Fprintln(stdout, "Source:   a published Isotope release (trusted)")
	case info.Isotope:
		fmt.Fprintln(stdout, "Source:   labelled by its publisher, not a published release")
	default:
		fmt.Fprintln(stdout, "Source:   not an Isotope image")
	}

	override := false
	if info.Refusal != "" {
		if !info.Overridable || !advanced {
			fmt.Fprintln(stderr, "isotope:", info.Refusal)
			return exitError
		}
		fmt.Fprintln(stdout, info.Refusal)
		question := "This is not an Isotope image. Install it anyway?"
		if info.Isotope {
			question = "This is not a published Isotope release. Install it anyway?"
		}
		// --yes never answers this one: it is the question that stops a
		// mistyped image from replacing the system.
		if !confirm(in, stdout, question, false) {
			fmt.Fprintln(stdout, "Cancelled.")
			return exitOK
		}
		override = true
	}
	// Another line is allowed, like another flavor; `switch --gpu` has
	// already warned (and passes yes), anything else is warned here.
	if info.GPU != "" && info.GPU != rel.Line() && !yes {
		fmt.Fprintln(stdout, lineWarning(info.GPU))
	}
	fmt.Fprintln(stdout, "The running system is not touched; the new one is used from the next boot.")
	if !yes && !confirm(in, stdout, "Continue?", true) {
		fmt.Fprintln(stdout, "Cancelled.")
		return exitOK
	}

	events, stop, err := c.Watch()
	if err != nil {
		fmt.Fprintln(stderr, "isotope:", err)
		return exitError
	}
	defer stop()
	if err := c.Apply(ref, info.Digest, override); err != nil {
		return daemonFail(stderr, "", err)
	}
	// A Check that failed between Watch and Apply leaves StateError behind;
	// it is not this install. Only a state this job passes through, seen
	// after Apply, makes error and reboot-pending count.
	started := false
	for ev := range events {
		if ev.Line != "" {
			fmt.Fprintln(stdout, ev.Line)
		}
		if ev.Status == nil {
			continue
		}
		switch ev.Status.State {
		case StateInspecting, StateApplying:
			started = true
		}
		if !started {
			continue
		}
		switch ev.Status.State {
		case StateIdle, StateUpToDate, StateUpdateAvailable:
			// No job passes through these: it was cancelled elsewhere (the
			// tray) or the daemon restarted and forgot it.
			fmt.Fprintln(stderr, "isotope: lost the update job: it was cancelled or the update service restarted.")
			return exitError
		case StateRebootPending:
			if p := ev.Status.Pending; p == pendingFailed || p == pendingStarted {
				// kairos-agent may have swapped the slots before it failed;
				// the service refuses another install until a restart.
				fmt.Fprintln(stderr, "isotope: the update failed after it started writing.")
				fmt.Fprintln(stderr, pendingDetail(p))
				return exitError
			}
			fmt.Fprintln(stdout, "\nThe update is installed and starts on the next boot.")
			return offerReboot(c, reboot, in, stdout, stderr)
		case StateError:
			// Only a failure before kairos-agent started ends here.
			fmt.Fprintln(stderr, "isotope: the update failed:", ev.Status.Detail)
			fmt.Fprintln(stderr, "Nothing changed on the running system; you can retry safely.")
			return exitError
		}
	}
	fmt.Fprintln(stderr, "isotope: lost the connection to the update service before the update finished.")
	return exitError
}

func offerReboot(c daemonClient, reboot bool, in *bufio.Reader, stdout, stderr io.Writer) int {
	if !reboot && !confirm(in, stdout, "Reboot now?", false) {
		return exitOK
	}
	if err := c.Reboot(); err != nil {
		return daemonFail(stderr, "reboot failed: ", err)
	}
	return exitOK
}
