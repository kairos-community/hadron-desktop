package smoke

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// RunUpload copies a local file into the guest through the public write_file
// tool and verifies it arrived intact.
//
// The UI gate needs a fixture binary inside the appliance, and the honest way
// to put it there is the same tool a client would use -- not a shared folder,
// not SSH, not a rebuilt image. That also makes the upload path itself part of
// what the gate exercises.
//
// The content is sent base64-encoded because the payload is a binary tar; the
// checksum is verified in the guest afterwards, so a truncated or re-encoded
// transfer fails here rather than surfacing later as a mysteriously broken
// fixture.
func (s *Suite) RunUpload(ctx context.Context, localPath, remotePath, wantSHA256 string) Report {
	const name = "upload_fixture"
	r := Report{Mode: "upload"}

	data, err := os.ReadFile(localPath)
	if err != nil {
		r.Checks = append(r.Checks, failAssert(name, "could not read the local fixture"))
		return r
	}

	sess, err := s.Client.Connect(ctx, s.UserBearer)
	if err != nil {
		r.Checks = append(r.Checks, failTransport(name, "could not open the user MCP session"))
		return r
	}
	defer sess.Close()

	in := api.WriteFileInput{
		Path:          remotePath,
		Content:       base64.StdEncoding.EncodeToString(data),
		Encoding:      api.EncodingBase64,
		CreateParents: true,
	}
	var out api.WriteFileOutput
	if err := s.callInto(ctx, sess, api.ToolWriteFile, in, &out); err != nil {
		r.Checks = append(r.Checks, failTransport(name, "write_file call failed"))
		return r
	}
	if out.Code != "" {
		r.Checks = append(r.Checks, failAssert(name, fmt.Sprintf("write_file reported %s", out.Code)))
		return r
	}

	// Verify in the guest, not from the bytes we just sent: the point is to
	// prove what LANDED matches, which a local re-hash could never show.
	var check api.TerminalOutput
	cmd := fmt.Sprintf("sha256sum %q | awk '{print $1}'", remotePath)
	if err := s.callInto(ctx, sess, api.ToolTerminal, api.TerminalInput{Command: cmd}, &check); err != nil {
		r.Checks = append(r.Checks, failTransport(name, "could not checksum the uploaded file"))
		return r
	}
	got := trimSpace(check.Stdout)
	if wantSHA256 != "" && got != wantSHA256 {
		r.Checks = append(r.Checks, failAssert(name,
			fmt.Sprintf("uploaded checksum %s does not match the expected %s", short(got), short(wantSHA256))))
		return r
	}

	r.Checks = append(r.Checks, pass(name,
		fmt.Sprintf("uploaded %d bytes to the guest, checksum %s", len(data), short(got))))
	return r
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\n' || s[start] == '\r' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\n' || s[end-1] == '\r' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

// short truncates a digest for a human-readable detail line. The full value is
// never interesting in a report and a truncated one keeps the line scannable.
func short(digest string) string {
	if len(digest) <= 12 {
		return digest
	}
	return digest[:12]
}
