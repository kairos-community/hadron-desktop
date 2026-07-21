package smoke

import (
	"context"
	"fmt"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// ExecResult is the outcome of one exec-mode run.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RunExec executes a single shell script through the public `bash` tool
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

	// bash takes no timeout: it runs to completion. The caller's own budget
	// is applied by the context.
	in := api.BashInput{Command: command}

	// SESSION_UNAVAILABLE is advertised as retryable, and a gate that fires
	// seconds after /readyz turns 200 can genuinely race the session broker
	// settling. Not retrying it asserted a stricter contract than the appliance
	// publishes -- and cost a UI gate run to a broker that was merely still
	// coming up.
	var out api.BashOutput
	var callErr error
	meta := s.retryWhileUnavailable(ctx, func() api.ResultMeta {
		out = api.BashOutput{}
		callErr = s.callInto(ctx, sess, api.ToolBash, in, &out)
		if callErr != nil {
			return api.ResultMeta{}
		}
		return out.ResultMeta
	})
	if callErr != nil {
		r.Checks = append(r.Checks, failTransport(name, "bash call failed"))
		return r, ExecResult{ExitCode: -1}
	}
	if meta.Code != "" {
		r.Checks = append(r.Checks, failAssert(name, fmt.Sprintf("bash reported %s", meta.Code)))
		return r, ExecResult{ExitCode: -1}
	}

	res := ExecResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode}
	r.Checks = append(r.Checks, pass(name,
		fmt.Sprintf("command ran as %s and exited %d", class, out.ExitCode)))
	return r, res
}
