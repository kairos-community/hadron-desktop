package smoke

import (
	"context"
	"fmt"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// ExecResult is the outcome of one exec-mode run.
type ExecResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// RunExec executes a single shell command through the public `terminal` tool
// and returns what the appliance reported.
//
// This exists for the recovery gate, which has to inject failures into a
// running appliance -- kill the Cua driver, kill a broker, exit the graphical
// session. Driving those through the public tool surface, with the same bearer
// classes an operator would use, keeps the gate honest: it exercises the same
// path a real client takes instead of reaching around the contract via a side
// channel such as SSH or the serial console.
//
// The command line is deliberately NOT recorded in the artifact. Everything
// else here is redacted, and a recovery command is operator-supplied text that
// could name paths or hosts; the check name and outcome are enough to audit.
func (s *Suite) RunExec(ctx context.Context, command string, admin bool, timeout time.Duration) (Report, ExecResult) {
	class := "user"
	bearer := s.UserBearer
	if admin {
		class = "admin"
		bearer = s.AdminBearer
	}
	name := "exec_" + class
	r := Report{Mode: "exec"}

	sess, err := s.Client.Connect(ctx, bearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport(name, "could not open the "+class+" MCP session"))
		return r, ExecResult{ExitCode: -1}
	}
	defer sess.Close()

	ms := int(timeout / time.Millisecond)
	in := api.TerminalInput{Command: command, TimeoutMs: &ms}

	var out api.TerminalOutput
	if err := s.callInto(ctx, sess, api.ToolTerminal, in, &out); err != nil {
		r.Checks = append(r.Checks, failTransport(name, "terminal call failed"))
		return r, ExecResult{ExitCode: -1}
	}
	if out.Code != "" {
		r.Checks = append(r.Checks, failAssert(name, fmt.Sprintf("terminal reported %s", out.Code)))
		return r, ExecResult{ExitCode: -1}
	}

	res := ExecResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode, Truncated: out.Truncated}
	r.Checks = append(r.Checks, pass(name,
		fmt.Sprintf("command ran as %s and exited %d", class, out.ExitCode)))
	return r, res
}
