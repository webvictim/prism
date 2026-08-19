package tbot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/webvictim/prism/internal/tunnel"
)

func TestRuntime_PrepareWritesTbotYAML(t *testing.T) {
	dir := t.TempDir()
	r := &Runtime{
		Dir:       dir,
		LocalPort: 7331,
		Sidecar: &SidecarConfig{
			ProxyServer:        "odd-firefly.beams.sh:443",
			TokenName:          "prism-bot",
			RegistrationSecret: "secret123",
			RoleName:           "prism-bot-role",
			BotName:            "prism-bot",
		},
	}
	if err := r.Prepare(context.Background(), tunnel.AppInfo{
		AppName:   "agile-grid-72ae",
		LocalPort: 7331,
	}); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "tbot.yaml"))
	if err != nil {
		t.Fatalf("read tbot.yaml: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"app_name: agile-grid-72ae",
		"listen: tcp://127.0.0.1:7331",
		"registration_secret: secret123",
		"token: prism-bot",
		"proxy_server: odd-firefly.beams.sh:443",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("tbot.yaml missing %q\nfull body:\n%s", want, got)
		}
	}

	// Second Prepare with a different app rewrites the file (no leftover
	// content from the first render).
	if err := r.Prepare(context.Background(), tunnel.AppInfo{
		AppName:   "different-beam-1234",
		LocalPort: 7331,
	}); err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	body, _ = os.ReadFile(filepath.Join(dir, "tbot.yaml"))
	got = string(body)
	if strings.Contains(got, "agile-grid-72ae") {
		t.Error("second Prepare should have overwritten the first app_name, but it's still there")
	}
	if !strings.Contains(got, "app_name: different-beam-1234") {
		t.Errorf("second Prepare did not write new app_name; got:\n%s", got)
	}
}

func TestValidate_ReportsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	problems := Validate(dir)
	// Fresh empty dir: all three checks should fail.
	if len(problems) != 3 {
		t.Errorf("expected 3 problems for empty dir, got %d: %v", len(problems), problems)
	}

	// Add the sidecar + tbot.yaml + storage dir.
	if err := SaveSidecar(dir, &SidecarConfig{ProxyServer: "x", TokenName: "y", RegistrationSecret: "z"}); err != nil {
		t.Fatalf("SaveSidecar: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tbot.yaml"), []byte("hi"), 0o600); err != nil {
		t.Fatalf("write tbot.yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "storage"), 0o700); err != nil {
		t.Fatalf("mkdir storage: %v", err)
	}
	if got := Validate(dir); len(got) != 0 {
		t.Errorf("expected no problems for fully-populated dir, got: %v", got)
	}
}

func TestSidecar_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	in := &SidecarConfig{
		ProxyServer:        "odd-firefly.beams.sh:443",
		TokenName:          "prism-bot",
		RegistrationSecret: "supersecret",
		RoleName:           "prism-bot-role",
		BotName:            "prism-bot",
	}
	if err := SaveSidecar(dir, in); err != nil {
		t.Fatalf("SaveSidecar: %v", err)
	}
	out, err := LoadSidecar(dir)
	if err != nil {
		t.Fatalf("LoadSidecar: %v", err)
	}
	if *out != *in {
		t.Errorf("roundtrip differs:\n  in:  %+v\n  out: %+v", in, out)
	}
}

func TestSidecar_MissingReturnsHelpfulError(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadSidecar(dir)
	if err == nil {
		t.Fatal("expected error for missing sidecar")
	}
	if !strings.Contains(err.Error(), "prism tbot bootstrap") {
		t.Errorf("error should point users at bootstrap; got: %v", err)
	}
}
