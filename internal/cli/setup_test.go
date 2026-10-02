package cli

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kevinpita/ilofan/internal/ilofan"
)

func TestWriteSetup(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := filepath.Join(t.TempDir(), "etc")
	f := setupFile{Host: "ilo", Username: "admin", PasswordFile: filepath.Join(dir, "password"), HostKey: "ssh-rsa AAAA", TLSFingerprint: "AA", Mode: "observe"}
	config := filepath.Join(dir, "config.json")
	if err := writeSetup(config, f, "hunter2"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{config: 0o644, f.PasswordFile: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Fatal("password written into the config")
	}
	c, err := ilofan.LoadConfig(config)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if got, err := os.ReadFile(c.PasswordFile); string(got) != "hunter2\n" || err != nil {
		t.Fatalf("password file reads %q, %v", got, err)
	}
}

func TestPrompterDefaults(t *testing.T) {
	p := prompter{bufio.NewReader(strings.NewReader("\n192.168.1.148\n")), io.Discard}
	if got, _ := p.ask("user", "Administrator"); got != "Administrator" {
		t.Fatalf("empty answer gave %q", got)
	}
	if got, _ := p.ask("host", ""); got != "192.168.1.148" {
		t.Fatalf("got %q", got)
	}
}
