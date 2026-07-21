package smoke

import (
	"context"
	"fmt"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// RunWarm brings the lazily-started computer-use backend up.
//
// Nothing starts Cua until the first computer_use call, so a gate that wants to
// kill the driver, or to watch it reconnect, has to make one first. A bash call
// will not do it -- that was a real defect in the recovery gate, which "warmed"
// the backend with a shell command and then reported that cua-driver was not
// running.
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
