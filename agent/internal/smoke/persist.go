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
