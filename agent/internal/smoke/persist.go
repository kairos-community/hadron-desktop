package smoke

import (
	"context"
	"fmt"
	"strings"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// PersistenceMarkerPath is the on-disk file the install-gate persistence check
// writes before a reboot and reads back after it, proving installed state
// survives an OS-driven reboot. It lives under the unprivileged agent user's
// e2e working directory.
const PersistenceMarkerPath = e2eDir + "/persistence-marker"

// RunPersistWrite writes the persistence marker as the unprivileged user, then
// verifies the admin credential still resolves to root and asks the appliance
// to reboot with `systemctl reboot`. The reboot tool call itself may return an
// error because the session drops as systemd tears the gateway down; that is
// expected and is NOT treated as a failure -- the harness detects the reboot by
// watching the forwarded port. This is invoked as `--mode persist-write`.
func (s *Suite) RunPersistWrite(ctx context.Context, marker string) Report {
	r := Report{Mode: "persist-write"}

	userSess, err := s.Client.Connect(ctx, s.UserBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_user", "could not open the user MCP session"))
		return r
	}
	defer userSess.Close()

	var wr api.WriteFileOutput
	if err := s.callInto(ctx, userSess, api.ToolWriteFile, api.WriteFileInput{
		Path: PersistenceMarkerPath, Content: marker, CreateParents: true,
	}, &wr); err != nil {
		r.Checks = append(r.Checks, failTransport("persist_write_marker", "write_file call failed"))
		return r
	}
	if wr.Code != "" {
		r.Checks = append(r.Checks, failAssert("persist_write_marker", fmt.Sprintf("write_file reported %s", wr.Code)))
		return r
	}
	r.Checks = append(r.Checks, pass("persist_write_marker", "wrote the persistence marker under the agent e2e dir"))

	adminSess, err := s.Client.Connect(ctx, s.AdminBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_admin", "could not open the admin MCP session"))
		return r
	}
	defer adminSess.Close()

	// Prove the admin credential is genuinely root before we trust it to
	// reboot; a reboot that "worked" through an unauthenticated path would be
	// meaningless.
	out, res := s.terminalID(ctx, adminSess)
	if res != nil {
		r.Checks = append(r.Checks, res.named("persist_admin_root"))
		return r
	}
	if out.uid != 0 || out.user != "root" {
		r.Checks = append(r.Checks, failAssert("persist_admin_root", fmt.Sprintf("admin terminal ran as uid=%d user=%s, want root", out.uid, out.user)))
		return r
	}
	r.Checks = append(r.Checks, pass("persist_admin_root", "admin terminal executes as root before reboot"))

	// Ask the appliance to reboot. The call may return an error as the gateway
	// goes down; either way the request has been delivered, so we record it as
	// issued and let the harness observe the reboot via the forwarded port.
	var term api.TerminalOutput
	_ = s.callInto(ctx, adminSess, api.ToolTerminal, api.TerminalInput{Command: "systemctl reboot"}, &term)
	r.Checks = append(r.Checks, pass("persist_reboot_requested", "requested systemctl reboot via the admin path"))
	return r
}

// RunPersistVerify runs after the reboot: it reads the marker back (proving
// installed on-disk state and the user digest survived), reconfirms admin-root
// and that the agent account is still locked, and records the hostname. Every
// successful connection with the SAME bearers additionally proves the endpoint
// config and the digest identity persisted across the reboot. This is invoked
// as `--mode persist-verify`.
func (s *Suite) RunPersistVerify(ctx context.Context, marker string) Report {
	r := Report{Mode: "persist-verify"}

	userSess, err := s.Client.Connect(ctx, s.UserBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_user", "could not open the user MCP session (digest/endpoint did not persist?)"))
		return r
	}
	defer userSess.Close()

	var rd api.ReadFileOutput
	if err := s.callInto(ctx, userSess, api.ToolReadFile, api.ReadFileInput{Path: PersistenceMarkerPath}, &rd); err != nil {
		r.Checks = append(r.Checks, failTransport("persist_read_marker", "read_file call failed"))
	} else if rd.Code != "" {
		r.Checks = append(r.Checks, failAssert("persist_read_marker", fmt.Sprintf("read_file reported %s", rd.Code)))
	} else if strings.TrimSpace(rd.Content) != strings.TrimSpace(marker) {
		r.Checks = append(r.Checks, failAssert("persist_read_marker", "marker content did not survive the reboot"))
	} else {
		r.Checks = append(r.Checks, pass("persist_read_marker", "persistence marker survived the reboot with matching content"))
	}

	// Hostname proves endpoint/host identity is stable across the reboot.
	var hn api.TerminalOutput
	if err := s.callInto(ctx, userSess, api.ToolTerminal, api.TerminalInput{Command: "hostname"}, &hn); err != nil {
		r.Checks = append(r.Checks, failTransport("persist_hostname", "hostname call failed"))
	} else if h := strings.TrimSpace(hn.Stdout); h == "" {
		r.Checks = append(r.Checks, failAssert("persist_hostname", "hostname was empty after reboot"))
	} else {
		r.Checks = append(r.Checks, pass("persist_hostname", "installed hostname present after reboot: "+h))
	}

	adminSess, err := s.Client.Connect(ctx, s.AdminBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_admin", "could not open the admin MCP session (admin digest did not persist?)"))
		return r
	}
	defer adminSess.Close()

	out, res := s.terminalID(ctx, adminSess)
	if res != nil {
		r.Checks = append(r.Checks, res.named("persist_admin_root"))
	} else if out.uid != 0 || out.user != "root" {
		r.Checks = append(r.Checks, failAssert("persist_admin_root", fmt.Sprintf("admin terminal ran as uid=%d user=%s, want root", out.uid, out.user)))
	} else {
		r.Checks = append(r.Checks, pass("persist_admin_root", "admin digest still resolves to root after reboot"))
	}

	// The locked, non-admin agent account must persist across the install+reboot.
	var ps api.TerminalOutput
	if err := s.callInto(ctx, adminSess, api.ToolTerminal, api.TerminalInput{Command: "passwd -S agent"}, &ps); err != nil {
		r.Checks = append(r.Checks, failTransport("persist_agent_locked", "passwd -S call failed"))
	} else if locked, ok := parsePasswdLocked(ps.Stdout); !ok {
		r.Checks = append(r.Checks, failAssert("persist_agent_locked", "could not parse passwd -S output for the agent account"))
	} else if !locked {
		r.Checks = append(r.Checks, failAssert("persist_agent_locked", "the agent account is not locked after reboot"))
	} else {
		r.Checks = append(r.Checks, pass("persist_agent_locked", "the agent account remains locked after reboot"))
	}

	// The whole point of the appliance is remote control of the desktop and the
	// browser. Proving that files and identities survived a reboot says nothing
	// about whether it can still DO anything, and the two capabilities are
	// exactly what a reboot was most likely to break: the desktop session has to
	// come back on the real seat, and the computer-use backend has to start
	// again underneath it.
	//
	// Both are retried while the lazily-started backend comes up, because the
	// appliance advertises SESSION_UNAVAILABLE as retryable and a gate should
	// not assert a stricter contract than the one it is testing.
	var cu api.ComputerUseOutput
	cuMeta := s.retryWhileUnavailable(ctx, func() api.ResultMeta {
		cu = api.ComputerUseOutput{}
		if err := s.callInto(ctx, userSess, api.ToolComputerUse,
			api.ComputerUseInput{Action: api.ActionCapture}, &cu); err != nil {
			return api.ResultMeta{Code: api.CodeInternal, Message: err.Error()}
		}
		return cu.ResultMeta
	})
	switch {
	case cuMeta.Code != "":
		r.Checks = append(r.Checks, failAssert("persist_computer_use",
			fmt.Sprintf("computer_use reported %s after reboot", cuMeta.Code)))
	case cu.ImageBase64 == "":
		r.Checks = append(r.Checks, failAssert("persist_computer_use",
			"computer_use returned no image after reboot"))
	default:
		r.Checks = append(r.Checks, pass("persist_computer_use",
			fmt.Sprintf("the desktop is controllable after reboot (%d-byte capture)", len(cu.ImageBase64))))
	}

	var br api.BrowserOutput
	brMeta := s.retryWhileUnavailable(ctx, func() api.ResultMeta {
		br = api.BrowserOutput{}
		if err := s.callInto(ctx, userSess, api.ToolBrowser,
			api.BrowserInput{Action: api.BrowserSnapshot}, &br); err != nil {
			return api.ResultMeta{Code: api.CodeInternal, Message: err.Error()}
		}
		return br.ResultMeta
	})
	switch brMeta.Code {
	case "":
		r.Checks = append(r.Checks, pass("persist_browser",
			fmt.Sprintf("the browser tool is usable after reboot (%d elements from %s)", len(br.Elements), br.URL)))
	case api.CodeBrowserUnavailable:
		// No browser is running on a freshly rebooted appliance, and starting
		// one is not this check's job. Reaching the resolve path proves the tool
		// is wired end to end after the reboot, which is what is being asserted.
		r.Checks = append(r.Checks, pass("persist_browser",
			"the browser tool is reachable after reboot and reports no browser window open"))
	default:
		r.Checks = append(r.Checks, failAssert("persist_browser",
			fmt.Sprintf("browser reported %s after reboot", brMeta.Code)))
	}
	return r
}

// parsePasswdLocked parses `passwd -S <user>` output. The second
// whitespace-separated field is the password status flag: "L"/"LK" (locked),
// "P"/"PS" (usable password), or "NP" (no password). It returns
// (locked, parsed): parsed is false when no status line could be read, so a
// caller never mistakes an unparseable transcript for an unlocked account.
func parsePasswdLocked(stdout string) (locked, parsed bool) {
	for _, ln := range strings.Split(stdout, "\n") {
		f := strings.Fields(ln)
		if len(f) < 2 {
			continue
		}
		switch f[1] {
		case "L", "LK":
			return true, true
		case "P", "PS", "NP":
			return false, true
		}
	}
	return false, false
}
