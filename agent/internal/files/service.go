// Package files implements the executor behind the read_file, search_files,
// write_file, and patch MCP tools defined in package api. It is invoked by
// both the unprivileged session broker and the root helper (later Phase-2
// tasks): the OS process identity the executor runs under IS the access
// boundary, so this package deliberately implements no path allowlist of its
// own — every syscall below is subject to normal OS permission checks for
// whatever identity the calling process holds.
//
// All four tools are bounded: read/write bodies are capped, search results
// and per-file scan sizes are capped, and patch caps both the number of
// replacements and the aggregate size of files it rewrites. Where a tool's
// output type carries a dedicated Truncated field, exceeding a default bound
// (one the caller did not explicitly widen) is reported as a successful
// result with ResultMeta.Code set to api.CodeOutputTruncated in addition to
// Truncated=true; exceeding a hard bound the caller explicitly requested
// widened is reported the same way once clamped. write_file and patch have
// no truncation concept (a partially written file would be data corruption,
// not a helpful partial result), so exceeding their bounds is a hard
// api.CodeResourceExhausted failure and nothing is written.
package files

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mudler/hadron-desktop/agent/internal/api"
)

// Bounds. See the package doc comment for how default vs. max are applied
// per tool.
const (
	// DefaultReadBytes is how much of a file read_file returns when the
	// caller does not specify limit.
	DefaultReadBytes int64 = 1 << 20 // 1 MiB
	// MaxReadBytes is the hard ceiling read_file will ever return in one
	// call, regardless of a caller-supplied limit.
	MaxReadBytes int64 = 8 << 20 // 8 MiB
	// MaxWriteBytes is the hard ceiling on decoded write_file content.
	// Unlike read, write has no "default" — content is either written in
	// full or rejected, since a partial write would corrupt the file.
	MaxWriteBytes int64 = 8 << 20 // 8 MiB

	// DefaultSearchResults is how many matches search_files returns when
	// the caller does not specify max_results.
	DefaultSearchResults = 100
	// MaxSearchResults is the hard ceiling on search_files matches,
	// regardless of a caller-supplied max_results.
	MaxSearchResults = 1000
	// MaxSearchFileBytes is the hard ceiling on the size of an individual
	// file search_files will read for a content search; larger files are
	// skipped. A caller-supplied max_file_bytes may only lower this, never
	// raise it.
	MaxSearchFileBytes int64 = 1 << 20 // 1 MiB

	// MaxPatchReplacements is the hard ceiling on the total number of
	// individual replacement operations a single patch call may perform
	// across all files.
	MaxPatchReplacements = 128
	// MaxPatchAggregateBytes is the hard ceiling on the summed size of every
	// file a single patch call rewrites.
	MaxPatchAggregateBytes int64 = 8 << 20 // 8 MiB
)

// backupSuffix pattern used for patch's same-directory rollback backups.
const backupPattern = ".hadron-patch-bak-%d-%d"

// tempPattern is the same-directory temp file pattern used by the atomic
// write path.
const tempPattern = ".hadron-write-*.tmp"

// Service executes the bounded filesystem tools. It holds no state and no
// path allowlist: the identity of the OS process it runs in is the access
// boundary, so a Service is safe to share across concurrent calls.
type Service struct{}

// NewService constructs a Service.
func NewService() *Service {
	return &Service{}
}

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// fail builds a failure ResultMeta.
func fail(code api.ErrorCode, msg string, retryable bool) api.ResultMeta {
	return api.ResultMeta{Code: code, Message: msg, Retryable: retryable}
}

func invalidArgument(err error) api.ResultMeta {
	return fail(api.CodeInvalidArgument, err.Error(), false)
}

// mapStatError classifies an error from Stat/Lstat/Open/ReadFile into a
// ResultMeta.
func mapStatError(err error) api.ResultMeta {
	switch {
	case os.IsNotExist(err):
		return fail(api.CodeNotFound, err.Error(), false)
	case os.IsPermission(err):
		return fail(api.CodeForbidden, err.Error(), false)
	default:
		return fail(api.CodeInternal, err.Error(), false)
	}
}

// mapWriteError classifies an error from the atomic replace path.
func mapWriteError(err error) api.ResultMeta {
	switch {
	case os.IsPermission(err):
		return fail(api.CodeForbidden, err.Error(), true)
	case os.IsNotExist(err):
		return fail(api.CodeNotFound, err.Error(), false)
	default:
		return fail(api.CodeInternal, err.Error(), false)
	}
}

// ctxMeta reports a non-nil ResultMeta if ctx has already been cancelled or
// timed out, and ok=false; otherwise ok=true and the caller should proceed.
func ctxMeta(ctx context.Context) (meta api.ResultMeta, ok bool) {
	switch ctx.Err() {
	case nil:
		return api.ResultMeta{}, true
	case context.DeadlineExceeded:
		return fail(api.CodeDeadlineExceeded, ctx.Err().Error(), true), false
	default:
		return fail(api.CodeInternal, ctx.Err().Error(), true), false
	}
}

// decodeContent decodes body per the requested encoding. The zero value ""
// means utf8.
func decodeContent(content string, encoding api.FileEncoding) ([]byte, error) {
	switch encoding {
	case "", api.EncodingUTF8:
		return []byte(content), nil
	case api.EncodingBase64:
		data, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 content: %w", err)
		}
		return data, nil
	default:
		return nil, fmt.Errorf("invalid encoding %q", encoding)
	}
}

// encodeContent renders data per the requested encoding. If requested is
// utf8 (or unset, its default) but data is not valid UTF-8, it silently
// falls back to base64: a Go string holding invalid UTF-8 would otherwise be
// corrupted by JSON marshaling (invalid bytes are replaced with U+FFFD), and
// this way the round trip is always lossless. The output encoding actually
// used is returned alongside the content so callers can tell which happened.
func encodeContent(data []byte, requested api.FileEncoding) (string, api.FileEncoding) {
	if requested == api.EncodingBase64 {
		return base64.StdEncoding.EncodeToString(data), api.EncodingBase64
	}
	if utf8.Valid(data) {
		return string(data), api.EncodingUTF8
	}
	return base64.StdEncoding.EncodeToString(data), api.EncodingBase64
}

// parseFileMode parses a POSIX mode string such as "0644" or "644".
func parseFileMode(s string) (os.FileMode, error) {
	v, err := strconv.ParseUint(s, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid mode %q: %w", s, err)
	}
	return os.FileMode(v) & os.ModePerm, nil
}

// ownerInfo is the Unix uid/gid of an existing file, captured so a
// replacement can preserve it.
type ownerInfo struct {
	uid int
	gid int
}

// ownerOf extracts ownerInfo from fi, or nil if unavailable (non-Unix, or
// the Sys() value is not a *syscall.Stat_t).
func ownerOf(fi os.FileInfo) *ownerInfo {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	return &ownerInfo{uid: int(st.Uid), gid: int(st.Gid)}
}

// isSymlink reports whether fi (from Lstat) is a symlink.
func isSymlink(fi os.FileInfo) bool {
	return fi.Mode()&os.ModeSymlink != 0
}

// replaceFile atomically replaces path's contents with data: it writes to a
// same-directory temporary file, fsyncs it, chmods it to mode, best-effort
// chowns it to owner (when non-nil — preserving ownership of a file being
// replaced; the unprivileged broker cannot chown, so a permission failure
// here is swallowed rather than failing the write), renames it over path,
// and fsyncs the parent directory. The rename is what makes this atomic:
// path either has its old content or its new content at every observable
// instant, never a partial write.
func replaceFile(path string, data []byte, mode os.FileMode, owner *ownerInfo) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	if owner != nil {
		// Best effort: an unprivileged caller cannot chown to an arbitrary
		// uid/gid and will get EPERM here. Preservation is a nicety for the
		// root helper's replacements, not a correctness requirement for the
		// unprivileged broker's, so degrade gracefully.
		_ = os.Chown(tmpPath, owner.uid, owner.gid)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	renamed = true

	// Best effort: fsync the parent directory so the rename itself is
	// durable. Some filesystems reject syncing a directory fd; that must
	// not fail a write whose rename has already landed.
	if dirFile, err := os.Open(dir); err == nil {
		_ = dirFile.Sync()
		dirFile.Close()
	}

	return nil
}

// ---------------------------------------------------------------------------
// read_file
// ---------------------------------------------------------------------------

// Read implements the read_file tool. It may follow a final file symlink
// (Stat/Open both traverse symlinks, subject to normal OS permissions);
// nothing about read_file needs to reject one, unlike write_file/patch which
// would otherwise silently replace the link target.
func (s *Service) Read(ctx context.Context, in api.ReadFileInput) api.ReadFileOutput {
	if meta, ok := ctxMeta(ctx); !ok {
		return api.ReadFileOutput{ResultMeta: meta}
	}
	if err := in.Validate(); err != nil {
		return api.ReadFileOutput{ResultMeta: invalidArgument(err)}
	}
	if in.Offset != nil && *in.Offset < 0 {
		return api.ReadFileOutput{ResultMeta: invalidArgument(fmt.Errorf("offset must be >= 0"))}
	}
	if in.Limit != nil && *in.Limit < 0 {
		return api.ReadFileOutput{ResultMeta: invalidArgument(fmt.Errorf("limit must be >= 0"))}
	}

	fi, err := os.Stat(in.Path)
	if err != nil {
		return api.ReadFileOutput{ResultMeta: mapStatError(err)}
	}
	if fi.IsDir() {
		return api.ReadFileOutput{ResultMeta: invalidArgument(fmt.Errorf("%s is a directory", in.Path))}
	}

	if in.MetadataOnly {
		return api.ReadFileOutput{Size: fi.Size()}
	}

	f, err := os.Open(in.Path)
	if err != nil {
		return api.ReadFileOutput{ResultMeta: mapStatError(err)}
	}
	defer f.Close()

	var offset int64
	if in.Offset != nil {
		offset = *in.Offset
	}

	limit := DefaultReadBytes
	explicitLimit := false
	clamped := false
	if in.Limit != nil {
		explicitLimit = true
		limit = *in.Limit
		if limit > MaxReadBytes {
			limit = MaxReadBytes
			clamped = true
		}
	}

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return api.ReadFileOutput{ResultMeta: mapStatError(err)}
		}
	}

	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return api.ReadFileOutput{ResultMeta: mapStatError(err)}
	}
	data := buf[:n]

	moreData := offset+int64(n) < fi.Size()
	truncated := moreData

	var meta api.ResultMeta
	if truncated && (clamped || !explicitLimit) {
		meta = fail(api.CodeOutputTruncated, "content truncated by the read size bound", false)
	}

	content, outEncoding := encodeContent(data, in.Encoding)
	return api.ReadFileOutput{
		ResultMeta: meta,
		Content:    content,
		Encoding:   outEncoding,
		Size:       fi.Size(),
		Truncated:  truncated,
	}
}

// ---------------------------------------------------------------------------
// search_files
// ---------------------------------------------------------------------------

// Search implements the search_files tool. filepath.WalkDir is used
// unmodified: it identifies directory entries by the Lstat-style type bits
// reported by the parent directory's listing, so a symlink to a directory is
// never itself reported as a directory and WalkDir never recurses into one —
// directory symlinks are not followed. Symlinked files are additionally
// skipped explicitly below (not required by the brief, but avoids search
// silently reading through an unexpected link).
func (s *Service) Search(ctx context.Context, in api.SearchFilesInput) api.SearchFilesOutput {
	if meta, ok := ctxMeta(ctx); !ok {
		return api.SearchFilesOutput{ResultMeta: meta}
	}
	if err := in.Validate(); err != nil {
		return api.SearchFilesOutput{ResultMeta: invalidArgument(err)}
	}

	mode := in.Mode
	if mode == "" {
		mode = api.SearchByName
	}

	maxResults := DefaultSearchResults
	explicitMaxResults := false
	clampedResults := false
	if in.MaxResults != nil {
		explicitMaxResults = true
		maxResults = *in.MaxResults
		if maxResults <= 0 {
			return api.SearchFilesOutput{ResultMeta: invalidArgument(fmt.Errorf("max_results must be > 0"))}
		}
		if maxResults > MaxSearchResults {
			maxResults = MaxSearchResults
			clampedResults = true
		}
	}

	maxFileBytes := MaxSearchFileBytes
	if in.MaxFileBytes != nil {
		if *in.MaxFileBytes <= 0 {
			return api.SearchFilesOutput{ResultMeta: invalidArgument(fmt.Errorf("max_file_bytes must be > 0"))}
		}
		if *in.MaxFileBytes < maxFileBytes {
			maxFileBytes = *in.MaxFileBytes
		}
	}

	var re *regexp.Regexp
	if in.Regex {
		compiled, err := regexp.Compile(in.Query)
		if err != nil {
			return api.SearchFilesOutput{ResultMeta: invalidArgument(fmt.Errorf("invalid regex: %w", err))}
		}
		re = compiled
	}

	match := func(s string) bool {
		if re != nil {
			return re.MatchString(s)
		}
		return strings.Contains(s, in.Query)
	}

	var matches []api.SearchMatch
	truncated := false

	walkErr := filepath.WalkDir(in.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				// The root itself could not be stat'd.
				return err
			}
			// A subdirectory could not be read (e.g. permission denied);
			// skip it and keep searching the rest of the tree.
			return nil
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}

		switch mode {
		case api.SearchByName:
			if match(d.Name()) {
				matches = append(matches, api.SearchMatch{Path: p})
			}
		case api.SearchByContent:
			info, ierr := d.Info()
			if ierr != nil {
				return nil
			}
			if info.Size() > maxFileBytes {
				return nil
			}
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if !utf8.Valid(data) {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if match(line) {
					matches = append(matches, api.SearchMatch{Path: p, Line: i + 1, Excerpt: line})
					if len(matches) >= maxResults {
						break
					}
				}
			}
		}

		if len(matches) >= maxResults {
			truncated = true
			return filepath.SkipAll
		}
		return nil
	})

	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) || errors.Is(walkErr, context.DeadlineExceeded) {
			meta, _ := ctxMeta(ctx)
			return api.SearchFilesOutput{ResultMeta: meta}
		}
		return api.SearchFilesOutput{ResultMeta: mapStatError(walkErr)}
	}

	var meta api.ResultMeta
	if truncated && (clampedResults || !explicitMaxResults) {
		meta = fail(api.CodeOutputTruncated, "result set truncated by the search size bound", false)
	}

	return api.SearchFilesOutput{ResultMeta: meta, Matches: matches, Truncated: truncated}
}

// ---------------------------------------------------------------------------
// write_file
// ---------------------------------------------------------------------------

// Write implements the write_file tool: an atomic same-directory temp file +
// fsync + rename + parent-directory fsync, preserving mode/ownership when
// replacing an existing file. It rejects writing through a final symlink so
// an atomic rename cannot silently replace the link's target.
func (s *Service) Write(ctx context.Context, in api.WriteFileInput) api.WriteFileOutput {
	if meta, ok := ctxMeta(ctx); !ok {
		return api.WriteFileOutput{ResultMeta: meta}
	}
	if err := in.Validate(); err != nil {
		return api.WriteFileOutput{ResultMeta: invalidArgument(err)}
	}

	data, err := decodeContent(in.Content, in.Encoding)
	if err != nil {
		return api.WriteFileOutput{ResultMeta: invalidArgument(err)}
	}
	if int64(len(data)) > MaxWriteBytes {
		return api.WriteFileOutput{ResultMeta: fail(api.CodeResourceExhausted, fmt.Sprintf("content of %d bytes exceeds max write size of %d bytes", len(data), MaxWriteBytes), false)}
	}

	var mode os.FileMode = 0o644
	var owner *ownerInfo
	existed := false

	lst, err := os.Lstat(in.Path)
	switch {
	case err == nil:
		existed = true
		if isSymlink(lst) {
			return api.WriteFileOutput{ResultMeta: fail(api.CodeAlreadyExists, fmt.Sprintf("%s is a symlink; write_file refuses to replace a symlink's target", in.Path), false)}
		}
		if lst.IsDir() {
			return api.WriteFileOutput{ResultMeta: invalidArgument(fmt.Errorf("%s is a directory", in.Path))}
		}
		mode = lst.Mode().Perm()
		owner = ownerOf(lst)
	case os.IsNotExist(err):
		// Fine: this is a new file.
	default:
		return api.WriteFileOutput{ResultMeta: mapStatError(err)}
	}

	dir := filepath.Dir(in.Path)
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return api.WriteFileOutput{ResultMeta: mapStatError(err)}
		}
		if !in.CreateParents {
			return api.WriteFileOutput{ResultMeta: fail(api.CodeNotFound, fmt.Sprintf("parent directory %s does not exist", dir), false)}
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return api.WriteFileOutput{ResultMeta: fail(api.CodeInternal, fmt.Sprintf("create parent directories: %v", err), false)}
		}
	}

	if in.Mode != "" {
		parsed, err := parseFileMode(in.Mode)
		if err != nil {
			return api.WriteFileOutput{ResultMeta: invalidArgument(err)}
		}
		mode = parsed
	}

	if err := replaceFile(in.Path, data, mode, owner); err != nil {
		return api.WriteFileOutput{ResultMeta: mapWriteError(err)}
	}

	return api.WriteFileOutput{BytesWritten: int64(len(data)), Created: !existed}
}

// ---------------------------------------------------------------------------
// patch
// ---------------------------------------------------------------------------

// patchPlan is one file's fully computed replacement, produced during
// Patch's planning phase before any file on disk is touched.
type patchPlan struct {
	path     string
	original []byte
	updated  []byte
	mode     os.FileMode
	owner    *ownerInfo
}

// patchBackup records a same-directory backup file written during Patch's
// apply phase, so an already-replaced file can be restored if a later file
// in the same call fails to write.
type patchBackup struct {
	path       string
	backupPath string
	mode       os.FileMode
	owner      *ownerInfo
}

// Patch implements the patch tool. It plans every exact replacement for
// every file in memory first (rejecting an absent or non-unique `old` unless
// its replacement sets all=true, and enforcing the replacement-count and
// aggregate-size caps) before writing anything. Only once the whole call is
// known to be applicable does it write files one at a time; if any write
// fails partway through, every file already replaced in this call is
// restored from a same-directory backup before the failure is returned, so a
// partially-applied patch is never observable.
func (s *Service) Patch(ctx context.Context, in api.PatchInput) api.PatchOutput {
	if meta, ok := ctxMeta(ctx); !ok {
		return api.PatchOutput{ResultMeta: meta}
	}
	if err := in.Validate(); err != nil {
		return api.PatchOutput{ResultMeta: invalidArgument(err)}
	}

	// Reject duplicate paths up front, before any file is read: applying two
	// entries for the same path would have the second's backup capture the
	// first's already-updated content as "original", silently losing the
	// first entry's edits while reporting the path as changed twice.
	seen := make(map[string]struct{}, len(in.Files))
	for _, pf := range in.Files {
		clean := filepath.Clean(pf.Path)
		if _, dup := seen[clean]; dup {
			return api.PatchOutput{ResultMeta: fail(api.CodeInvalidArgument, fmt.Sprintf("duplicate path in patch: %s", pf.Path), false)}
		}
		seen[clean] = struct{}{}
	}

	plans := make([]patchPlan, 0, len(in.Files))
	totalReplacements := 0
	var aggregateBytes int64

	for _, pf := range in.Files {
		lst, err := os.Lstat(pf.Path)
		if err != nil {
			return api.PatchOutput{ResultMeta: mapStatError(err)}
		}
		if isSymlink(lst) {
			return api.PatchOutput{ResultMeta: fail(api.CodeAlreadyExists, fmt.Sprintf("%s is a symlink; patch refuses to replace a symlink's target", pf.Path), false)}
		}
		if lst.IsDir() {
			return api.PatchOutput{ResultMeta: invalidArgument(fmt.Errorf("%s is a directory", pf.Path))}
		}
		// Reject an oversized file by its on-disk size (from the Lstat
		// above) before reading it into memory: the aggregate cap is
		// re-checked after reading below too, but that check alone would
		// still have already paid for a multi-GB ReadFile by the time it
		// fires. This package also runs as the root helper, so an
		// unbounded read here is an OOM vector, not just a slow path.
		if lst.Size() > MaxPatchAggregateBytes {
			return api.PatchOutput{ResultMeta: fail(api.CodeResourceExhausted, fmt.Sprintf("%s is %d bytes, exceeding max aggregate patch size of %d bytes", pf.Path, lst.Size(), MaxPatchAggregateBytes), false)}
		}

		original, err := os.ReadFile(pf.Path)
		if err != nil {
			return api.PatchOutput{ResultMeta: mapStatError(err)}
		}

		content := original
		for _, r := range pf.Replacements {
			count := strings.Count(string(content), r.Old)
			if count == 0 {
				return api.PatchOutput{ResultMeta: fail(api.CodeNotFound, fmt.Sprintf("%s: old text not found: %q", pf.Path, r.Old), false)}
			}
			if count > 1 && !r.All {
				return api.PatchOutput{ResultMeta: fail(api.CodeInvalidArgument, fmt.Sprintf("%s: old text matches %d times; set all=true to replace every occurrence: %q", pf.Path, count, r.Old), false)}
			}

			applied := 1
			if r.All {
				content = bytes.ReplaceAll(content, []byte(r.Old), []byte(r.New))
				applied = count
			} else {
				content = bytes.Replace(content, []byte(r.Old), []byte(r.New), 1)
			}

			totalReplacements += applied
			if totalReplacements > MaxPatchReplacements {
				return api.PatchOutput{ResultMeta: fail(api.CodeResourceExhausted, fmt.Sprintf("patch exceeds max %d replacements", MaxPatchReplacements), false)}
			}
		}

		aggregateBytes += int64(len(content))
		if aggregateBytes > MaxPatchAggregateBytes {
			return api.PatchOutput{ResultMeta: fail(api.CodeResourceExhausted, fmt.Sprintf("patch exceeds max aggregate %d bytes", MaxPatchAggregateBytes), false)}
		}

		plans = append(plans, patchPlan{
			path:     pf.Path,
			original: original,
			updated:  content,
			mode:     lst.Mode().Perm(),
			owner:    ownerOf(lst),
		})
	}

	// Apply phase: write files one at a time, keeping a same-directory
	// backup of each original before it is replaced, so we can roll back if
	// a later file fails.
	backups := make([]patchBackup, 0, len(plans))
	changed := make([]string, 0, len(plans))

	for i, p := range plans {
		backupPath := p.path + fmt.Sprintf(backupPattern, os.Getpid(), time.Now().UnixNano())
		if err := os.WriteFile(backupPath, p.original, 0o600); err != nil {
			failed := rollbackPatch(backups)
			cleanupBackups(backups)
			return api.PatchOutput{ResultMeta: fail(api.CodeInternal, rollbackMessage(fmt.Sprintf("create backup for %s: %v", p.path, err), failed), false)}
		}
		backups = append(backups, patchBackup{path: p.path, backupPath: backupPath, mode: p.mode, owner: p.owner})

		if err := replaceFile(p.path, p.updated, p.mode, p.owner); err != nil {
			// This file's own replace failed before any rename landed (or
			// the rename itself failed), so it still holds its original
			// content and needs no restore. Everything before it in this
			// call (backups[:i]) was already replaced and does.
			failed := rollbackPatch(backups[:i])
			cleanupBackups(backups)
			return api.PatchOutput{ResultMeta: fail(api.CodeInternal, rollbackMessage(fmt.Sprintf("write %s: %v", p.path, err), failed), false)}
		}
		changed = append(changed, p.path)
	}

	cleanupBackups(backups)
	return api.PatchOutput{FilesChanged: changed, ReplacementsApplied: totalReplacements}
}

// rollbackMessage appends a "rollback incomplete" note listing the paths
// (never contents) that failed to restore to base, so a caller cannot
// mistake a partially-rolled-back patch for a fully-undone one.
func rollbackMessage(base string, failedRestores []string) string {
	if len(failedRestores) == 0 {
		return base
	}
	return fmt.Sprintf("%s; rollback incomplete for %s: these files may still hold patched content", base, strings.Join(failedRestores, ", "))
}

// rollbackPatch restores every file in backups to its pre-patch content,
// using the same atomic replace path as a normal write. It keeps restoring
// the remaining files even if one restore fails, and returns the paths of
// any files whose restore did not succeed so the caller can surface that a
// rollback left the filesystem in a partially-modified state.
func rollbackPatch(backups []patchBackup) (failedRestores []string) {
	for _, b := range backups {
		data, err := os.ReadFile(b.backupPath)
		if err != nil {
			failedRestores = append(failedRestores, b.path)
			continue
		}
		if err := replaceFile(b.path, data, b.mode, b.owner); err != nil {
			failedRestores = append(failedRestores, b.path)
		}
	}
	return failedRestores
}

// cleanupBackups best-effort removes every backup file, whether the patch
// succeeded or was rolled back.
func cleanupBackups(backups []patchBackup) {
	for _, b := range backups {
		os.Remove(b.backupPath)
	}
}
