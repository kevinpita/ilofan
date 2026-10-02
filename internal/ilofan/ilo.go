package ilofan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// TLSFingerprint reads the HTTPS certificate fingerprint without verifying it.
// The caller must confirm this fingerprint before it is trusted.
func TLSFingerprint(host string) (string, error) {
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

// SSHHostKey reads the SSH host key without logging in or verifying it.
// The caller must confirm this key before it is trusted.
func SSHHostKey(host string) (ssh.PublicKey, error) {
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

const prompt = "</>hpiLO->"

// Device is the iLO as seen by the daemon.
type Device interface {
	Thermal(ctx context.Context) (Thermal, error)
	Run(ctx context.Context, commands ...string) error
}

func pwm(percent int) int { return int(math.Round(float64(percent) * 255 / 100)) }

func lockCommands(percent int) []string {
	return []string{
		fmt.Sprintf("fan p 0 lock %d", pwm(percent)),
		fmt.Sprintf("fan p 1 lock %d", pwm(percent)),
	}
}

var unlockCommands = []string{"fan p 0 unlock", "fan p 1 unlock"}

type ILO struct {
	host         string
	username     string
	passwordFile string
	// checkMode rejects password files that other users can read.
	checkMode bool
	hostKey   ssh.PublicKey
	http      *http.Client
}

// NewILO creates a client with the configured credentials and pinned keys.
func NewILO(c *Config) (*ILO, error) {
	i := &ILO{host: c.Host, username: c.Username, passwordFile: c.PasswordFile, checkMode: !c.passwordFromSystemd}
	if c.HostKey != "" {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
		if err != nil {
			return nil, fmt.Errorf("hostKey: %w", err)
		}
		i.hostKey = key
	}
	// The iLO certificate is self-signed and expired, so chain and expiry
	// checks are replaced by pinning the leaf certificate.
	tlsConfig := &tls.Config{InsecureSkipVerify: true}
	if !c.InsecureTLS {
		pin, err := hex.DecodeString(strings.NewReplacer(":", "", "sha256/", "").Replace(strings.ToLower(c.TLSFingerprint)))
		if err != nil || len(pin) != sha256.Size {
			return nil, errors.New("tlsFingerprint must be a SHA-256 hex fingerprint")
		}
		tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			sum := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if !bytes.Equal(sum[:], pin) {
				return errors.New("iLO TLS certificate does not match tlsFingerprint")
			}
			return nil
		}
	}
	i.http = &http.Client{Timeout: 12 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsConfig}}
	return i, nil
}

// password is read on every use so a rotated secret applies without a
// restart. Errors never include the file contents.
func (i *ILO) password() (string, error) {
	info, err := os.Stat(i.passwordFile)
	if err != nil {
		return "", err
	}
	if i.checkMode && info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must not be readable by group or others (mode %o)", i.passwordFile, info.Mode().Perm())
	}
	data, err := os.ReadFile(i.passwordFile)
	if err != nil {
		return "", err
	}
	password := strings.TrimRight(string(data), "\r\n")
	if password == "" {
		return "", fmt.Errorf("%s is empty", i.passwordFile)
	}
	return password, nil
}

func (i *ILO) Thermal(ctx context.Context) (Thermal, error) {
	var t Thermal
	password, err := i.password()
	if err != nil {
		return t, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+i.host+"/redfish/v1/Chassis/1/Thermal/", nil)
	if err != nil {
		return t, err
	}
	req.SetBasicAuth(i.username, password)
	resp, err := i.http.Do(req)
	if err != nil {
		return t, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return t, fmt.Errorf("redfish: %s", resp.Status)
	}
	return t, json.NewDecoder(resp.Body).Decode(&t)
}

// Run sends commands in one interactive shell. iLO ignores commands passed
// as an SSH exec request, and exit status says nothing about success, so
// callers verify the effect through Redfish.
func (i *ILO) Run(ctx context.Context, commands ...string) error {
	if i.hostKey == nil {
		return errors.New("hostKey is not configured")
	}
	password, err := i.password()
	if err != nil {
		return err
	}
	answer := func(_, _ string, questions []string, _ []bool) ([]string, error) {
		answers := make([]string, len(questions))
		for n := range answers {
			answers[n] = password
		}
		return answers, nil
	}
	auth := []ssh.AuthMethod{ssh.Password(password), ssh.KeyboardInteractive(answer)}
	client, err := ssh.Dial("tcp", net.JoinHostPort(i.host, "22"), sshConfig(i.username, auth, ssh.FixedHostKey(i.hostKey)))
	if err != nil {
		return err
	}
	defer client.Close()
	stop := context.AfterFunc(ctx, func() { client.Close() })
	defer stop()

	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	if err := session.RequestPty("vt100", 80, 200, ssh.TerminalModes{}); err != nil {
		return err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		return err
	}
	if err := session.Shell(); err != nil {
		return err
	}
	return converse(stdin, stdout, commands, 18*time.Second, 8*time.Second)
}

func sshConfig(user string, auth []ssh.AuthMethod, hostKey ssh.HostKeyCallback) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User:              user,
		Auth:              auth,
		HostKeyCallback:   hostKey,
		HostKeyAlgorithms: []string{ssh.KeyAlgoRSA},
		Timeout:           10 * time.Second,
		// iLO 4 only offers these legacy algorithms.
		KeyExchanges: []string{"diffie-hellman-group14-sha1"},
		Ciphers:      []string{"aes256-ctr", "aes128-ctr"},
		MACs:         []string{"hmac-sha2-256", "hmac-sha1"},
	}
}

// converse waits for the iLO prompt before and after each command.
func converse(w io.Writer, r io.Reader, commands []string, login, step time.Duration) error {
	chunks := make(chan []byte)
	go func() {
		defer close(chunks)
		for {
			buf := make([]byte, 4096)
			n, err := r.Read(buf)
			if n > 0 {
				chunks <- buf[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	var pending []byte
	wait := func(timeout time.Duration, after string) error {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		for {
			if i := bytes.Index(pending, []byte(prompt)); i >= 0 {
				pending = pending[i+len(prompt):]
				return nil
			}
			select {
			case chunk, ok := <-chunks:
				if !ok {
					return fmt.Errorf("iLO closed the session %s", after)
				}
				pending = append(pending, chunk...)
			case <-timer.C:
				return fmt.Errorf("no iLO prompt %s", after)
			}
		}
	}
	if err := wait(login, "after login"); err != nil {
		return err
	}
	for _, cmd := range commands {
		if _, err := io.WriteString(w, cmd+"\r"); err != nil {
			return err
		}
		if err := wait(step, "after "+cmd); err != nil {
			return err
		}
	}
	io.WriteString(w, "exit\r")
	return nil
}
