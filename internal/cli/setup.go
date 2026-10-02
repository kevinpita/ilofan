package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"

	"github.com/kevinpita/ilofan/internal/ilofan"
)

// setupFile is what setup writes. Everything else keeps its default.
type setupFile struct {
	Host           string `json:"host"`
	Username       string `json:"username"`
	PasswordFile   string `json:"passwordFile"`
	HostKey        string `json:"hostKey"`
	TLSFingerprint string `json:"tlsFingerprint"`
	Mode           string `json:"mode"`
}

type prompter struct {
	in  *bufio.Reader
	out io.Writer
}

func (p prompter) ask(question, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(p.out, "%s [%s]: ", question, fallback)
	} else {
		fmt.Fprintf(p.out, "%s: ", question)
	}
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	if line = strings.TrimSpace(line); line == "" {
		return fallback, nil
	}
	return line, nil
}

func (p prompter) secret(question string) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return p.ask(question, "")
	}
	fmt.Fprintf(p.out, "%s: ", question)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(p.out)
	return string(b), err
}

func (p prompter) confirm(question string) (bool, error) {
	answer, err := p.ask(question+" [y/N]", "")
	return strings.EqualFold(answer, "y") || strings.EqualFold(answer, "yes"), err
}

// runSetup asks for the iLO details, pins its keys, tests both logins without
// touching the fans, and writes the config and password files.
func runSetup(configPath string, in io.Reader, out io.Writer) error {
	p := prompter{bufio.NewReader(in), out}
	dir := filepath.Dir(configPath)
	passwordPath := filepath.Join(dir, "password")

	if _, err := os.Stat(configPath); err == nil {
		ok, err := p.confirm(configPath + " exists. Overwrite?")
		if err != nil || !ok {
			return errors.New("setup cancelled")
		}
	}
	host, err := p.ask("iLO address", "")
	if err != nil || host == "" {
		return errors.New("iLO address is required")
	}
	user, err := p.ask("iLO username", "Administrator")
	if err != nil {
		return err
	}
	password, err := p.secret("iLO password")
	if err != nil || password == "" {
		return errors.New("password is required")
	}

	fmt.Fprintf(out, "\nReading the keys of %s...\n", host)
	fingerprint, err := ilofan.TLSFingerprint(host)
	if err != nil {
		return fmt.Errorf("HTTPS: %w", err)
	}
	hostKey, err := ilofan.SSHHostKey(host)
	if err != nil {
		return fmt.Errorf("SSH: %w", err)
	}
	fmt.Fprintf(out, "  HTTPS certificate  SHA256 %s\n", fingerprint)
	fmt.Fprintf(out, "  SSH host key       %s\n", ssh.FingerprintSHA256(hostKey))
	fmt.Fprintln(out, "Compare them with the iLO web interface or a connection you already trust.")
	if ok, err := p.confirm("Trust these keys?"); err != nil || !ok {
		return errors.New("setup cancelled")
	}

	f := setupFile{
		Host:           host,
		Username:       user,
		PasswordFile:   passwordPath,
		HostKey:        strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey))),
		TLSFingerprint: fingerprint,
	}
	if err := testLogin(f, password, out); err != nil {
		return err
	}

	mode, err := p.ask("Mode: observe only watches, control sets the fans", "observe")
	if err != nil {
		return err
	}
	if mode != "observe" && mode != "control" {
		return fmt.Errorf("unknown mode %q", mode)
	}
	f.Mode = mode

	if err := writeSetup(configPath, f, password); err != nil {
		return err
	}
	fmt.Fprintf(out, "\nWrote %s and %s (readable only by its owner).\n", configPath, passwordPath)
	fmt.Fprintln(out, "Start the daemon with your service manager, for example: sudo systemctl enable --now ilofan")
	return nil
}

func testLogin(f setupFile, password string, out io.Writer) error {
	tmp, err := os.CreateTemp("", "ilofan-password-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(password); err != nil {
		return err
	}
	tmp.Close()

	c := ilofan.DefaultConfig()
	c.Host, c.Username, c.PasswordFile, c.HostKey, c.TLSFingerprint = f.Host, f.Username, tmp.Name(), f.HostKey, f.TLSFingerprint
	ilo, err := ilofan.NewILO(&c)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	fmt.Fprint(out, "Testing Redfish login... ")
	t, err := ilo.Thermal(ctx)
	if err != nil {
		return err
	}
	a := ilofan.Assess(t, c.Sensors)
	fmt.Fprintf(out, "ok, %d sensors, fans %v%%\n", len(t.Readings()), a.Fans)
	if a.Level == ilofan.Danger {
		fmt.Fprintf(out, "  The default sensor list does not match this machine: %s\n", strings.Join(a.Reasons, "; "))
		fmt.Fprintln(out, "  Adjust sensors in the config before using control mode.")
	}

	fmt.Fprint(out, "Testing SSH login... ")
	if err := ilo.Run(ctx); err != nil {
		return err
	}
	fmt.Fprintln(out, "ok")
	return nil
}

func writeSetup(configPath string, f setupFile, password string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}
	if err := writePrivate(f.PasswordFile, []byte(password+"\n")); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	// The config holds no secrets and must be readable by the service user.
	return writeMode(configPath, append(data, '\n'), 0o644)
}

func writePrivate(path string, data []byte) error { return writeMode(path, data, 0o600) }

// writeMode also fixes the mode of a file that already exists.
func writeMode(path string, data []byte, mode os.FileMode) error {
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
