package provision

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// InstallSeed carries the two TOP-LEVEL Kairos install keys that gate a
// zero-touch (autoinstall) disk wipe. These are NOT part of the hadron_agent
// section: they live directly under the cloud-config root ("install.auto" and
// "install.device") and are merged here across every /oem/*.yaml file.
type InstallSeed struct {
	// Auto mirrors the top-level "install.auto" key. A zero-touch install is
	// authorized only when it is explicitly true.
	Auto bool
	// Device mirrors the top-level "install.device" key: the disk the install
	// will write. Authorization additionally requires this to be a non-empty
	// "/dev/..." path so no path can wipe an unspecified or wildcard disk.
	Device string
}

// LoadInstallSeed reads every "*.yaml" file directly inside oemDir (sorted by
// filename, later files winning for each key they set) and returns the merged
// top-level install.auto / install.device values.
//
// A missing directory or no matching files is NOT an error: it yields a zero
// InstallSeed (Auto == false, Device == ""), which never authorizes a wipe.
// Unlike hadron_agent parsing this is deliberately NON-strict: every unrelated
// Kairos key (users, stages, hadron_agent, ...) is ignored simply by not
// appearing in the decode target, so an ordinary cloud-config that merely sets
// other keys is parsed cleanly and reports Auto == false.
func LoadInstallSeed(oemDir string) (InstallSeed, error) {
	paths, err := findYAMLFiles(oemDir)
	if err != nil {
		return InstallSeed{}, fmt.Errorf("provision: %w", err)
	}

	var seed InstallSeed
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return InstallSeed{}, fmt.Errorf("provision: read %s: %w", path, err)
		}
		var doc struct {
			Install struct {
				Auto   *bool   `yaml:"auto"`
				Device *string `yaml:"device"`
			} `yaml:"install"`
		}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return InstallSeed{}, fmt.Errorf("provision: %s: parse yaml: %w", filepath.Base(path), err)
		}
		if doc.Install.Auto != nil {
			seed.Auto = *doc.Install.Auto
		}
		if doc.Install.Device != nil {
			seed.Device = *doc.Install.Device
		}
	}
	return seed, nil
}

// SeedAuthorization is the fully-evaluated verdict on whether the merged
// /oem/*.yaml seed authorizes a zero-touch (non-interactive) disk install.
type SeedAuthorization struct {
	// Authorized is true ONLY when all three independent conditions hold:
	// install.auto == true, install.device is a non-empty "/dev/..." path, and
	// the hadron_agent runtime is enabled. It is the single field the
	// autoinstall path may act on; when it is false, ordinary NON-destructive
	// live mode must proceed.
	Authorized bool
	// Device is the authorized install device, set only when Authorized.
	Device string
	// Reason explains why authorization was withheld, for a non-quiet report.
	Reason string
}

// InspectSeed evaluates the merged /oem/*.yaml seed against the three
// independent conditions that must ALL hold before any disk may be wiped
// without an interactive typed confirmation:
//
//  1. install.auto is explicitly true,
//  2. install.device is a non-empty "/dev/..." path, and
//  3. hadron_agent.enabled is true (an explicit "enabled: false" suppresses it).
//
// It returns Authorized == true (with Device set) only when every condition is
// met; otherwise Authorized is false and Reason names the first failing
// condition. A parse/validation error (malformed seed) is returned as err and
// callers MUST treat it as unauthorized.
func InspectSeed(oemDir string) (SeedAuthorization, error) {
	cfg, err := Load(oemDir)
	if err != nil {
		return SeedAuthorization{}, err
	}
	seed, err := LoadInstallSeed(oemDir)
	if err != nil {
		return SeedAuthorization{}, err
	}

	if !seed.Auto {
		return SeedAuthorization{Reason: "install.auto is not true"}, nil
	}
	device := strings.TrimSpace(seed.Device)
	if !strings.HasPrefix(device, "/dev/") || len(device) <= len("/dev/") {
		return SeedAuthorization{Reason: fmt.Sprintf("install.device %q is not a non-empty /dev/... path", seed.Device)}, nil
	}
	if !cfg.Enabled {
		return SeedAuthorization{Reason: "hadron_agent.enabled is not true"}, nil
	}
	return SeedAuthorization{Authorized: true, Device: device}, nil
}
