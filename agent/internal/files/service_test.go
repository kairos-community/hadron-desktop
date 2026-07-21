package files

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

func newService() *Service { return NewService() }

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

func TestReadFileUTF8(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := newService().Read(context.Background(), api.ReadFileInput{Path: path})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.Encoding != api.EncodingUTF8 {
		t.Fatalf("encoding = %q, want utf8", out.Encoding)
	}
	if out.Content != "hello world" {
		t.Fatalf("content = %q", out.Content)
	}
	if out.Size != 11 {
		t.Fatalf("size = %d, want 11", out.Size)
	}
	if out.Truncated {
		t.Fatalf("truncated = true, want false")
	}
}

func TestReadFileBase64Requested(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	data := []byte{0x00, 0x01, 0xff, 'h', 'i'}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := newService().Read(context.Background(), api.ReadFileInput{Path: path, Encoding: api.EncodingBase64})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.Encoding != api.EncodingBase64 {
		t.Fatalf("encoding = %q, want base64", out.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("decoded = %v, want %v", decoded, data)
	}
}

func TestReadFileInvalidUTF8FallsBackToBase64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	data := []byte{0xff, 0xfe, 0x00, 0x80}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := newService().Read(context.Background(), api.ReadFileInput{Path: path})
	if out.Encoding != api.EncodingBase64 {
		t.Fatalf("encoding = %q, want base64 fallback", out.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Content)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(decoded, data) {
		t.Fatalf("decoded = %v, want %v", decoded, data)
	}
}

func TestReadFileOffsetLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}

	offset := int64(3)
	limit := int64(4)
	out := newService().Read(context.Background(), api.ReadFileInput{Path: path, Offset: &offset, Limit: &limit})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.Content != "3456" {
		t.Fatalf("content = %q, want 3456", out.Content)
	}
	// The caller asked for exactly this slice; running off the end of the
	// file due to their own limit is informational, not a bound violation.
	if !out.Truncated {
		t.Fatalf("truncated = false, want true (more data beyond the requested slice)")
	}
	if out.Code == api.CodeOutputTruncated {
		t.Fatalf("code = OUTPUT_TRUNCATED, want empty for an explicit, honored limit")
	}
}

func TestReadFileMetadataOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("some content"), 0o644); err != nil {
		t.Fatal(err)
	}

	out := newService().Read(context.Background(), api.ReadFileInput{Path: path, MetadataOnly: true})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.Content != "" {
		t.Fatalf("content = %q, want empty for metadata-only", out.Content)
	}
	if out.Size != 12 {
		t.Fatalf("size = %d, want 12", out.Size)
	}
}

func TestReadFileDefaultBoundTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	data := bytes.Repeat([]byte("a"), int(DefaultReadBytes)+10)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	out := newService().Read(context.Background(), api.ReadFileInput{Path: path})
	if out.Code != api.CodeOutputTruncated {
		t.Fatalf("code = %q, want OUTPUT_TRUNCATED", out.Code)
	}
	if !out.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if int64(len(out.Content)) != DefaultReadBytes {
		t.Fatalf("content len = %d, want %d", len(out.Content), DefaultReadBytes)
	}
}

func TestReadFileLimitClampedToMax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	data := bytes.Repeat([]byte("a"), int(MaxReadBytes)+10)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	huge := MaxReadBytes * 2
	out := newService().Read(context.Background(), api.ReadFileInput{Path: path, Limit: &huge})
	if out.Code != api.CodeOutputTruncated {
		t.Fatalf("code = %q, want OUTPUT_TRUNCATED", out.Code)
	}
	if int64(len(out.Content)) != MaxReadBytes {
		t.Fatalf("content len = %d, want %d", len(out.Content), MaxReadBytes)
	}
}

func TestReadFileNotFound(t *testing.T) {
	out := newService().Read(context.Background(), api.ReadFileInput{Path: filepath.Join(t.TempDir(), "nope")})
	if out.Code != api.CodeNotFound {
		t.Fatalf("code = %q, want NOT_FOUND", out.Code)
	}
}

func TestReadFileFollowsFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("through the link"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	out := newService().Read(context.Background(), api.ReadFileInput{Path: link})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.Content != "through the link" {
		t.Fatalf("content = %q", out.Content)
	}
}

// ---------------------------------------------------------------------------
// search_files
// ---------------------------------------------------------------------------

func TestSearchFilesByNameLiteral(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "needle.txt"), "x")
	mustWrite(t, filepath.Join(dir, "other.txt"), "x")
	mustMkdir(t, filepath.Join(dir, "sub"))
	mustWrite(t, filepath.Join(dir, "sub", "needle2.txt"), "x")

	out := newService().Search(context.Background(), api.SearchFilesInput{Path: dir, Query: "needle", Mode: api.SearchByName})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if len(out.Matches) != 2 {
		t.Fatalf("matches = %d, want 2: %+v", len(out.Matches), out.Matches)
	}
}

func TestSearchFilesByContentLiteral(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "line one\nfindme here\nline three\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "nothing interesting\n")

	out := newService().Search(context.Background(), api.SearchFilesInput{Path: dir, Query: "findme", Mode: api.SearchByContent})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %d, want 1: %+v", len(out.Matches), out.Matches)
	}
	if out.Matches[0].Line != 2 {
		t.Fatalf("line = %d, want 2", out.Matches[0].Line)
	}
	if out.Matches[0].Excerpt != "findme here" {
		t.Fatalf("excerpt = %q", out.Matches[0].Excerpt)
	}
}

func TestSearchFilesRegex(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "foo123bar\n")
	mustWrite(t, filepath.Join(dir, "b.txt"), "no digits here\n")

	out := newService().Search(context.Background(), api.SearchFilesInput{
		Path: dir, Query: `\d+`, Mode: api.SearchByContent, Regex: true,
	})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %d, want 1", len(out.Matches))
	}

	out = newService().Search(context.Background(), api.SearchFilesInput{
		Path: dir, Query: `[bad`, Mode: api.SearchByContent, Regex: true,
	})
	if out.Code != api.CodeInvalidArgument {
		t.Fatalf("code = %q, want INVALID_ARGUMENT for bad regex", out.Code)
	}
}

func TestSearchFilesResultTruncation(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < DefaultSearchResults+10; i++ {
		mustWrite(t, filepath.Join(dir, fmt.Sprintf("needle%06d.txt", i)), "x")
	}

	out := newService().Search(context.Background(), api.SearchFilesInput{Path: dir, Query: "needle", Mode: api.SearchByName})
	if out.Code != api.CodeOutputTruncated {
		t.Fatalf("code = %q, want OUTPUT_TRUNCATED", out.Code)
	}
	if !out.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if len(out.Matches) != DefaultSearchResults {
		t.Fatalf("matches = %d, want %d", len(out.Matches), DefaultSearchResults)
	}
}

func TestSearchFilesDoesNotFollowDirectorySymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	mustMkdir(t, real)
	mustWrite(t, filepath.Join(real, "needle.txt"), "x")

	outside := t.TempDir()
	outsideTarget := filepath.Join(outside, "outside_real")
	mustMkdir(t, outsideTarget)
	mustWrite(t, filepath.Join(outsideTarget, "needle_outside.txt"), "x")

	if err := os.Symlink(outsideTarget, filepath.Join(dir, "link_to_outside")); err != nil {
		t.Fatal(err)
	}

	out := newService().Search(context.Background(), api.SearchFilesInput{Path: dir, Query: "needle", Mode: api.SearchByName})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	for _, m := range out.Matches {
		if strings.Contains(m.Path, "needle_outside") {
			t.Fatalf("search followed a directory symlink: matched %s", m.Path)
		}
	}
	if len(out.Matches) != 1 {
		t.Fatalf("matches = %d, want 1 (only the real needle.txt)", len(out.Matches))
	}
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

func TestWriteFileCreatesNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: "hello"})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if !out.Created {
		t.Fatalf("created = false, want true")
	}
	if out.BytesWritten != 5 {
		t.Fatalf("bytes_written = %d, want 5", out.BytesWritten)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}
}

func TestWriteFileAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("original content that is longer"), 0o640); err != nil {
		t.Fatal(err)
	}

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: "new"})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.Created {
		t.Fatalf("created = true, want false (replacement)")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want new", got)
	}

	// No stray temp files should remain in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("dir entries = %d, want 1 (no leftover temp file): %+v", len(entries), entries)
	}
}

func TestWriteFilePreservesModeOnReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(path, []byte("orig"), 0o600); err != nil {
		t.Fatal(err)
	}

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: "new"})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600 preserved", fi.Mode().Perm())
	}
}

func TestWriteFileAppliesRequestedMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "new.txt")

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: "x", Mode: "0600"})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestWriteFileMissingParentRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "f.txt")

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: "x"})
	if out.Code != api.CodeNotFound {
		t.Fatalf("code = %q, want NOT_FOUND", out.Code)
	}
}

func TestWriteFileCreateParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "f.txt")

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: "x", CreateParents: true})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not created: %v", err)
	}
}

func TestWriteFileRejectsFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	out := newService().Write(context.Background(), api.WriteFileInput{Path: link, Content: "clobber"})
	if out.Code != api.CodeAlreadyExists {
		t.Fatalf("code = %q, want ALREADY_EXISTS", out.Code)
	}

	// The link and its target must be untouched.
	lst, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if lst.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link was replaced, no longer a symlink")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("target content = %q, want original (untouched)", got)
	}
}

func TestWriteFileBase64(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	data := []byte{0x00, 0xff, 0x10}

	out := newService().Write(context.Background(), api.WriteFileInput{
		Path: path, Content: base64.StdEncoding.EncodeToString(data), Encoding: api.EncodingBase64,
	})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("content = %v, want %v", got, data)
	}
}

func TestWriteFileExceedsMaxRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.bin")
	big := strings.Repeat("a", int(MaxWriteBytes)+1)

	out := newService().Write(context.Background(), api.WriteFileInput{Path: path, Content: big})
	if out.Code != api.CodeResourceExhausted {
		t.Fatalf("code = %q, want RESOURCE_EXHAUSTED", out.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not have been created")
	}
}

// ---------------------------------------------------------------------------
// patch
// ---------------------------------------------------------------------------

func TestPatchSimpleReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	mustWrite(t, path, "hello world")

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "world", New: "there"}}},
	}})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.ReplacementsApplied != 1 {
		t.Fatalf("replacements_applied = %d, want 1", out.ReplacementsApplied)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != path {
		t.Fatalf("files_changed = %v", out.FilesChanged)
	}
	got := mustRead(t, path)
	if got != "hello there" {
		t.Fatalf("content = %q, want %q", got, "hello there")
	}
}

func TestPatchAbsentOldRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	mustWrite(t, path, "hello world")

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "nonexistent", New: "x"}}},
	}})
	if out.Code != api.CodeNotFound {
		t.Fatalf("code = %q, want NOT_FOUND", out.Code)
	}
	if mustRead(t, path) != "hello world" {
		t.Fatalf("file was modified despite rejected patch")
	}
}

func TestPatchNonUniqueOldRejectedWithoutAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	mustWrite(t, path, "foo foo foo")

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "foo", New: "bar"}}},
	}})
	if out.Code != api.CodeInvalidArgument {
		t.Fatalf("code = %q, want INVALID_ARGUMENT", out.Code)
	}
	if mustRead(t, path) != "foo foo foo" {
		t.Fatalf("file was modified despite rejected patch")
	}
}

func TestPatchAllTrueReplacesEveryOccurrence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	mustWrite(t, path, "foo foo foo")

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "foo", New: "bar", All: true}}},
	}})
	if out.Code != "" {
		t.Fatalf("unexpected error: %+v", out.ResultMeta)
	}
	if out.ReplacementsApplied != 3 {
		t.Fatalf("replacements_applied = %d, want 3", out.ReplacementsApplied)
	}
	if mustRead(t, path) != "bar bar bar" {
		t.Fatalf("content = %q, want bar bar bar", mustRead(t, path))
	}
}

func TestPatchRejectsFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	mustWrite(t, target, "hello world")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: link, Replacements: []api.PatchReplacement{{Old: "world", New: "there"}}},
	}})
	if out.Code != api.CodeAlreadyExists {
		t.Fatalf("code = %q, want ALREADY_EXISTS", out.Code)
	}
	if mustRead(t, target) != "hello world" {
		t.Fatalf("target was modified despite symlink rejection")
	}
}

func TestPatchExceedsMaxReplacementsRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	mustWrite(t, path, strings.Repeat("x", MaxPatchReplacements+50))

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "x", New: "y", All: true}}},
	}})
	if out.Code != api.CodeResourceExhausted {
		t.Fatalf("code = %q, want RESOURCE_EXHAUSTED", out.Code)
	}
	if mustRead(t, path) != strings.Repeat("x", MaxPatchReplacements+50) {
		t.Fatalf("file was modified despite exceeding the replacement cap")
	}
}

func TestPatchExceedsAggregateBytesRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	// One replacement that blows the file up past the aggregate cap.
	original := "START" + strings.Repeat("y", 100) + "END"
	mustWrite(t, path, original)

	big := strings.Repeat("z", int(MaxPatchAggregateBytes)+10)
	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "START", New: big}}},
	}})
	if out.Code != api.CodeResourceExhausted {
		t.Fatalf("code = %q, want RESOURCE_EXHAUSTED", out.Code)
	}
	if mustRead(t, path) != original {
		t.Fatalf("file was modified despite exceeding the aggregate cap")
	}
}

// TestPatchRollsBackOnPartialFailure forces the second file in a multi-file
// patch to fail at write time (by making its directory unwritable) after the
// first file's replacement has already succeeded, and asserts the first
// file is restored to its exact original bytes.
func TestPatchRollsBackOnPartialFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: directory permission bits do not block writes")
	}

	root := t.TempDir()

	okDir := filepath.Join(root, "ok")
	mustMkdir(t, okDir)
	okPath := filepath.Join(okDir, "a.txt")
	mustWrite(t, okPath, "alpha original")

	failDir := filepath.Join(root, "fail")
	mustMkdir(t, failDir)
	failPath := filepath.Join(failDir, "b.txt")
	mustWrite(t, failPath, "beta original")

	// Make failDir unwritable so the atomic replace of failPath cannot even
	// create its temp file, forcing a write failure after okPath has
	// already been replaced.
	if err := os.Chmod(failDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(failDir, 0o755) })

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: okPath, Replacements: []api.PatchReplacement{{Old: "alpha", New: "ALPHA"}}},
		{Path: failPath, Replacements: []api.PatchReplacement{{Old: "beta", New: "BETA"}}},
	}})
	if out.Code == "" {
		t.Fatalf("expected a failure, got success: %+v", out)
	}
	if len(out.FilesChanged) != 0 {
		t.Fatalf("files_changed = %v, want empty on a rolled-back patch", out.FilesChanged)
	}

	if got := mustRead(t, okPath); got != "alpha original" {
		t.Fatalf("okPath = %q, want restored to %q", got, "alpha original")
	}

	// No stray backup files left behind in okDir.
	entries, err := os.ReadDir(okDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("okDir entries = %d, want 1 (no leftover backup): %+v", len(entries), entries)
	}
}

// TestPatchOversizedFileRejectedBeforeRead asserts that a file whose on-disk
// size (per Lstat) already exceeds MaxPatchAggregateBytes is rejected via the
// stat-based guard, without patch ever reading its contents into memory. A
// sparse file created with os.Truncate reports the requested size from Stat
// without allocating real disk blocks for it, so this exercises the guard
// without needing a real multi-GB file.
func TestPatchOversizedFileRejectedBeforeRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(MaxPatchAggregateBytes + 1); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "x", New: "y"}}},
	}})
	if out.Code != api.CodeResourceExhausted {
		t.Fatalf("code = %q, want RESOURCE_EXHAUSTED", out.Code)
	}
}

// TestPatchDuplicatePathRejected asserts that a patch listing the same path
// twice is rejected up front (before any write), rather than silently
// dropping the first entry's edits.
func TestPatchDuplicatePathRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	mustWrite(t, path, "hello world")

	out := newService().Patch(context.Background(), api.PatchInput{Files: []api.PatchFile{
		{Path: path, Replacements: []api.PatchReplacement{{Old: "hello", New: "goodbye"}}},
		{Path: path, Replacements: []api.PatchReplacement{{Old: "world", New: "there"}}},
	}})
	if out.Code != api.CodeInvalidArgument {
		t.Fatalf("code = %q, want INVALID_ARGUMENT", out.Code)
	}
	if mustRead(t, path) != "hello world" {
		t.Fatalf("file was modified despite rejected duplicate-path patch")
	}
}

// TestPatchRollbackIncompleteSurfacesPath exercises rollbackPatch directly
// (the function Patch calls to undo already-applied files after a later
// file in the same call fails to write): it sets up a valid backup for a
// file, then makes the file's parent directory unwritable — simulating the
// state after a successful backup but before restore — so the restore
// itself fails. rollbackPatch must keep going rather than abort, report the
// path whose restore failed, and rollbackMessage must fold that into a
// caller-visible "rollback incomplete" note naming only the path.
func TestPatchRollbackIncompleteSurfacesPath(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: directory permission bits do not block writes")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	mustWrite(t, path, "ALPHA patched")

	backupPath := path + ".bak"
	mustWrite(t, backupPath, "alpha original")

	// Simulate the post-backup state where the parent directory has become
	// unwritable, so replaceFile's atomic restore (which needs to create a
	// temp file in dir) cannot succeed.
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })

	backups := []patchBackup{{path: path, backupPath: backupPath, mode: 0o644}}
	failed := rollbackPatch(backups)
	if len(failed) != 1 || failed[0] != path {
		t.Fatalf("rollbackPatch failed-restores = %v, want [%s]", failed, path)
	}

	// The file must be left exactly as it was found (still patched, since
	// the restore failed) — not silently reported as rolled back.
	os.Chmod(dir, 0o755)
	if got := mustRead(t, path); got != "ALPHA patched" {
		t.Fatalf("path = %q, want left untouched at %q since its restore failed", got, "ALPHA patched")
	}

	msg := rollbackMessage("write some/other/file: boom", failed)
	if !strings.Contains(msg, "rollback incomplete") {
		t.Fatalf("rollback message = %q, want it to mention rollback incomplete", msg)
	}
	if !strings.Contains(msg, path) {
		t.Fatalf("rollback message = %q, want it to name %s", msg, path)
	}
}

// TestPatchRollbackContinuesPastFirstFailure asserts rollbackPatch restores
// every backup it can even when an earlier one in the list fails, rather
// than aborting the whole rollback on the first error.
func TestPatchRollbackContinuesPastFirstFailure(t *testing.T) {
	dir := t.TempDir()

	goodPath := filepath.Join(dir, "good.txt")
	mustWrite(t, goodPath, "patched")
	goodBackup := filepath.Join(dir, "good.bak")
	mustWrite(t, goodBackup, "original")

	badPath := filepath.Join(dir, "bad.txt")
	mustWrite(t, badPath, "patched")
	// No backup file created at this path: ReadFile will fail, forcing the
	// "restore failed" branch without needing directory permission tricks.
	badBackup := filepath.Join(dir, "missing.bak")

	backups := []patchBackup{
		{path: badPath, backupPath: badBackup, mode: 0o644},
		{path: goodPath, backupPath: goodBackup, mode: 0o644},
	}
	failed := rollbackPatch(backups)
	if len(failed) != 1 || failed[0] != badPath {
		t.Fatalf("rollbackPatch failed-restores = %v, want [%s]", failed, badPath)
	}
	if got := mustRead(t, goodPath); got != "original" {
		t.Fatalf("goodPath = %q, want restored to %q despite an earlier failure", got, "original")
	}
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
