package smoke

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// RunLongCommand executes a command through the public `process` tool, polling
// until it exits, and returns its accumulated output.
//
// This exists because the gateway bounds every single request (see
// gateway.DefaultRequestTimeout, 60s). A multi-minute job -- installing an
// application from Flathub is the motivating case -- can therefore never be run
// through `terminal`, no matter how generous the client's own timeout is: the
// gateway cuts the request long before the command finishes, and the caller
// gets an empty result that looks like the command produced no output.
//
// `process` is the tool that models this correctly: start returns immediately
// with a handle, and each poll is its own short request. That is exactly what a
// real client has to do, so driving the gate this way keeps it honest.
func (s *Suite) RunLongCommand(ctx context.Context, command string, admin bool, budget time.Duration) (Report, ExecResult) {
	class := "user"
	bearer := s.UserBearer
	if admin {
		class = "admin"
		bearer = s.AdminBearer
	}
	name := "run_" + class
	r := Report{Mode: "run"}

	sess, err := s.Client.Connect(ctx, bearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport(name, "could not open the "+class+" MCP session"))
		return r, ExecResult{ExitCode: -1}
	}
	defer sess.Close()

	var start api.ProcessOutput
	if err := s.callInto(ctx, sess, api.ToolProcess, api.ProcessInput{
		Action: api.ProcessStart, Command: "/bin/sh", Args: []string{"-lc", command},
	}, &start); err != nil {
		r.Checks = append(r.Checks, failTransport(name, "process start failed"))
		return r, ExecResult{ExitCode: -1}
	}
	if start.Code != "" || start.ProcessID == "" {
		r.Checks = append(r.Checks, failAssert(name,
			fmt.Sprintf("could not start the process (code=%s)", codeOrNone(start.Code))))
		return r, ExecResult{ExitCode: -1}
	}

	if budget <= 0 {
		budget = 20 * time.Minute
	}
	deadline := time.Now().Add(budget)
	var out strings.Builder
	for {
		var poll api.ProcessOutput
		if err := s.callInto(ctx, sess, api.ToolProcess, api.ProcessInput{
			Action: api.ProcessPoll, ProcessID: start.ProcessID,
		}, &poll); err != nil {
			r.Checks = append(r.Checks, failTransport(name, "process poll failed"))
			return r, ExecResult{Stdout: out.String(), ExitCode: -1}
		}
		out.WriteString(poll.Stdout)
		if !poll.Running {
			// ExitCode is a pointer so an explicit 0 is distinguishable from
			// "the process has not reported one".
			code := 0
			if poll.ExitCode != nil {
				code = *poll.ExitCode
			}
			r.Checks = append(r.Checks, pass(name,
				fmt.Sprintf("command ran as %s and exited %d", class, code)))
			return r, ExecResult{Stdout: out.String(), Stderr: poll.Stderr, ExitCode: code}
		}
		if time.Now().After(deadline) {
			r.Checks = append(r.Checks, failAssert(name,
				fmt.Sprintf("command did not finish within %s", budget)))
			return r, ExecResult{Stdout: out.String(), ExitCode: -1}
		}
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return r, ExecResult{Stdout: out.String(), ExitCode: -1}
		}
	}
}

// RunWarm brings the lazily-started computer-use backend up.
//
// Nothing starts Cua until the first computer_use call, so a gate that wants to
// kill the driver, or to watch it reconnect, has to make one first. A terminal
// call will not do it -- that was a real defect in the recovery gate, which
// "warmed" the backend with a shell command and then reported that cua-driver
// was not running.
func (s *Suite) RunWarm(ctx context.Context) (Report, ExecResult) {
	const name = "warm_computer_use"
	r := Report{Mode: "warm"}

	sess, err := s.Client.Connect(ctx, s.UserBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport(name, "could not open the user MCP session"))
		return r, ExecResult{ExitCode: -1}
	}
	defer sess.Close()

	var out api.ComputerUseOutput
	meta := s.retryWhileUnavailable(ctx, func() api.ResultMeta {
		out = api.ComputerUseOutput{}
		if err := s.callInto(ctx, sess, api.ToolComputerUse,
			api.ComputerUseInput{Action: api.ActionCapture}, &out); err != nil {
			return api.ResultMeta{Code: api.CodeInternal, Message: err.Error()}
		}
		return out.ResultMeta
	})
	if meta.Code != "" {
		r.Checks = append(r.Checks, failAssert(name, fmt.Sprintf("computer_use reported %s", meta.Code)))
		return r, ExecResult{ExitCode: 1}
	}
	r.Checks = append(r.Checks, pass(name, "computer-use backend is up"))
	return r, ExecResult{ExitCode: 0}
}
