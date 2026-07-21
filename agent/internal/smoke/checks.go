package smoke

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// e2eDir is the writable working directory the file round-trip check exercises,
// owned by the unprivileged agent user on the live appliance.
const e2eDir = "/home/agent/e2e"

// cgroupLeafMarker is the substring every tracked process's own cgroup path
// must contain: package process names each leaf "hadron-proc-<id>", so its
// presence in a process's /proc/self/cgroup proves the process runs in its
// dedicated MCP cgroup leaf (the authoritative kill boundary).
const cgroupLeafMarker = "hadron-proc-"

// privilegedGroups are the supplementary groups the unprivileged agent must NOT
// belong to; membership in any would breach the privilege boundary.
var privilegedGroups = []string{"sudo", "admin", "docker"}

// CheckResult is one assertion's outcome. Detail is always a redacted,
// operator-safe summary: it never contains a bearer, a command line, or file
// contents. Transport marks a failure caused by transport/readiness rather than
// a violated assertion, so the caller can map it to the right exit code.
type CheckResult struct {
	Name      string `json:"name"`
	Passed    bool   `json:"passed"`
	Transport bool   `json:"transport,omitempty"`
	Detail    string `json:"detail"`
}

func pass(name, detail string) CheckResult {
	return CheckResult{Name: name, Passed: true, Detail: detail}
}

func failAssert(name, detail string) CheckResult {
	return CheckResult{Name: name, Passed: false, Detail: detail}
}

func failTransport(name, detail string) CheckResult {
	return CheckResult{Name: name, Passed: false, Transport: true, Detail: detail}
}

// Suite runs the contract-mode assertions against one gateway using a user and
// an admin bearer. It is the single body of assertion logic shared by the live
// binary and the in-process unit test.
type Suite struct {
	Client      *Client
	UserBearer  string
	AdminBearer string
	CallTimeout time.Duration

	// WarmupTimeout bounds how long a retryable SESSION_UNAVAILABLE is
	// tolerated while the lazily-started computer-use backend comes up. Zero
	// means the default.
	WarmupTimeout time.Duration
}

func (s *Suite) callTimeout() time.Duration {
	if s.CallTimeout <= 0 {
		return 30 * time.Second
	}
	return s.CallTimeout
}

// Report is the outcome of a suite run.
type Report struct {
	Mode   string        `json:"mode"`
	Checks []CheckResult `json:"checks"`
}

// Outcome collapses the per-check results into a single verdict, favoring the
// most severe: a transport failure (exit 3) over an assertion failure (exit 1)
// over success (exit 0).
func (r Report) Outcome() int {
	transport := false
	assertion := false
	for _, c := range r.Checks {
		if c.Passed {
			continue
		}
		if c.Transport {
			transport = true
		} else {
			assertion = true
		}
	}
	switch {
	case transport:
		return ExitTransport
	case assertion:
		return ExitAssertion
	default:
		return ExitPass
	}
}

// RunContract opens the user and admin sessions and runs every contract-mode
// assertion. A failure to open either session is a transport error that aborts
// the remaining, session-dependent checks.
func (s *Suite) RunContract(ctx context.Context) Report {
	r := Report{Mode: "contract"}

	userSess, err := s.Client.Connect(ctx, s.UserBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_user", "could not open the user MCP session"))
		return r
	}
	defer userSess.Close()

	adminSess, err := s.Client.Connect(ctx, s.AdminBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport("connect_admin", "could not open the admin MCP session"))
		return r
	}
	defer adminSess.Close()

	r.Checks = append(r.Checks,
		s.checkSevenTools(ctx, userSess, adminSess),
		s.checkUserUnprivileged(ctx, userSess),
		s.checkBashCgroupContainment(ctx, userSess),
		s.checkFilesRoundTrip(ctx, userSess),
		s.checkComputerUseCapture(ctx, userSess),
		s.checkBrowserReachable(ctx, userSess),
		s.checkRootReadDenied(ctx, userSess),
		s.checkDockerSocketDenied(ctx, userSess),
		s.checkAdminIdentityRoot(ctx, adminSess),
		s.checkInvalidAdminRejected(ctx),
	)
	return r
}

// ---------------------------------------------------------------------------
// Checks
// ---------------------------------------------------------------------------

// checkSevenTools proves both credential classes list exactly the seven public
// tool names and that the two lists are identical (brief items 1 and 10). The
// eighth, browser, was added by the 2026-07-20 amendment.
func (s *Suite) checkSevenTools(ctx context.Context, userSess, adminSess *mcp.ClientSession) CheckResult {
	const name = "seven_tools_both_classes"
	userNames, err := listToolNames(ctx, userSess, s.callTimeout())
	if err != nil {
		return failTransport(name, "listing user tools failed")
	}
	adminNames, err := listToolNames(ctx, adminSess, s.callTimeout())
	if err != nil {
		return failTransport(name, "listing admin tools failed")
	}
	want := api.ToolNames()
	if !equalStrings(userNames, want) {
		return failAssert(name, fmt.Sprintf("user tool set is not the seven public tools (got %d)", len(userNames)))
	}
	if !equalStrings(adminNames, want) {
		return failAssert(name, fmt.Sprintf("admin tool set is not the seven public tools (got %d)", len(adminNames)))
	}
	return pass(name, "both classes list exactly the seven public tools")
}

// retryWhileUnavailable runs attempt until it reports something other than a
// retryable SESSION_UNAVAILABLE, or the budget runs out.
//
// The Cua backend starts LAZILY: the first computer_use or browser call is what
// brings it up, so a suite that fires immediately after /readyz turns 200 can
// legitimately race its startup. The contract already says what to do about
// that -- SESSION_UNAVAILABLE is returned with Retryable set -- and a gate that
// ignored its own advertised retry semantics would be asserting a stricter
// contract than the one the appliance publishes.
//
// This is deliberately NOT a blanket retry: anything else, including a
// non-retryable SESSION_UNAVAILABLE, is returned on the first try so a genuinely
// dead session still fails fast.
func (s *Suite) retryWhileUnavailable(ctx context.Context, attempt func() api.ResultMeta) api.ResultMeta {
	deadline := time.Now().Add(s.warmupBudget())
	for {
		meta := attempt()
		if meta.Code != api.CodeSessionUnavailable || !meta.Retryable {
			return meta
		}
		if time.Now().After(deadline) {
			return meta
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return meta
		}
	}
}

// warmupBudget bounds how long the lazily-started backend is given.
func (s *Suite) warmupBudget() time.Duration {
	if s.WarmupTimeout > 0 {
		return s.WarmupTimeout
	}
	return 60 * time.Second
}

// checkUserUnprivileged proves the user's terminal runs as a non-root account
// with none of the privileged supplementary groups (brief items 2 and 8: the
// ordinary bearer never reaches the root path).
func (s *Suite) checkUserUnprivileged(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "user_unprivileged_identity"
	out, res := s.terminalID(ctx, sess)
	if res != nil {
		return res.named(name)
	}
	if out.uid == 0 || out.user == "root" {
		return failAssert(name, fmt.Sprintf("user terminal ran as uid=%d user=%s, want non-root", out.uid, out.user))
	}
	for _, g := range privilegedGroups {
		if containsString(out.groups, g) {
			return failAssert(name, fmt.Sprintf("agent unexpectedly belongs to privileged group %q", g))
		}
	}
	return pass(name, fmt.Sprintf("uid=%d user=%s without admin/sudo/docker groups", out.uid, out.user))
}

// checkBashCgroupContainment proves that a bash call runs in its own cgroup
// leaf and that nothing it spawned outlives it.
//
// This is the containment guarantee the emergency pause depends on: the leaf is
// the kill boundary, so a detached grandchild -- one that called setsid
// precisely to escape its process group -- must still die when the call ends.
// A process group alone would not catch it.
//
// It replaces the old PTY/process-tool version. With `process` gone there is no
// long-lived handle to drive over stdin, so the whole thing is one script: the
// script records its own cgroup and spawns the escapee, and a second call
// checks the escapee is dead. That is a stronger shape anyway -- it asserts the
// state AFTER the call returned, which is exactly when teardown must have run.
func (s *Suite) checkBashCgroupContainment(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "bash_cgroup_containment"

	script := `cat /proc/self/cgroup
setsid sh -c 'sleep 300' </dev/null >/dev/null 2>&1 &
echo GC=$!`

	var run api.BashOutput
	if err := s.callInto(ctx, sess, api.ToolBash, api.BashInput{Command: script}, &run); err != nil {
		return failTransport(name, "bash call failed")
	}
	if run.Code != "" {
		return failAssert(name, fmt.Sprintf("bash reported %s", run.Code))
	}
	if !strings.Contains(run.Stdout, cgroupLeafMarker) {
		return failAssert(name, fmt.Sprintf(
			"the command did not run in its dedicated MCP cgroup leaf (saw %q)", lastLine(run.Stdout)))
	}
	gcPID := parseGrandchildPID(run.Stdout)
	if gcPID == "" {
		return failAssert(name, "could not observe the spawned grandchild pid")
	}

	// The call has returned, so teardown has already run. A detached grandchild
	// still alive here means the leaf did not hold.
	var check api.BashOutput
	probe := "kill -0 " + gcPID + " 2>/dev/null && echo ALIVE || echo GONE"
	if err := s.callInto(ctx, sess, api.ToolBash, api.BashInput{Command: probe}, &check); err != nil {
		return failTransport(name, "grandchild liveness probe failed")
	}
	if !strings.Contains(check.Stdout, "GONE") || strings.Contains(check.Stdout, "ALIVE") {
		return failAssert(name, "a detached grandchild survived the bash call")
	}
	return pass(name, "the command ran in its MCP cgroup leaf and left no surviving grandchild")
}

// checkFilesRoundTrip writes, reads, searches, and structured-patches a file
// under the agent's e2e directory (brief item 4).
func (s *Suite) checkFilesRoundTrip(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "files_round_trip"
	const marker = "hadron-e2e-probe"
	file := e2eDir + "/probe.txt"

	var wr api.WriteFileOutput
	if err := s.callInto(ctx, sess, api.ToolWriteFile, api.WriteFileInput{
		Path: file, Content: marker, CreateParents: true,
	}, &wr); err != nil {
		return failTransport(name, "write_file call failed")
	}
	if wr.Code != "" {
		return failAssert(name, fmt.Sprintf("write_file reported %s", wr.Code))
	}

	var rd api.ReadFileOutput
	if err := s.callInto(ctx, sess, api.ToolReadFile, api.ReadFileInput{Path: file}, &rd); err != nil {
		return failTransport(name, "read_file call failed")
	}
	if rd.Code != "" || rd.Content != marker {
		return failAssert(name, "read_file did not return the written content")
	}

	var sr api.SearchFilesOutput
	if err := s.callInto(ctx, sess, api.ToolSearchFiles, api.SearchFilesInput{
		Path: e2eDir, Query: marker, Mode: api.SearchByContent,
	}, &sr); err != nil {
		return failTransport(name, "search_files call failed")
	}
	if sr.Code != "" || len(sr.Matches) == 0 {
		return failAssert(name, "search_files did not find the written marker")
	}

	var pt api.PatchOutput
	if err := s.callInto(ctx, sess, api.ToolPatch, api.PatchInput{
		Files: []api.PatchFile{{Path: file, Replacements: []api.PatchReplacement{{Old: marker, New: "patched"}}}},
	}, &pt); err != nil {
		return failTransport(name, "patch call failed")
	}
	if pt.Code != "" || pt.ReplacementsApplied < 1 {
		return failAssert(name, "patch did not apply the replacement")
	}
	return pass(name, "write, read, search, and patch all succeeded under the agent e2e dir")
}

// checkComputerUseCapture proves computer_use can capture the desktop (brief
// item 5).
func (s *Suite) checkComputerUseCapture(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "computer_use_capture"
	var out api.ComputerUseOutput
	var callErr error
	meta := s.retryWhileUnavailable(ctx, func() api.ResultMeta {
		out = api.ComputerUseOutput{}
		callErr = s.callInto(ctx, sess, api.ToolComputerUse, api.ComputerUseInput{Action: api.ActionCapture}, &out)
		if callErr != nil {
			return api.ResultMeta{}
		}
		return out.ResultMeta
	})
	if callErr != nil {
		return failTransport(name, "computer_use call failed")
	}
	if meta.Code != "" {
		return failAssert(name, fmt.Sprintf("capture reported %s", meta.Code))
	}
	if out.ImageBase64 == "" {
		return failAssert(name, "capture returned no image")
	}
	return pass(name, fmt.Sprintf("captured a %d-byte desktop image", len(out.ImageBase64)))
}

// checkBrowserReachable proves the browser tool is wired end to end: the call
// reaches the session broker, resolves a window (or reports that there is no
// browser), and comes back with a structured result.
//
// BROWSER_UNAVAILABLE is a PASS here on purpose. The appliance does not ship a
// running browser, so demanding a live page would make this check assert the
// fixture's contents rather than the tool's plumbing -- and it is exactly the
// code that proves the resolve path ran. A transport failure, an unknown tool,
// or any other code is still a failure.
func (s *Suite) checkBrowserReachable(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "browser_reachable"
	var out api.BrowserOutput
	var callErr error
	meta := s.retryWhileUnavailable(ctx, func() api.ResultMeta {
		out = api.BrowserOutput{}
		callErr = s.callInto(ctx, sess, api.ToolBrowser, api.BrowserInput{Action: api.BrowserSnapshot}, &out)
		if callErr != nil {
			return api.ResultMeta{}
		}
		return out.ResultMeta
	})
	if callErr != nil {
		return failTransport(name, "browser call failed")
	}
	out.ResultMeta = meta
	switch out.Code {
	case "":
		return pass(name, fmt.Sprintf("snapshot returned %d elements from %s", len(out.Elements), out.URL))
	case api.CodeBrowserUnavailable:
		return pass(name, "browser tool is reachable and reports no browser window is open")
	default:
		return failAssert(name, fmt.Sprintf("browser snapshot reported %s", out.Code))
	}
}

// rootOnlyProbeFile is the read target for checkRootReadDenied. Unlike
// /root/.bashrc (which may or may not exist), /etc/shadow is guaranteed to be
// present on every Linux host and is always root-only (mode 0640/0600), so a
// FORBIDDEN result on THIS path is a genuine proof of the deny boundary.
const rootOnlyProbeFile = "/etc/shadow"

// checkRootReadDenied proves the ordinary bearer cannot read a root-only file
// (brief item 6). The check requires the SPECIFIC api.CodeForbidden outcome:
// success (readable) fails the assertion, and so does any other code
// (NOT_FOUND included) — a missing target does not prove the boundary, it
// just means the probe couldn't test it, so the check must not GREEN on a
// vacuous "absent" reading. See TestRootReadDenyNotFoundDoesNotPass for the
// non-vacuity proof.
func (s *Suite) checkRootReadDenied(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "root_read_denied"
	var rd api.ReadFileOutput
	if err := s.callInto(ctx, sess, api.ToolReadFile, api.ReadFileInput{Path: rootOnlyProbeFile}, &rd); err != nil {
		return failTransport(name, "read_file call failed")
	}
	switch rd.Code {
	case api.CodeForbidden:
		return pass(name, "user read of "+rootOnlyProbeFile+" denied (FORBIDDEN)")
	case "":
		return failAssert(name, "user read of "+rootOnlyProbeFile+" unexpectedly succeeded")
	default:
		return failAssert(name, fmt.Sprintf("user read of %s reported %s (want FORBIDDEN); cannot prove the deny boundary", rootOnlyProbeFile, rd.Code))
	}
}

// checkDockerSocketDenied proves the ordinary bearer cannot access the Docker
// control socket (brief item 7). Phase-3 mounts /run/docker.sock into the
// desktop, so on a correctly configured appliance the socket is PRESENT and
// access is DENIED — that is the strong, expected pass. ABSENT is treated as
// a distinct, visible outcome rather than folded into DENIED: the security
// property still holds (there's nothing to access), so the run does not fail,
// but a socket that has disappeared is mount-drift the operator must be able
// to see in the recorded Detail, never a silent, indistinguishable pass. See
// TestDockerSocketAbsentIsFlaggedNotConflatedWithDenied.
func (s *Suite) checkDockerSocketDenied(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "docker_socket_denied"
	cmd := "if [ -e /run/docker.sock ]; then if [ -r /run/docker.sock ]; then echo OPEN; else echo DENIED; fi; else echo ABSENT; fi"
	var out api.BashOutput
	if err := s.callInto(ctx, sess, api.ToolBash, api.BashInput{Command: cmd}, &out); err != nil {
		return failTransport(name, "terminal call failed")
	}
	if out.Code != "" {
		return failAssert(name, fmt.Sprintf("docker socket probe reported %s", out.Code))
	}
	got := strings.TrimSpace(out.Stdout)
	switch {
	case strings.Contains(got, "OPEN"):
		return failAssert(name, "the Docker socket is accessible to the agent")
	case strings.Contains(got, "DENIED"):
		return pass(name, "the Docker control socket is present and access is denied to the agent")
	case strings.Contains(got, "ABSENT"):
		return pass(name, "FLAGGED: docker.sock is absent (expected present+denied; possible mount drift, not a proven deny)")
	default:
		return failAssert(name, "unexpected docker socket probe result")
	}
}

// checkAdminIdentityRoot proves an admin OS tool executes as root (brief item
// 9, first half).
func (s *Suite) checkAdminIdentityRoot(ctx context.Context, sess *mcp.ClientSession) CheckResult {
	const name = "admin_identity_root"
	out, res := s.terminalID(ctx, sess)
	if res != nil {
		return res.named(name)
	}
	if out.uid != 0 || out.user != "root" {
		return failAssert(name, fmt.Sprintf("admin terminal ran as uid=%d user=%s, want root", out.uid, out.user))
	}
	return pass(name, "admin terminal executes as root via the privileged path")
}

// checkInvalidAdminRejected proves a malformed admin bearer is rejected (brief
// item 9, second half). A rejection at connect OR at call time both count.
func (s *Suite) checkInvalidAdminRejected(ctx context.Context) CheckResult {
	const name = "invalid_admin_rejected"
	bad := mutateBearer(s.AdminBearer)

	sess, err := s.Client.Connect(ctx, bad)
	if err != nil {
		return pass(name, "invalid admin bearer rejected at connect")
	}
	defer sess.Close()

	var out api.BashOutput
	if err := s.callInto(ctx, sess, api.ToolBash, api.BashInput{Command: "true"}, &out); err != nil {
		return pass(name, "invalid admin bearer rejected at call")
	}
	if out.Code == api.CodeUnauthenticated {
		return pass(name, "invalid admin bearer rejected as UNAUTHENTICATED")
	}
	return failAssert(name, "an invalid admin bearer was accepted")
}

// ---------------------------------------------------------------------------
type idResult struct {
	uid    int
	user   string
	groups []string
}

// terminalFail wraps a helper failure so a check can rename it to its own check
// name while preserving the transport/assertion classification.
type terminalFail struct {
	transport bool
	detail    string
}

func (f *terminalFail) named(name string) CheckResult {
	if f.transport {
		return failTransport(name, f.detail)
	}
	return failAssert(name, f.detail)
}

// terminalID runs `id -u; id -un; id -nG` and parses the three lines.
// callInto performs one tool call and decodes its structured result.
// callInto performs one tool call and decodes its structured result.
//
// A RETRYABLE SESSION_UNAVAILABLE is retried within a bounded budget rather
// than surfaced. The appliance advertises that code as retryable and means it:
// the session broker and the lazily-started computer-use backend both settle
// after boot, and a gate firing seconds behind /readyz can legitimately arrive
// first. Treating it as fatal made the suite assert a stricter contract than
// the appliance publishes -- and produced install-gate failures on user-class
// bash and file calls while every other check on the same machine passed.
//
// Only that one code is retried, and only when the appliance marks it
// retryable, so a genuinely dead session still fails fast.
func (s *Suite) callInto(ctx context.Context, sess *mcp.ClientSession, tool string, args any, out any) error {
	deadline := time.Now().Add(s.warmupBudget())
	for {
		data, err := s.callRaw(ctx, sess, tool, args)
		if err != nil {
			return err
		}
		var meta struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		}
		_ = json.Unmarshal(data, &meta)
		if meta.Code != string(api.CodeSessionUnavailable) || !meta.Retryable || time.Now().After(deadline) {
			return json.Unmarshal(data, out)
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return json.Unmarshal(data, out)
		}
	}
}

// callRaw performs one tool call and returns its structured result as JSON.
func (s *Suite) callRaw(ctx context.Context, sess *mcp.ClientSession, tool string, args any) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, s.callTimeout())
	defer cancel()
	res, err := sess.CallTool(cctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.StructuredContent == nil {
		return nil, errors.New("tool result carried no structured content")
	}
	return json.Marshal(res.StructuredContent)
}

func (s *Suite) terminalID(ctx context.Context, sess *mcp.ClientSession) (idResult, *terminalFail) {
	var out api.BashOutput
	if err := s.callInto(ctx, sess, api.ToolBash, api.BashInput{Command: "id -u; id -un; id -nG"}, &out); err != nil {
		return idResult{}, &terminalFail{transport: true, detail: "terminal id call failed"}
	}
	if out.Code != "" {
		return idResult{}, &terminalFail{detail: fmt.Sprintf("terminal id reported %s", out.Code)}
	}
	id, ok := parseID(out.Stdout)
	if !ok {
		return idResult{}, &terminalFail{detail: "could not parse id output"}
	}
	return id, nil
}

// ---------------------------------------------------------------------------
// Parsing / pure helpers
// ---------------------------------------------------------------------------

// parseID parses the three lines produced by `id -u; id -un; id -nG`.
func parseID(stdout string) (idResult, bool) {
	var lines []string
	for _, ln := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			lines = append(lines, t)
		}
	}
	if len(lines) < 3 {
		return idResult{}, false
	}
	uid, err := strconv.Atoi(lines[0])
	if err != nil {
		return idResult{}, false
	}
	return idResult{uid: uid, user: lines[1], groups: strings.Fields(lines[2])}, true
}

// parseGrandchildPID extracts the PID printed as "GC=<pid>".
func parseGrandchildPID(stdout string) string {
	for _, ln := range strings.Split(stdout, "\n") {
		ln = strings.TrimSpace(ln)
		if v, ok := strings.CutPrefix(ln, "GC="); ok {
			v = strings.TrimSpace(v)
			if _, err := strconv.Atoi(v); err == nil {
				return v
			}
		}
	}
	return ""
}

// mutateBearer returns a well-formed-looking but altered copy of a bearer so a
// rejection reflects the digest, not a parse error. The returned value is a
// bearer and must never be logged.
func mutateBearer(b string) string {
	if b == "" {
		return "hdn_a_0"
	}
	last := b[len(b)-1]
	repl := byte('a')
	if last == 'a' {
		repl = 'b'
	}
	return b[:len(b)-1] + string(repl)
}

func listToolNames(ctx context.Context, sess *mcp.ClientSession, timeout time.Duration) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := sess.ListTools(cctx, nil)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, t := range res.Tools {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func codeOrNone(c api.ErrorCode) string {
	if c == "" {
		return "none"
	}
	return string(c)
}

// lastLine returns the final non-empty line of s, for error details that would
// otherwise quote a whole terminal transcript.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
