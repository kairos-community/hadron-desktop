package provision

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSeed writes a single cloud-config file into a fresh OEM dir and returns
// the dir path.
func writeSeed(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	return dir
}

func TestInspectSeed(t *testing.T) {
	cases := []struct {
		name         string
		content      string
		wantAuth     bool
		wantDevice   string
		wantParseErr bool
	}{
		{
			name:     "absent seed authorizes nothing",
			content:  "#cloud-config\n",
			wantAuth: false,
		},
		{
			name: "unrelated cloud-config does not authorize",
			// Other Kairos keys, but no install.auto -> never authorized.
			content: "#cloud-config\nhostname: box\nusers:\n  - name: alice\nstages:\n  boot: []\n",
		},
		{
			name:    "auto false does not authorize",
			content: "#cloud-config\ninstall:\n  auto: false\n  device: /dev/sda\nhadron_agent:\n  enabled: true\n",
		},
		{
			name:    "auto true but no device does not authorize",
			content: "#cloud-config\ninstall:\n  auto: true\nhadron_agent:\n  enabled: true\n",
		},
		{
			name:    "auto true but empty device does not authorize",
			content: "#cloud-config\ninstall:\n  auto: true\n  device: \"\"\nhadron_agent:\n  enabled: true\n",
		},
		{
			name:    "auto true but non-dev device does not authorize",
			content: "#cloud-config\ninstall:\n  auto: true\n  device: auto\nhadron_agent:\n  enabled: true\n",
		},
		{
			name:    "auto+device but agent disabled does not authorize",
			content: "#cloud-config\ninstall:\n  auto: true\n  device: /dev/sda\nhadron_agent:\n  enabled: false\n",
		},
		{
			name:       "fully authorized",
			content:    "#cloud-config\ninstall:\n  auto: true\n  device: /dev/vda\nhadron_agent:\n  enabled: true\n",
			wantAuth:   true,
			wantDevice: "/dev/vda",
		},
		{
			name:       "fully authorized with implicit enabled default",
			content:    "#cloud-config\ninstall:\n  auto: true\n  device: /dev/sdb\n",
			wantAuth:   true,
			wantDevice: "/dev/sdb",
		},
		{
			name:         "malformed yaml is a parse error, never authorized",
			content:      "#cloud-config\ninstall:\n  auto: true\n  device: [unterminated\n",
			wantParseErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeSeed(t, "50_seed.yaml", tc.content)
			verdict, err := InspectSeed(dir)
			if tc.wantParseErr {
				if err == nil {
					t.Fatalf("expected parse error, got verdict %+v", verdict)
				}
				if verdict.Authorized {
					t.Fatalf("parse error must never authorize")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if verdict.Authorized != tc.wantAuth {
				t.Fatalf("Authorized = %v (%s), want %v", verdict.Authorized, verdict.Reason, tc.wantAuth)
			}
			if verdict.Authorized && verdict.Device != tc.wantDevice {
				t.Fatalf("Device = %q, want %q", verdict.Device, tc.wantDevice)
			}
			if !verdict.Authorized && verdict.Reason == "" {
				t.Fatalf("unauthorized verdict must carry a reason")
			}
		})
	}
}

// A missing OEM directory yields a zero, unauthorized seed rather than an error.
func TestInspectSeedMissingDir(t *testing.T) {
	verdict, err := InspectSeed(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
	if verdict.Authorized {
		t.Fatalf("missing dir must not authorize")
	}
}

// Later files win for install.auto/device, matching hadron_agent scalar merge.
func TestLoadInstallSeedLastFileWins(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "10_a.yaml"),
		[]byte("install:\n  auto: false\n  device: /dev/sda\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20_b.yaml"),
		[]byte("install:\n  auto: true\n  device: /dev/sdb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	seed, err := LoadInstallSeed(dir)
	if err != nil {
		t.Fatalf("LoadInstallSeed: %v", err)
	}
	if !seed.Auto || seed.Device != "/dev/sdb" {
		t.Fatalf("last file should win: got %+v", seed)
	}
}
