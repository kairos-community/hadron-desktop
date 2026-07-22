package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderCloudConfigContainsRequiredFields(t *testing.T) {
	c := Choice{Hostname: "box", Username: "ada", Disk: "/dev/vda"}
	out := RenderCloudConfig(c, "$6$abc$def")

	for _, want := range []string{
		"#cloud-config",
		"device: \"/dev/vda\"",
		"auto: true",
		"reboot: true",
		"hostname: \"box\"",
		"- name: \"ada\"",
		"passwd: '$6$abc$def'",
		"groups: [admin, audio, video, render, input, bluetooth, seat, docker]",
		"path: /etc/ly/save.ini",
		"user = ada",
		"session_index = 2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered config missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderCloudConfigOmitsSSHWhenNoGithub(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda"}, "x")
	if strings.Contains(out, "ssh_authorized_keys") {
		t.Errorf("expected no ssh_authorized_keys block:\n%s", out)
	}
}

func TestRenderCloudConfigIncludesSSHWhenGithubSet(t *testing.T) {
	out := RenderCloudConfig(Choice{Hostname: "h", Username: "u", Disk: "/dev/sda", Github: "ada"}, "x")
	if !strings.Contains(out, "ssh_authorized_keys:") || !strings.Contains(out, "- github:ada") {
		t.Errorf("expected github key import:\n%s", out)
	}
}

func TestConfigPresentDetectsUsersOrInstallBlock(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "99_hadron-user.yaml")

	// Empty dir: nothing present.
	if got, err := ConfigPresent(dir, out); err != nil || got {
		t.Fatalf("empty dir: got %v err %v, want false nil", got, err)
	}

	// Our own output file must be ignored, or a rerun would skip the wizard.
	if err := os.WriteFile(out, []byte("users:\n  - name: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ConfigPresent(dir, out); err != nil || got {
		t.Fatalf("own output file: got %v err %v, want false nil", got, err)
	}

	// A foreign config with a users: block counts.
	if err := os.WriteFile(filepath.Join(dir, "10_other.yaml"), []byte("users:\n  - name: y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := ConfigPresent(dir, out); err != nil || !got {
		t.Fatalf("foreign users config: got %v err %v, want true nil", got, err)
	}
}

func TestConfigPresentIgnoresUnrelatedKeys(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "10_other.yaml"), []byte("stages:\n  boot: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ConfigPresent(dir, filepath.Join(dir, "99_hadron-user.yaml"))
	if err != nil || got {
		t.Fatalf("got %v err %v, want false nil", got, err)
	}
}
