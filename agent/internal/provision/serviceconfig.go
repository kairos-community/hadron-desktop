// This file is the READ side of the machine state state.go writes: the public
// loaders the hadron-agent gateway/session/root-helper services call at startup
// to derive their entire configuration (listen address, TLS paths, bearer
// rotation, and limits) from the config.json the provisioner persisted, instead
// of re-plumbing every value through flags and environment variables.
//
// The on-disk JSON shapes (GatewayConfig, SessionConfig, RootConfig, Rotation,
// ServiceLimits) are defined in state.go and shared with the writer, so a
// writer -> reader round-trip is exact by construction and the two can never
// disagree on a field name.
package provision

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/mudler/hadron-desktop/agent/internal/auth"
)

// LoadGatewayConfig reads and decodes a gateway config.json (the file
// Materialize writes to <state>/gateway/config.json). A missing file, an
// unreadable file, or malformed JSON is a hard error: unlike provisioning's
// idempotent first-boot path, a service that cannot load its config must fail
// to start rather than run with an empty, credential-less configuration.
func LoadGatewayConfig(path string) (GatewayConfig, error) {
	var gw GatewayConfig
	if err := loadJSONFile(path, &gw); err != nil {
		return GatewayConfig{}, fmt.Errorf("provision: load gateway config: %w", err)
	}
	return gw, nil
}

// LoadSessionConfig reads and decodes a session config.json (the file
// Materialize writes to <state>/session/config.json).
func LoadSessionConfig(path string) (SessionConfig, error) {
	var sc SessionConfig
	if err := loadJSONFile(path, &sc); err != nil {
		return SessionConfig{}, fmt.Errorf("provision: load session config: %w", err)
	}
	return sc, nil
}

// LoadRootConfig reads and decodes a root config.json (the file Materialize
// writes to <state>/root/config.json when an admin bearer is provisioned).
func LoadRootConfig(path string) (RootConfig, error) {
	var rc RootConfig
	if err := loadJSONFile(path, &rc); err != nil {
		return RootConfig{}, fmt.Errorf("provision: load root config: %w", err)
	}
	return rc, nil
}

// loadJSONFile reads path and JSON-decodes it into v, distinguishing a read
// error from a parse error so a corrupt config is reported as such.
func loadJSONFile(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("config %s is corrupt: %w", path, err)
	}
	return nil
}

// AuthRotation converts an on-disk Rotation into the auth.RotationConfig a
// Verifier is built from, honoring the bounded-overlap previous digest. An
// empty Previous means no rotation is in progress (the common case) and yields
// a config with only Current set. A Previous with an empty or unparseable
// previous_valid_until is rejected here rather than silently accepting a
// previous digest that would never expire; auth.NewVerifier separately enforces
// the 24h overlap bound as a second line of defense.
func (r Rotation) AuthRotation() (auth.RotationConfig, error) {
	cfg := auth.RotationConfig{Current: auth.Digest(r.Current)}
	if r.Previous == "" {
		if r.PreviousValidUntil != "" {
			return auth.RotationConfig{}, fmt.Errorf("provision: rotation has previous_valid_until without a previous digest")
		}
		return cfg, nil
	}
	cfg.Previous = auth.Digest(r.Previous)
	if r.PreviousValidUntil == "" {
		return auth.RotationConfig{}, fmt.Errorf("provision: rotation has a previous digest without previous_valid_until")
	}
	until, err := time.Parse(time.RFC3339, r.PreviousValidUntil)
	if err != nil {
		return auth.RotationConfig{}, fmt.Errorf("provision: parse previous_valid_until %q: %w", r.PreviousValidUntil, err)
	}
	cfg.PreviousValidUntil = until
	return cfg, nil
}
