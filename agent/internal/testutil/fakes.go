// Package testutil provides hermetic fakes for the four Phase-2 executors
// (filesystem, process/terminal, the Cua computer-use backend, and OS
// identity) so the session broker, the root helper, and the end-to-end MCP
// contract can be exercised without a real cua-driver, real process spawning,
// or real root. The fakes are deliberately structural: their method sets match
// both package session's and package roothelper's injected interfaces, so one
// fake type satisfies the same role in either broker.
package testutil

import (
	"context"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// ---------------------------------------------------------------------------
// FakeFiles
// ---------------------------------------------------------------------------

// FakeFiles is a no-op filesystem executor. Every operation succeeds with an
// empty result (no ResultMeta.Code), which the broker treats as a served
// result and stamps with the execution identity. It satisfies both
// session.FileService and roothelper.FileService.
type FakeFiles struct{}

func (FakeFiles) Read(context.Context, api.ReadFileInput) api.ReadFileOutput {
	return api.ReadFileOutput{Content: "", Size: 0}
}

func (FakeFiles) Search(context.Context, api.SearchFilesInput) api.SearchFilesOutput {
	return api.SearchFilesOutput{}
}

func (FakeFiles) Write(context.Context, api.WriteFileInput) api.WriteFileOutput {
	return api.WriteFileOutput{}
}

func (FakeFiles) Patch(context.Context, api.PatchInput) api.PatchOutput {
	return api.PatchOutput{}
}

// ---------------------------------------------------------------------------
// FakeProcess
// ---------------------------------------------------------------------------

// FakeProcess is a process/terminal executor whose calls can be made to block,
// so a test can hold a call "active" across a pause. It satisfies both
// session.ProcessManager and roothelper.ProcessManager.
//
// When Hold is non-nil, Terminal and a ProcessStart block until Hold is closed
// or the call's context is cancelled; Entered (if non-nil) receives one value
// as each such call begins, so a test can synchronize on the call being active.
// When Hold is nil the calls return immediately.
type FakeProcess struct {
	// Entered, if non-nil, receives one value at the start of every blocking
	// call (Terminal and ProcessStart) before it waits on Hold.
	Entered chan struct{}
	// Hold, if non-nil, blocks every Terminal / ProcessStart until it is
	// closed or the call's context is cancelled.
	Hold chan struct{}
}

func (f *FakeProcess) block(ctx context.Context) (interrupted bool) {
	if f.Entered != nil {
		select {
		case f.Entered <- struct{}{}:
		case <-ctx.Done():
			return true
		}
	}
	if f.Hold == nil {
		return false
	}
	select {
	case <-f.Hold:
		return false
	case <-ctx.Done():
		return true
	}
}

func (f *FakeProcess) Bash(ctx context.Context, in api.BashInput) (api.BashOutput, error) {
	if f.block(ctx) {
		// Interrupted mid-call: report the interrupt code the real executors
		// use so the broker's finish() can translate a pause into PAUSED.
		return api.BashOutput{ResultMeta: api.ResultMeta{Code: api.CodeDeadlineExceeded}}, nil
	}
	return api.BashOutput{Stdout: "ok", ExitCode: 0}, nil
}

func (f *FakeProcess) Close() error { return nil }

// ---------------------------------------------------------------------------
// FakeComputer
// ---------------------------------------------------------------------------

// FakeComputer is a Cua computer-use backend that is always available and
// returns an empty (successful) result. It satisfies session.Computer.
type FakeComputer struct {
	// Unavailable, when true, makes ComputerUse degrade to SESSION_UNAVAILABLE
	// and Ready report false, mimicking a desktop session that is not reachable.
	Unavailable bool
}

func (c *FakeComputer) ComputerUse(context.Context, api.ComputerUseInput) (api.ComputerUseOutput, error) {
	if c.Unavailable {
		return api.ComputerUseOutput{ResultMeta: api.ResultMeta{
			Code:      api.CodeSessionUnavailable,
			Message:   "desktop session unavailable",
			Retryable: true,
		}}, nil
	}
	return api.ComputerUseOutput{}, nil
}

func (c *FakeComputer) Browser(context.Context, api.BrowserInput) (api.BrowserOutput, error) {
	if c.Unavailable {
		return api.BrowserOutput{ResultMeta: api.ResultMeta{
			Code:      api.CodeSessionUnavailable,
			Message:   "desktop session unavailable",
			Retryable: true,
		}}, nil
	}
	return api.BrowserOutput{}, nil
}

func (c *FakeComputer) Ready() bool  { return !c.Unavailable }
func (c *FakeComputer) Stop()        {}
func (c *FakeComputer) Close() error { return nil }

// ---------------------------------------------------------------------------
// FakeIdentity
// ---------------------------------------------------------------------------

// FakeIdentity reports a fixed unprivileged identity. It satisfies
// session.IdentityProvider.
type FakeIdentity struct {
	ID api.Identity
}

func (f FakeIdentity) Identity() api.Identity { return f.ID }
