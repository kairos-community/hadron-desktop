package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/godbus/dbus/v5"
)

const extUsage = `Usage: hadron-ext [command] [NAME] [flags]
       hadron-update ext [command] [NAME] [flags]

System extensions add software to /usr that must run on the host: a VPN
daemon, firmware tools, a kernel module. They run as root.

Commands:
  list               the installed extensions and their state (default)
  browse [WORD]      what the catalogs offer; ✓ marks the installed ones
  info NAME          the description, the versions and the files it adds
  install NAME[@VERSION]
                     install from the catalogs, check it, and enable it
  install URL|PATH   install an image from elsewhere (advanced mode only);
                     with "-", ask for it
      --force            enable it even when it replaces files of the system
                         with other content; this can break the desktop
      --yes              do not ask for confirmation
  enable NAME        turn an installed extension on, now and at every boot
      --force            as for install
  disable NAME       turn it off, now and at every boot
  remove NAME        turn it off and delete it
      --yes              do not ask for confirmation

The catalogs are listed in /etc/hadron-desktop/extensions.conf, one
catalog=URL per line. Only members of the admin group can change extensions.
`

// extClient is the CLI's view of the extension half of the service. The real
// one is the D-Bus client; tests inject a fake through dialExt.
type extClient interface {
	Status() (Status, error)
	Watch() (events <-chan Event, stop func(), err error)
	Extensions() ([]InstalledExt, error)
	Catalog() ([]CatalogEntry, error)
	ExtensionFiles(name string) ([]string, error)
	InstallExtension(ref, version string, force bool) error
	SetExtensionEnabled(name string, on, force bool) error
	RemoveExtension(name string) error
	Close()
}

var dialExt = func() (extClient, error) { return dial() }

func (c *client) Extensions() ([]InstalledExt, error) {
	var ms []map[string]dbus.Variant
	if err := c.call("Extensions", []any{&ms}); err != nil {
		return nil, err
	}
	out := make([]InstalledExt, 0, len(ms))
	for _, m := range ms {
		out = append(out, InstalledExt{Name: str(m, "name"), File: str(m, "file"),
			Enabled: boolean(m, "enabled"), Merged: boolean(m, "merged"), Source: str(m, "source")})
	}
	return out, nil
}

func (c *client) Catalog() ([]CatalogEntry, error) {
	var ms []map[string]dbus.Variant
	if err := c.call("Catalog", []any{&ms}); err != nil {
		return nil, err
	}
	out := make([]CatalogEntry, 0, len(ms))
	for _, m := range ms {
		v, _ := m["versions"].Value().([]string)
		out = append(out, CatalogEntry{Name: str(m, "name"), Title: str(m, "title"),
			Description: str(m, "description"), Latest: str(m, "latest"), Catalog: str(m, "catalog"),
			Versions: v, Installed: boolean(m, "installed"), Enabled: boolean(m, "enabled")})
	}
	return out, nil
}

func (c *client) ExtensionFiles(name string) ([]string, error) {
	var files []string
	err := c.call("ExtensionFiles", []any{&files}, name)
	return files, err
}

func (c *client) InstallExtension(ref, version string, force bool) error {
	return c.call("InstallExtension", nil, ref, version, force)
}

func (c *client) SetExtensionEnabled(name string, on, force bool) error {
	return c.call("SetExtensionEnabled", nil, name, on, force)
}

func (c *client) RemoveExtension(name string) error {
	return c.call("RemoveExtension", nil, name)
}

// parseInterspersed parses flags that may come before or after the
// positional arguments, which the flag package alone stops at.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func describeExtError(err error) string {
	switch daemonErrName(err) {
	case "NotAuthorized":
		return "Only members of the admin group can change system extensions."
	case "Busy":
		return "The update service is busy with an update or another extension; try again when it ends."
	case "RebootPending", "Rebooting":
		return describeDaemonError(err)
	}
	return fmt.Sprint(err)
}

func extFail(stderr io.Writer, what string, err error) int {
	fmt.Fprintf(stderr, "hadron-ext: %s%s\n", what, describeExtError(err))
	return exitError
}

// cmdExt is `hadron-update ext` and `hadron-ext`.
func cmdExt(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if sub == "help" || sub == "-h" || sub == "--help" {
		fmt.Fprint(stdout, extUsage)
		return exitOK
	}
	fs := flag.NewFlagSet("hadron-ext "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, extUsage) }
	force := fs.Bool("force", false, "")
	yes := fs.Bool("yes", false, "")
	menu := fs.Bool("menu", false, "") // browse: lines for hadron-menu; not in the usage text
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage
	}
	usageErr := func(msg string) int {
		fmt.Fprintf(stderr, "hadron-ext: %s\n\n%s", msg, extUsage)
		return exitUsage
	}
	wantArgs := map[string][2]int{"list": {0, 0}, "browse": {0, 1}, "info": {1, 1}, "install": {1, 1},
		"enable": {1, 1}, "disable": {1, 1}, "remove": {1, 1}}
	n, known := wantArgs[sub]
	switch {
	case !known:
		return usageErr(fmt.Sprintf("unknown command %q", sub))
	case len(pos) < n[0] || len(pos) > n[1]:
		return usageErr(fmt.Sprintf("wrong number of arguments for %s", sub))
	case *force && sub != "install" && sub != "enable":
		return usageErr("--force is only for install and enable")
	case *menu && sub != "browse":
		return usageErr("--menu is only for browse")
	}
	if rel, err := LoadRelease(releaseFile()); err == nil && rel.Flavor == flavorAgent {
		fmt.Fprintln(stderr, "hadron-ext: system extensions are not available on the agent flavor.")
		return exitUsage
	}
	c, err := dialExt()
	if err != nil {
		fmt.Fprintln(stderr, "hadron-ext: The update service is not available (hadron-updated).")
		return exitError
	}
	defer c.Close()
	in := bufio.NewReader(stdin)
	arg := ""
	if len(pos) > 0 {
		arg = pos[0]
	}
	switch sub {
	case "list":
		return extList(c, stdout, stderr)
	case "browse":
		return extBrowse(c, arg, *menu, stdout, stderr)
	case "info":
		return extInfo(c, arg, stdout, stderr)
	case "install":
		return extInstall(c, arg, *force, *yes, in, stdout, stderr)
	case "enable", "disable":
		if !extName.MatchString(arg) {
			return usageErr(fmt.Sprintf("%q is not an extension name", arg))
		}
		on := sub == "enable"
		if *force {
			fmt.Fprintln(stdout, forceWarning)
		}
		code := extFollow(c, func() error { return c.SetExtensionEnabled(arg, on, *force) }, stdout, stderr)
		if code == exitConflict {
			fmt.Fprintf(stderr, "To enable it anyway, run `hadron-ext enable %s --force`.\n", arg)
			code = exitError
		}
		return code
	default: // remove
		if !extName.MatchString(arg) {
			return usageErr(fmt.Sprintf("%q is not an extension name", arg))
		}
		if !*yes && !confirm(in, stdout, fmt.Sprintf("Remove the extension %s? It is turned off and deleted.", arg), false) {
			fmt.Fprintln(stdout, "Cancelled.")
			return exitOK
		}
		return extFollow(c, func() error { return c.RemoveExtension(arg) }, stdout, stderr)
	}
}

const forceWarning = "Warning: --force enables the extension even when it puts other copies over files the system has. Programs that use those files may stop working; `hadron-ext disable` undoes it."

func extState(x InstalledExt) string {
	switch {
	case x.Enabled && x.Merged:
		return "on"
	case x.Enabled:
		return "on from the next boot"
	case x.Merged:
		return "off from the next boot"
	}
	return "off"
}

func extList(c extClient, stdout, stderr io.Writer) int {
	list, err := c.Extensions()
	if err != nil {
		return extFail(stderr, "", err)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "No system extensions are installed. Run `hadron-ext browse` to see what the catalogs offer.")
		return exitOK
	}
	w := 4
	for _, x := range list {
		w = max(w, len(x.Name))
	}
	fmt.Fprintf(stdout, "%-*s  %-22s  %s\n", w, "NAME", "STATE", "SOURCE")
	for _, x := range list {
		fmt.Fprintf(stdout, "%-*s  %-22s  %s\n", w, x.Name, extState(x), orUnknown(x.Source))
	}
	return exitOK
}

// menuURLItem is the browse --menu line that asks for a URL; hadron-menu
// matches it.
const menuURLItem = "Install from a URL…"

func extBrowse(c extClient, word string, menu bool, stdout, stderr io.Writer) int {
	var entries []CatalogEntry
	err := retryBusy(func() (err error) { entries, err = c.Catalog(); return })
	if err != nil {
		return extFail(stderr, "could not read the catalogs: ", err)
	}
	word = strings.ToLower(word)
	shown := 0
	for _, e := range entries {
		if word != "" && !strings.Contains(strings.ToLower(e.Name+" "+e.Title+" "+e.Description), word) {
			continue
		}
		shown++
		mark := " "
		if e.Installed {
			mark = "✓"
		}
		if menu {
			fmt.Fprintf(stdout, "%s %s — %s\n", mark, e.Name, orUnknown(e.Description))
			continue
		}
		fmt.Fprintf(stdout, "%s %-12s %-10s %s\n", mark, e.Name, e.Latest, e.Description)
	}
	if menu {
		if st, err := c.Status(); err == nil && st.Advanced {
			fmt.Fprintln(stdout, menuURLItem)
		}
		return exitOK
	}
	if shown == 0 {
		if word != "" {
			fmt.Fprintf(stdout, "No extension in the catalogs matches %q.\n", word)
		} else {
			fmt.Fprintln(stdout, "The catalogs offer no extension for this machine.")
		}
	}
	return exitOK
}

func extInfo(c extClient, name string, stdout, stderr io.Writer) int {
	var entries []CatalogEntry
	cerr := retryBusy(func() (err error) { entries, err = c.Catalog(); return })
	list, err := c.Extensions()
	if err != nil {
		return extFail(stderr, "", err)
	}
	var installed *InstalledExt
	for i := range list {
		if list[i].Name == name || list[i].File == name {
			installed = &list[i]
		}
	}
	var entry *CatalogEntry
	for i := range entries {
		if entries[i].Name == name {
			entry = &entries[i]
		}
	}
	if entry == nil && installed == nil {
		if cerr != nil {
			return extFail(stderr, "could not read the catalogs: ", cerr)
		}
		fmt.Fprintf(stderr, "hadron-ext: no catalog offers %q, and it is not installed.\n", name)
		return exitError
	}
	fmt.Fprintf(stdout, "Name:        %s\n", name)
	if entry != nil {
		if entry.Title != "" && entry.Title != entry.Name {
			fmt.Fprintf(stdout, "Title:       %s\n", entry.Title)
		}
		fmt.Fprintf(stdout, "Description: %s\nLatest:      %s\nVersions:    %s\nCatalog:     %s\n",
			orUnknown(entry.Description), entry.Latest, strings.Join(entry.Versions, ", "), entry.Catalog)
	}
	if installed == nil {
		fmt.Fprintln(stdout, "Installed:   no")
		fmt.Fprintln(stdout, "Files:       shown once it is installed")
		return exitOK
	}
	fmt.Fprintf(stdout, "Installed:   %s (%s)\nSource:      %s\n", installed.File, extState(*installed), orUnknown(installed.Source))
	var files []string
	if err := retryBusy(func() (err error) { files, err = c.ExtensionFiles(installed.Name); return }); err != nil {
		fmt.Fprintf(stdout, "Files:       could not list them: %s\n", describeExtError(err))
		return exitOK
	}
	fmt.Fprintln(stdout, "Files:")
	for _, f := range files {
		fmt.Fprintf(stdout, "  %s\n", f)
	}
	return exitOK
}

func extInstall(c extClient, arg string, force, yes bool, in *bufio.Reader, stdout, stderr io.Writer) int {
	st, err := c.Status()
	if err != nil {
		return extFail(stderr, "", err)
	}
	if arg == "-" {
		if !st.Advanced {
			fmt.Fprintln(stderr, "hadron-ext: advanced mode is off; only extensions from the catalogs can be installed.")
			return exitUsage
		}
		fmt.Fprint(stdout, "Extension URL or path: ")
		line, _ := in.ReadString('\n')
		if arg = strings.TrimSpace(line); arg == "" {
			fmt.Fprintln(stdout, "Cancelled.")
			return exitOK
		}
	}
	ref, version := arg, ""
	if !isExtReference(arg) {
		ref, version, _ = strings.Cut(arg, "@")
	}
	if err := checkExtRef(ref); err != nil {
		fmt.Fprintln(stderr, "hadron-ext:", err)
		return exitUsage
	}
	if isExtReference(ref) {
		if !st.Advanced {
			fmt.Fprintln(stderr, "hadron-ext: advanced mode is off; only extensions from the catalogs can be installed.")
			return exitUsage
		}
		fmt.Fprintf(stdout, "Extension:   %s\n", ref)
		fmt.Fprintln(stdout, "This extension is not from a catalog. It runs as root, and nobody checked it.")
		if force {
			fmt.Fprintln(stdout, forceWarning)
		}
		// --yes never answers this one, as for an image that is not a release.
		if !confirm(in, stdout, "Install it anyway?", false) {
			fmt.Fprintln(stdout, "Cancelled.")
			return exitOK
		}
	} else {
		var entries []CatalogEntry
		if err := retryBusy(func() (err error) { entries, err = c.Catalog(); return }); err != nil {
			return extFail(stderr, "could not read the catalogs: ", err)
		}
		var entry *CatalogEntry
		for i := range entries {
			if entries[i].Name == ref {
				entry = &entries[i]
			}
		}
		if entry == nil {
			fmt.Fprintf(stderr, "hadron-ext: no catalog offers %q for this machine; run `hadron-ext browse` to see what they offer.\n", ref)
			return exitError
		}
		if entry.Installed {
			fmt.Fprintf(stderr, "hadron-ext: %s is already installed.\n", ref)
			return exitError
		}
		fmt.Fprintf(stdout, "Extension:   %s\nDescription: %s\nVersion:     %s\nCatalog:     %s\n",
			entry.Name, orUnknown(entry.Description), orUnknown(map[bool]string{true: version, false: entry.Latest}[version != ""]), entry.Catalog)
		fmt.Fprintln(stdout, "It runs as root and adds files to /usr until you remove it. It is checked against the system before it is enabled.")
		if force {
			fmt.Fprintln(stdout, forceWarning)
		}
		if !yes && !confirm(in, stdout, "Install it?", true) {
			fmt.Fprintln(stdout, "Cancelled.")
			return exitOK
		}
	}
	code := extFollow(c, func() error { return c.InstallExtension(ref, version, force) }, stdout, stderr)
	if code == exitConflict {
		fmt.Fprintln(stderr, "It was removed again; nothing changed. To install it anyway, run")
		fmt.Fprintf(stderr, "`hadron-ext install %s --force`.\n", arg)
		code = exitError
	}
	return code
}

// exitConflict is extFollow's answer for a conflict, so the caller can add
// the --force hint; it never leaves the process.
const exitConflict = -1

// extFollow subscribes, starts the job, prints its progress and returns when
// the service leaves the extension state. As in installFlow, only a state
// change seen after the job started counts.
func extFollow(c extClient, start func() error, stdout, stderr io.Writer) int {
	events, stop, err := c.Watch()
	if err != nil {
		fmt.Fprintln(stderr, "hadron-ext:", err)
		return exitError
	}
	defer stop()
	if err := start(); err != nil {
		return extFail(stderr, "", err)
	}
	started := false
	for ev := range events {
		if ev.Line != "" {
			fmt.Fprintln(stdout, ev.Line)
		}
		if ev.Status == nil {
			continue
		}
		if ev.Status.State == StateExtension {
			started = true
			continue
		}
		if !started {
			continue
		}
		switch ev.Status.ExtResult {
		case extResultOK:
			fmt.Fprintln(stdout, ev.Status.ExtDetail)
			return exitOK
		case extResultConflict:
			fmt.Fprintln(stderr, "hadron-ext: refused:", ev.Status.ExtDetail)
			return exitConflict
		case extResultFailed:
			fmt.Fprintln(stderr, "hadron-ext: failed:", ev.Status.ExtDetail)
			return exitError
		}
		fmt.Fprintln(stderr, "hadron-ext: lost the extension job: the update service restarted.")
		return exitError
	}
	fmt.Fprintln(stderr, "hadron-ext: lost the connection to the update service before the change finished.")
	return exitError
}
