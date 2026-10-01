package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
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

	if fileExists(configPath) {
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
	fingerprint, err := tlsFingerprint(host)
	if err != nil {
		return fmt.Errorf("HTTPS: %w", err)
	}
	hostKey, err := sshHostKey(host)
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

	c := defaultConfig()
	c.Host, c.Username, c.PasswordFile, c.HostKey, c.TLSFingerprint = f.Host, f.Username, tmp.Name(), f.HostKey, f.TLSFingerprint
	ilo, err := newILO(&c)
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
	a := assess(t, c.Sensors)
	fmt.Fprintf(out, "ok, %d sensors, fans %v%%\n", len(temperatures(t)), a.Fans)
	if a.Level == Danger {
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

func tlsFingerprint(host string) (string, error) {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, "443"), &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return "", err
	}
	defer conn.Close()
	sum := sha256.Sum256(conn.ConnectionState().PeerCertificates[0].Raw)
	return formatFingerprint(sum[:]), nil
}

func formatFingerprint(sum []byte) string {
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// sshHostKey completes the key exchange without logging in.
func sshHostKey(host string) (ssh.PublicKey, error) {
	var key ssh.PublicKey
	capture := func(_ string, _ net.Addr, k ssh.PublicKey) error {
		key = k
		return nil
	}
	client, err := ssh.Dial("tcp", net.JoinHostPort(host, "22"), sshConfig("ilofan-setup", nil, capture))
	if client != nil {
		client.Close()
	}
	if key == nil {
		return nil, err
	}
	return key, nil
}
