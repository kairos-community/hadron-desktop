// This file is the redacted status publisher: the gateway serializes an
// 11-field, secret-free snapshot to a world-readable status.json that the
// appliance's i3 status bar (hadron-status) and one-shot first-run panel
// (hadron-agent-first-run) read. The FIELD NAMES here are a frozen contract
// with those two shell scripts -- see their header comments -- and must never
// drift.
//
// This snapshot is DISTINCT from control.Status (the 5-field /healthz body):
// /healthz stays byte-for-byte frozen, while this richer snapshot adds the
// derived readiness/degraded flags, the live computer_use_active flag, and the
// static provisioned cert_fingerprint/mdns/listen fields the panel displays.
//
// It carries NO secret: no bearer, no token, no path beyond the public listen
// address. The file is written atomically (temp file in the same directory,
// then rename) at a short cadence so a crash mid-write can only leave the old
// or the new file, never a truncated one.
package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Defaults for the redacted status publisher.
const (
	// DefaultStatusFile is the standard path the redacted status.json is
	// published to. The gateway unit grants ReadWritePaths on its parent dir.
	DefaultStatusFile = "/run/hadron-agent/status/status.json"
	// DefaultStatusInterval is how often the writer republishes the snapshot.
	DefaultStatusInterval = time.Second
)

// RedactedStatus is the fully redacted, 11-field snapshot published to
// status.json. Every field name here is depended on VERBATIM by hadron-status
// and hadron-agent-first-run; do not rename them. It carries only a version
// string, the public listen address, the redacted fingerprint, and booleans --
// never a bearer, token, user, or private path.
type RedactedStatus struct {
	Version           string `json:"version"`
	Gateway           bool   `json:"gateway"`
	Session           bool   `json:"session"`
	Cua               bool   `json:"cua"`
	Paused            bool   `json:"paused"`
	Degraded          bool   `json:"degraded"`
	ComputerUseActive bool   `json:"computer_use_active"`
	Ready             bool   `json:"ready"`
	CertFingerprint   string `json:"cert_fingerprint"`
	MDNS              bool   `json:"mdns"`
	Listen            string `json:"listen"`
}

// ComputerUseActive reports whether a remote computer_use tool call is
// CURRENTLY executing through the gateway. It is true only while at least one
// computer_use dispatch is in flight; no other tool and no merely-open MCP
// connection ever flips it.
func (g *Gateway) ComputerUseActive() bool {
	return g.computerUse.Load() > 0
}

// redactedStatus assembles the current 11-field snapshot. gateway/session/cua/
// paused/version come from the controller's redacted status; ready is its
// aggregate readiness; degraded is "up but not fully ready and not paused";
// computer_use_active is the live in-flight flag; and cert_fingerprint/mdns/
// listen are the static provisioned values (listen is the address actually
// bound).
func (g *Gateway) redactedStatus(ctx context.Context) RedactedStatus {
	s := g.controller.Status(ctx)
	ready := s.Ready()
	return RedactedStatus{
		Version:           s.Version,
		Gateway:           s.Gateway,
		Session:           s.Session,
		Cua:               s.Cua,
		Paused:            s.Paused,
		Degraded:          s.Gateway && !ready && !s.Paused,
		ComputerUseActive: g.ComputerUseActive(),
		Ready:             ready,
		CertFingerprint:   g.certFingerprint,
		MDNS:              g.mdns,
		Listen:            g.listenAddr,
	}
}

// writeStatusFile atomically publishes the current snapshot to the configured
// status file. It is a no-op when no status file is configured. The file is
// written 0644 (world-readable is fine: it is redacted) via a temp file in the
// SAME directory followed by an atomic rename, so a reader never observes a
// partial write.
func (g *Gateway) writeStatusFile(ctx context.Context) error {
	if g.statusFile == "" {
		return nil
	}
	data, err := json.Marshal(g.redactedStatus(ctx))
	if err != nil {
		return err
	}
	data = append(data, '\n')

	dir := filepath.Dir(g.statusFile)
	tmp, err := os.CreateTemp(dir, ".status-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Removed if anything below fails; a successful rename makes this a no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, g.statusFile)
}

// RunStatusWriter publishes the redacted status.json immediately and then every
// interval until ctx is cancelled. It is a no-op (returns at once) when no
// status file is configured, so tests and the contract suite never touch the
// filesystem. A transient write failure is logged (without the path, to keep
// the journal free of even non-secret filesystem detail) and retried on the
// next tick rather than aborting the loop.
func (g *Gateway) RunStatusWriter(ctx context.Context, interval time.Duration) {
	if g.statusFile == "" {
		return
	}
	if interval <= 0 {
		interval = DefaultStatusInterval
	}

	// Publish once up front so the status bar leaves "Starting" promptly.
	if err := g.writeStatusFile(ctx); err != nil {
		g.logger.Warn("status publish failed")
	}

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := g.writeStatusFile(ctx); err != nil {
				g.logger.Warn("status publish failed")
			}
		}
	}
}
