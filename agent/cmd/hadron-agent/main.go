// Command hadron-agent is the single MCP service binary for the Hadron "Cua
// agent appliance". It composes the Phase-2 packages into a small set of
// subcommands:
//
//	hadron-agent gateway       run the public HTTPS MCP gateway
//	hadron-agent session       run the unprivileged session broker
//	hadron-agent root-helper   run the privileged root helper
//	hadron-agent control ...   pause | resume | toggle the local runtime
//	hadron-agent status        print the local runtime status
//	hadron-agent version       print the build version and cua revision
//	hadron-agent provision     materialize machine TLS, digests, and configs
//	hadron-agent token rotate  rotate a user or admin bearer
//
// Exit codes: an unknown command or an unparseable flag exits 2; a runtime
// failure exits 1; -h/-help exits 0.
//
// This binary does NOT provision system users, TLS material, or systemd units.
// The gateway, session, and root-helper services take their configuration from
// the config.json that `hadron-agent provision` persists under the state dir,
// selected with --config; explicit flags and environment variables still
// override individual values (so tests and manual runs need no config file),
// and each service fails cleanly when a required value is absent from both.
// Materializing that config.json is Phase 3's provisioning responsibility.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
	"github.com/mudler/hadron-desktop/agent/internal/buildinfo"
	"github.com/mudler/hadron-desktop/agent/internal/control"
	"github.com/mudler/hadron-desktop/agent/internal/files"
	"github.com/mudler/hadron-desktop/agent/internal/gateway"
	"github.com/mudler/hadron-desktop/agent/internal/process"
	"github.com/mudler/hadron-desktop/agent/internal/provision"
	"github.com/mudler/hadron-desktop/agent/internal/roothelper"
	"github.com/mudler/hadron-desktop/agent/internal/rpc"
	"github.com/mudler/hadron-desktop/agent/internal/session"
	"github.com/mudler/hadron-desktop/agent/internal/watchdog"

	"golang.org/x/sys/unix"
)

// Environment variables that supply the bearer digests when the matching flag
// is not given. Digests are safe to carry in the environment; raw bearers are
// never accepted here.
const (
	envUserDigest  = "HADRON_AGENT_USER_DIGEST"
	envAdminDigest = "HADRON_AGENT_ADMIN_DIGEST"
)

// unusableDigest is a format-valid sha256 digest (all zeros) that no real
// bearer can ever hash to. It stands in for a class that a given process does
// not authenticate (e.g. the user class inside the root helper), so a Verifier
// can always be constructed while that class remains effectively disabled.
const unusableDigest = auth.Digest("sha256:0000000000000000000000000000000000000000000000000000000000000000")

// errUsage marks a usage/flag error: an unknown flag, a missing or unknown
// positional argument. It maps to exit code 2.
var errUsage = errors.New("usage")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches argv (excluding the program name) to the selected subcommand
// and maps the result to a process exit code: 0 on success or -h/-help, 2 for
// an unknown command or a usage/flag error, 1 for any other runtime failure.
// It is the single testable entry point; main only wires it to os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	cmd, rest := args[0], args[1:]

	var err error
	switch cmd {
	case "version", "-v", "--version":
		err = runVersion(rest, stdout, stderr)
	case "gateway":
		err = runGateway(rest, stderr)
	case "session":
		err = runSession(rest, stderr)
	case "root-helper":
		err = runRootHelper(rest, stderr)
	case "control":
		err = runControl(rest, stdout, stderr)
	case "status":
		err = runStatus(rest, stdout, stderr)
	case "display-watchdog":
		err = runDisplayWatchdog(rest, stderr)
	case "provision":
		err = runProvision(rest, stdout, stderr)
	case "token":
		err = runToken(rest, stdout, stderr)
	case "-h", "--help", "help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "hadron-agent: unknown command %q\n", cmd)
		usage(stderr)
		return 2
	}

	if err == nil {
		return 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if errors.Is(err, errUsage) {
		fmt.Fprintf(stderr, "hadron-agent %s: %v\n", cmd, err)
		return 2
	}
	fmt.Fprintf(stderr, "hadron-agent %s: %v\n", cmd, err)
	return 1
}

func usage(w io.Writer) {
	fmt.Fprint(w, `hadron-agent - MCP service binary for the Hadron Cua agent appliance

Usage:
  hadron-agent <command> [flags]

Commands:
  gateway       Run the public HTTPS MCP gateway.
  session       Run the unprivileged session broker.
  root-helper   Run the privileged root helper.
  control       pause | resume | toggle the local runtime.
  status        Print the local runtime status.
  display-watchdog  Keep the agent's foreground XLibre/i3 session alive.
  version       Print the build version and cua-driver revision.
  provision     Materialize machine TLS, bearer digests, and service configs.
  token         rotate user | admin bearer.

Run "hadron-agent <command> -h" for command-specific flags.
`)
}

// newFlagSet builds a FlagSet in ContinueOnError mode whose parse errors are
// written to stderr. It centralizes the flag-error -> exit-code mapping: -h
// returns flag.ErrHelp (exit 0), any other parse failure is wrapped in errUsage
// (exit 2).
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags runs fs.Parse and normalizes its error into the sentinel scheme.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return fmt.Errorf("%w: %v", errUsage, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// version
// ---------------------------------------------------------------------------

func runVersion(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("version", stderr)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	fmt.Fprintln(stdout, buildinfo.String())
	return nil
}

// ---------------------------------------------------------------------------
// gateway
// ---------------------------------------------------------------------------

func runGateway(args []string, stderr io.Writer) error {
	fs := newFlagSet("gateway", stderr)
	configPath := fs.String("config", "", "path to the provisioned gateway config.json (explicit flags/env override its values)")
	listen := fs.String("listen", gateway.DefaultListenAddr, "host:port to bind the HTTPS MCP endpoint")
	tlsCert := fs.String("tls-cert", "", "path to the server TLS certificate (PEM)")
	tlsKey := fs.String("tls-key", "", "path to the server TLS private key (PEM)")
	insecure := fs.Bool("insecure-loopback", false, "serve plain HTTP; permitted ONLY on a loopback --listen address")
	sessSock := fs.String("session-socket", rpc.SessionSocketPath, "path of the session broker's Unix socket")
	rootSock := fs.String("root-socket", rpc.RootSocketPath, "path of the root helper's Unix socket")
	ctrlSock := fs.String("control-socket", rpc.ControlSocketPath, "path of the local control Unix socket")
	statusFile := fs.String("status-file", gateway.DefaultStatusFile, "path the redacted status.json is published to (empty disables the writer)")
	userDigest := fs.String("user-digest", "", "sha256 digest of the user bearer (default $"+envUserDigest+")")
	adminDigest := fs.String("admin-digest", "", "sha256 digest of the admin bearer (default $"+envAdminDigest+")")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	set := flagsSet(fs)

	logger := newLogger(stderr)

	// When --config is given, every value below is derived from the provisioned
	// config.json unless an explicit flag or environment variable overrides it.
	var gwCfg *provision.GatewayConfig
	if *configPath != "" {
		loaded, err := provision.LoadGatewayConfig(*configPath)
		if err != nil {
			return err
		}
		gwCfg = &loaded
	}

	// Bearer verifier: an explicit --user-digest/--admin-digest (or its env)
	// replaces that class's whole rotation with a single current digest;
	// otherwise the provisioned rotation (current + bounded-overlap previous) is
	// honored; otherwise the class is disabled with the unusable digest.
	var userSrc, adminSrc *provision.Rotation
	if gwCfg != nil {
		userSrc, adminSrc = &gwCfg.User, &gwCfg.Admin
	}
	userRot, err := resolveRotation(*userDigest, envUserDigest, userSrc)
	if err != nil {
		return err
	}
	adminRot, err := resolveRotation(*adminDigest, envAdminDigest, adminSrc)
	if err != nil {
		return err
	}
	if userRot.Current == unusableDigest && adminRot.Current == unusableDigest {
		return fmt.Errorf("no bearer digests configured: set --config, --user-digest/--admin-digest, or $%s/$%s", envUserDigest, envAdminDigest)
	}
	verifier, err := auth.NewVerifier(userRot, adminRot)
	if err != nil {
		return err
	}

	// listen / insecure-loopback / TLS: explicit flag wins, else the provisioned
	// value.
	listenAddr := *listen
	if gwCfg != nil && !set["listen"] {
		listenAddr = gwCfg.Listen
	}
	insecureLoopback := *insecure
	if gwCfg != nil && !set["insecure-loopback"] {
		insecureLoopback = gwCfg.InsecureLoopback
	}
	certPath, keyPath := *tlsCert, *tlsKey
	if gwCfg != nil {
		if certPath == "" {
			certPath = gwCfg.TLSCertPath
		}
		if keyPath == "" {
			keyPath = gwCfg.TLSKeyPath
		}
	}
	tlsConf, err := loadTLS(certPath, keyPath)
	if err != nil {
		return err
	}

	// Limits: the provisioned bounds map onto the gateway's; zero leaves the
	// gateway's own defaults in place.
	var maxConcurrent int
	var maxBodyBytes int64
	if gwCfg != nil {
		maxConcurrent = gwCfg.Limits.MaxConcurrentCalls
		maxBodyBytes = int64(gwCfg.Limits.MaxRequestBytes)
	}

	// Static values republished (redacted) in status.json: the certificate
	// fingerprint and mDNS flag come from the provisioned config; the listen
	// address the gateway resolves is reported by the gateway itself. Absent a
	// --config, both are empty/false.
	var certFingerprint string
	var mdns bool
	if gwCfg != nil {
		certFingerprint = gwCfg.CertFingerprint
		mdns = gwCfg.MDNS
	}

	sessionClient := rpc.NewClient(*sessSock)
	defer sessionClient.Close()
	rootClient := rpc.NewClient(*rootSock)
	defer rootClient.Close()

	ctrl := control.New(control.Config{
		Version: buildinfo.Version,
		// The controller reaches the session broker over its NON-authenticated
		// lifecycle path (the session socket). Root-helper pause is gated behind
		// the auth'd root socket and is wired by Phase-3 provisioning; the local
		// generation cancellation below already stops all forwarding on pause.
		Brokers:        []control.Broker{lifecycleBroker{client: sessionClient}},
		SessionHealthy: healthProbe(sessionClient),
		Logger:         logger,
	})

	g, err := gateway.New(gateway.Config{
		Version:            buildinfo.Version,
		ListenAddr:         listenAddr,
		TLSConfig:          tlsConf,
		InsecureLoopback:   insecureLoopback,
		Session:            sessionClient,
		Root:               rootClient,
		Verifier:           verifier,
		Controller:         ctrl,
		Logger:             logger,
		MaxConcurrentCalls: maxConcurrent,
		MaxBodyBytes:       maxBodyBytes,
		CertFingerprint:    certFingerprint,
		MDNS:               mdns,
		StatusFile:         *statusFile,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Serve the local control socket so `hadron-agent control ...` can drive the
	// in-process controller. Runs alongside the HTTPS gateway.
	ctrlErr := make(chan error, 1)
	go func() {
		ctrlErr <- serveUnix(ctx, *ctrlSock, control.NewSocketHandler(ctrl), false, logger)
	}()

	l, err := net.Listen("tcp", g.ListenAddr())
	if err != nil {
		return fmt.Errorf("bind %s: %w", g.ListenAddr(), err)
	}
	go func() {
		<-ctx.Done()
		_ = l.Close()
	}()

	// Publish the redacted status.json once the listener is up, until shutdown.
	// A nil/empty --status-file disables the writer (RunStatusWriter returns at
	// once), so a manual run without one touches no status directory.
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		g.RunStatusWriter(ctx, gateway.DefaultStatusInterval)
	}()

	logger.Info("gateway listening", slog.String("addr", g.ListenAddr()), slog.Bool("tls", tlsConf != nil))
	serveErr := g.Serve(l)
	if errors.Is(serveErr, net.ErrClosed) {
		serveErr = nil
	}

	// Stop the status writer (ctx is already cancelled on shutdown) and give the
	// control socket goroutine a moment to unwind.
	<-statusDone
	select {
	case <-ctrlErr:
	case <-time.After(2 * time.Second):
	}
	return serveErr
}

// ---------------------------------------------------------------------------
// session
// ---------------------------------------------------------------------------

func runSession(args []string, stderr io.Writer) error {
	fs := newFlagSet("session", stderr)
	configPath := fs.String("config", "", "path to the provisioned session config.json (explicit flags override its values)")
	sock := fs.String("socket", rpc.SessionSocketPath, "path of the session broker's Unix socket to serve")
	cuaBinary := fs.String("cua-binary", "cua-driver", "cua-driver executable for computer_use")
	osConcurrency := fs.Int("os-concurrency", 0, "max concurrent shell/file calls (0 = default)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	set := flagsSet(fs)

	logger := newLogger(stderr)

	// Provisioned limits bound the broker: max_concurrent_calls caps concurrent
	// shell/file calls and max_processes caps live processes. An explicit
	// --os-concurrency still overrides the concurrency bound.
	var limits provision.ServiceLimits
	if *configPath != "" {
		sc, err := provision.LoadSessionConfig(*configPath)
		if err != nil {
			return err
		}
		limits = sc.Limits
	}
	osConc := *osConcurrency
	if !set["os-concurrency"] && limits.MaxConcurrentCalls > 0 {
		osConc = limits.MaxConcurrentCalls
	}
	procCfg := process.DefaultConfig()
	if limits.MaxProcesses > 0 {
		procCfg.MaxLive = limits.MaxProcesses
	}

	broker := session.New(session.Config{
		Files:         files.NewService(),
		Process:       process.New(procCfg),
		Computer:      session.NewCuaComputer(session.CuaConfig{Binary: *cuaBinary}),
		OSConcurrency: osConc,
	})
	defer func() { _ = broker.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("session broker serving", slog.String("socket", *sock))
	// The session socket is NON-authenticated: the gateway routes to it and the
	// controller drives its pause/resume lifecycle over the same socket.
	return serveUnix(ctx, *sock, sessionHandler{broker: broker}, false, logger)
}

// ---------------------------------------------------------------------------
// root-helper
// ---------------------------------------------------------------------------

func runRootHelper(args []string, stderr io.Writer) error {
	fs := newFlagSet("root-helper", stderr)
	configPath := fs.String("config", "", "path to the provisioned root config.json (explicit flag/env overrides its value)")
	sock := fs.String("socket", rpc.RootSocketPath, "path of the root helper's Unix socket to serve")
	adminDigest := fs.String("admin-digest", "", "sha256 digest of the admin bearer (default $"+envAdminDigest+")")
	osConcurrency := fs.Int("os-concurrency", 0, "max concurrent OS calls (0 = default)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	logger := newLogger(stderr)

	// The admin digest comes from the provisioned root config.json unless an
	// explicit --admin-digest (or its env) overrides it, honoring the persisted
	// rotation (current + bounded-overlap previous).
	var adminSrc *provision.Rotation
	if *configPath != "" {
		rc, err := provision.LoadRootConfig(*configPath)
		if err != nil {
			return err
		}
		adminSrc = &rc.Admin
	}
	adminRot, err := resolveRotation(*adminDigest, envAdminDigest, adminSrc)
	if err != nil {
		return err
	}
	if adminRot.Current == unusableDigest {
		return fmt.Errorf("no admin digest configured: set --config, --admin-digest or $%s", envAdminDigest)
	}
	// The root helper authenticates ONLY the admin class; the user class is left
	// unusable so a user bearer can never reach a privileged executor.
	verifier, err := auth.NewVerifier(auth.RotationConfig{Current: unusableDigest}, adminRot)
	if err != nil {
		return err
	}

	helper, err := roothelper.New(roothelper.Config{
		Verifier:      verifier,
		Files:         files.NewService(),
		Process:       process.New(process.DefaultConfig()),
		OSConcurrency: *osConcurrency,
	})
	if err != nil {
		return err
	}
	defer func() { _ = helper.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("root helper serving", slog.String("socket", *sock))
	// The root socket REQUIRES an Authorization header on every request; the
	// helper re-verifies the forwarded admin bearer locally before dispatch.
	return serveUnix(ctx, *sock, rootHandler{helper: helper}, true, logger)
}

// ---------------------------------------------------------------------------
// control
// ---------------------------------------------------------------------------

func runControl(args []string, stdout, stderr io.Writer) error {
	// The action is a positional argument that precedes any flags:
	//   hadron-agent control pause --socket /path
	var action string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}

	fs := newFlagSet("control", stderr)
	sock := fs.String("socket", rpc.ControlSocketPath, "path of the local control Unix socket")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if action == "" {
		return fmt.Errorf("%w: control requires an action: pause | resume | toggle", errUsage)
	}

	client := rpc.NewClient(*sock)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch action {
	case "pause":
		if err := client.Pause(ctx, ""); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "paused")
	case "resume":
		if err := client.Resume(ctx, ""); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "resumed")
	case "toggle":
		h, err := client.Health(ctx, "")
		if err != nil {
			return err
		}
		if h.Paused {
			if err := client.Resume(ctx, ""); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "resumed")
		} else {
			if err := client.Pause(ctx, ""); err != nil {
				return err
			}
			fmt.Fprintln(stdout, "paused")
		}
	default:
		return fmt.Errorf("%w: unknown control action %q (want pause | resume | toggle)", errUsage, action)
	}
	return nil
}

// ---------------------------------------------------------------------------
// status
// ---------------------------------------------------------------------------

func runStatus(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("status", stderr)
	sock := fs.String("socket", rpc.ControlSocketPath, "path of the local control Unix socket")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	client := rpc.NewClient(*sock)
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	h, err := client.Health(ctx, "")
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "version: %s\nready:   %t\npaused:  %t\n", buildinfo.Version, h.OK, h.Paused)
	return nil
}

// ---------------------------------------------------------------------------
// display-watchdog
// ---------------------------------------------------------------------------

// runDisplayWatchdog keeps the agent's foreground XLibre/i3 session alive. It
// requires BOTH an agent logind graphical session AND an agent-owned i3
// process; if either is absent past the grace period it restarts ly@tty1.service
// so Ly performs its one allowed autologin again. It NEVER launches a display
// itself. Restarts are rate-limited (three per five minutes) after which the
// watchdog reports degraded rather than thrashing tty1.
//
// The unit that runs this carries ConditionKernelCommandLine=!install-mode; as
// a belt-and-braces guard the binary also refuses to run when it detects
// install-mode on the kernel command line, so it can never fight the installer
// for tty1.
func runDisplayWatchdog(args []string, stderr io.Writer) error {
	fs := newFlagSet("display-watchdog", stderr)
	user := fs.String("user", "agent", "account whose graphical session is guarded")
	grace := fs.Duration("grace", watchdog.DefaultGrace, "how long the session may be absent before restarting Ly")
	poll := fs.Duration("poll", watchdog.DefaultPollInterval, "how often to sample the session state")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	if installModeActive() {
		return errors.New("refusing to run in install-mode: the installer owns tty1")
	}

	logger := newLogger(stderr)
	w := watchdog.New(watchdog.Config{
		User:         *user,
		Grace:        *grace,
		PollInterval: *poll,
		Detector:     watchdog.NewSystemDetector(*user),
		Restarter:    watchdog.NewSystemRestarter(),
		Logger:       logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("display watchdog starting",
		slog.String("user", *user), slog.Duration("grace", *grace))
	if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// installModeActive reports whether the kernel command line requests
// install-mode. The watchdog refuses to run in that case so it never restarts
// Ly while the installer needs tty1.
func installModeActive() bool {
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(b)) {
		if f == "install-mode" {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// provision
// ---------------------------------------------------------------------------

// runProvision materializes the machine's persistent state (TLS identity,
// bearer digests, and service config files) from the merged /oem cloud-config.
// It is root-only in production: writing into the default system state dir
// requires root, but pointing --state-dir at a temporary directory (as tests
// and tooling do) is permitted unprivileged, in which case ownership is
// recorded but not applied.
func runProvision(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("provision", stderr)
	oemDir := fs.String("oem-dir", "/oem", "directory of Kairos OEM *.yaml cloud-config files")
	stateDir := fs.String("state-dir", provision.DefaultStateDir, "persistent machine-state directory")
	runtimeDir := fs.String("runtime-dir", provision.DefaultRuntimeDir, "ephemeral runtime (tmpfs) directory")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	root := os.Geteuid() == 0
	if *stateDir == provision.DefaultStateDir && !root {
		return fmt.Errorf("provision must run as root to write %s", provision.DefaultStateDir)
	}

	cfg, err := provision.Load(*oemDir)
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		fmt.Fprintln(stdout, "hadron_agent is disabled; nothing to provision")
		return nil
	}

	m := provision.NewMaterializer(provision.Options{
		StateDir:   *stateDir,
		RuntimeDir: *runtimeDir,
		Chown:      root,
	})
	res, err := m.Materialize(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "provisioned: fingerprint=%s admin=%t first_run_token=%t\n",
		res.CertFingerprint, res.AdminEnabled, res.TokenWritten)
	return nil
}

// ---------------------------------------------------------------------------
// token rotate
// ---------------------------------------------------------------------------

// runToken dispatches the `token` subcommands (currently only `rotate`).
func runToken(args []string, stdout, stderr io.Writer) error {
	var action string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		action, args = args[0], args[1:]
	}
	switch action {
	case "rotate":
		return runTokenRotate(args, stdout, stderr)
	case "":
		return fmt.Errorf("%w: token requires an action: rotate", errUsage)
	default:
		return fmt.Errorf("%w: unknown token action %q (want rotate)", errUsage, action)
	}
}

// runTokenRotate rotates a user or admin bearer. The new bearer is printed
// ONLY to the controlling terminal: when stdout is not a TTY (a journal or a
// pipe) the rotation still happens but the secret is never emitted. A failed
// service reload rolls the persisted digest state back.
func runTokenRotate(args []string, stdout, stderr io.Writer) error {
	// The class is a positional argument preceding the flags:
	//   hadron-agent token rotate user --overlap 15m
	var classArg string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		classArg, args = args[0], args[1:]
	}

	fs := newFlagSet("token rotate", stderr)
	overlap := fs.Duration("overlap", 15*time.Minute, "how long the outgoing bearer stays valid (max 24h)")
	stateDir := fs.String("state-dir", provision.DefaultStateDir, "persistent machine-state directory")
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	var class auth.Class
	switch classArg {
	case "user":
		class = auth.ClassUser
	case "admin":
		class = auth.ClassAdmin
	case "":
		return fmt.Errorf("%w: token rotate requires a class: user | admin", errUsage)
	default:
		return fmt.Errorf("%w: unknown bearer class %q (want user | admin)", errUsage, classArg)
	}

	root := os.Geteuid() == 0
	if *stateDir == provision.DefaultStateDir && !root {
		return fmt.Errorf("token rotate must run as root to write %s", provision.DefaultStateDir)
	}

	// Present the secret only when stdout is a real terminal.
	var tty io.Writer
	if f, ok := stdout.(*os.File); ok && isTerminal(f) {
		tty = f
	}

	m := provision.NewMaterializer(provision.Options{StateDir: *stateDir, Chown: root})
	res, err := m.Rotate(provision.RotateOptions{
		Class:   class,
		Overlap: *overlap,
		TTY:     tty,
		Reload:  reloadServices,
	})
	if err != nil {
		return err
	}
	if !res.Printed {
		// No TTY: the secret was withheld on purpose. Report the digest (safe
		// to log) so the operator knows the rotation took effect.
		fmt.Fprintf(stderr, "rotated %s bearer (no terminal; new bearer withheld). new digest: %s\n", class, res.Digest)
	}
	return nil
}

// reloadServices signals the running gateway (and, when present, the root
// helper) to reload their bearer digests after a rotation. Wiring the actual
// systemd reload is Phase-3 Task 5; until then this is a no-op hook so a
// rotation on a not-yet-serviced system still succeeds.
func reloadServices() error { return nil }

// isTerminal reports whether f refers to a terminal, using a TCGETS ioctl (the
// same, cgo-free mechanism as golang.org/x/term on Linux).
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// ---------------------------------------------------------------------------
// Wiring adapters
// ---------------------------------------------------------------------------

// sessionHandler adapts a *session.Broker to the rpc.Handler served on the
// (non-authenticated) session socket. The session broker does not itself
// consume the forwarded Authorization header, so Call ignores it.
type sessionHandler struct{ broker *session.Broker }

func (h sessionHandler) Call(ctx context.Context, req rpc.CallRequest, _ string) (*rpc.CallResponse, error) {
	return h.broker.Call(ctx, req.Tool, req.Arguments)
}

func (h sessionHandler) Health(_ context.Context) (rpc.HealthStatus, error) {
	s := h.broker.Status()
	// Shell/file tools stay live whenever the broker is not paused, regardless
	// of Cua readiness, so OK tracks the pause state.
	return rpc.HealthStatus{OK: !s.Paused, Paused: s.Paused}, nil
}

func (h sessionHandler) Pause(ctx context.Context) error  { return h.broker.Pause(ctx) }
func (h sessionHandler) Resume(ctx context.Context) error { return h.broker.Resume(ctx) }

// rootHandler adapts a *roothelper.Helper to the rpc.Handler served on the
// (authenticated) root socket. Call forwards the raw Authorization header so
// the helper can re-verify the admin bearer locally.
type rootHandler struct{ helper *roothelper.Helper }

func (h rootHandler) Call(ctx context.Context, req rpc.CallRequest, authHeader string) (*rpc.CallResponse, error) {
	return h.helper.Call(ctx, req.Tool, req.Arguments, authHeader)
}

func (h rootHandler) Health(_ context.Context) (rpc.HealthStatus, error) {
	s := h.helper.Status()
	return rpc.HealthStatus{OK: !s.Paused, Paused: s.Paused}, nil
}

func (h rootHandler) Pause(ctx context.Context) error  { return h.helper.Pause(ctx) }
func (h rootHandler) Resume(ctx context.Context) error { return h.helper.Resume(ctx) }

// lifecycleBroker adapts an *rpc.Client to control.Broker so the controller can
// fan pause/resume out to a broker over its lifecycle socket. auth is the
// Authorization header to present (empty for a non-authenticated socket).
type lifecycleBroker struct {
	client *rpc.Client
	auth   string
}

func (b lifecycleBroker) Pause(ctx context.Context) error  { return b.client.Pause(ctx, b.auth) }
func (b lifecycleBroker) Resume(ctx context.Context) error { return b.client.Resume(ctx, b.auth) }

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// resolveDigest returns the flag value if set, otherwise the named environment
// variable, as an auth.Digest ("" when neither is present).
func resolveDigest(flagVal, envKey string) auth.Digest {
	if flagVal != "" {
		return auth.Digest(flagVal)
	}
	return auth.Digest(os.Getenv(envKey))
}

// flagsSet returns the set of flag names that were explicitly present on the
// command line, so a value provisioned via --config can be distinguished from a
// flag left at its default (and correctly overridden only when set).
func flagsSet(fs *flag.FlagSet) map[string]bool {
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// resolveRotation selects the bearer rotation for one class. An explicit flag
// value, or the named environment variable, takes precedence and installs a
// single current digest with no rotation window. Otherwise a provisioned
// rotation is honored (current + bounded-overlap previous). When neither is
// present the class is disabled with the unusable all-zero digest, so the
// Verifier still validates while no real bearer can ever authenticate.
func resolveRotation(flagVal, envKey string, provisioned *provision.Rotation) (auth.RotationConfig, error) {
	if d := resolveDigest(flagVal, envKey); d != "" {
		return auth.RotationConfig{Current: d}, nil
	}
	if provisioned != nil && provisioned.Current != "" {
		return provisioned.AuthRotation()
	}
	return auth.RotationConfig{Current: unusableDigest}, nil
}

// buildVerifier constructs a Verifier for the two bearer classes. An empty
// digest for a class is replaced with the unusable all-zero digest so that
// class is effectively disabled while the Verifier still validates.
func buildVerifier(userDigest, adminDigest auth.Digest) (*auth.Verifier, error) {
	if userDigest == "" {
		userDigest = unusableDigest
	}
	if adminDigest == "" {
		adminDigest = unusableDigest
	}
	return auth.NewVerifier(
		auth.RotationConfig{Current: userDigest},
		auth.RotationConfig{Current: adminDigest},
	)
}

// loadTLS loads a certificate/key pair into a *tls.Config, or returns (nil,
// nil) when neither path is given (the caller then relies on --insecure-loopback
// for a loopback bind). Supplying only one of the two is an error.
func loadTLS(certPath, keyPath string) (*tls.Config, error) {
	if certPath == "" && keyPath == "" {
		return nil, nil
	}
	if certPath == "" || keyPath == "" {
		return nil, errors.New("both --tls-cert and --tls-key must be provided together")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load TLS keypair: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
}

// healthProbe returns a control.HealthFunc that reports a broker healthy when
// its Health RPC succeeds and returns OK.
func healthProbe(client *rpc.Client) control.HealthFunc {
	return func(ctx context.Context) bool {
		s, err := client.Health(ctx, "")
		return err == nil && s.OK
	}
}

// newLogger builds a structured JSON logger writing to w.
func newLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, nil))
}

// serveUnix creates (and cleans up) the Unix socket at path, serves handler on
// it with the given auth requirement, and shuts the server down when ctx is
// cancelled. It creates the parent directory and removes any stale socket file
// first; Phase-3 provisioning owns the socket's ownership and permissions.
func serveUnix(ctx context.Context, path string, handler rpc.Handler, requireAuth bool, logger *slog.Logger) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create socket dir for %s: %w", path, err)
	}
	// Remove a stale socket left by a previous run so Listen does not fail with
	// "address already in use".
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale socket %s: %w", path, err)
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("listen unix %s: %w", path, err)
	}

	srv := rpc.NewServer(handler, rpc.Config{RequireAuth: requireAuth})
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	err = srv.Serve(l)
	_ = os.Remove(path)
	return err
}
