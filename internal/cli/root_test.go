package cli

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kevinpita/ilofan/internal/ilofan"
)

func TestCommandHelp(t *testing.T) {
	for _, name := range []string{"", "setup", "daemon", "status", "set", "auto", "release"} {
		t.Run(name, func(t *testing.T) {
			cmd := NewCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			args := []string{"--help"}
			if name != "" {
				args = append([]string{name}, args...)
			}
			cmd.SetArgs(args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Usage:") || !strings.Contains(out.String(), "ilofan") {
				t.Fatalf("missing help: %s", &out)
			}
		})
	}
}

func TestClientCommands(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "control.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	type request struct{ method, path string }
	requests := make(chan request, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- request{r.Method, r.URL.RequestURI()}
		json.NewEncoder(w).Encode(ilofan.Report{Override: ilofan.Auto})
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })

	for _, tc := range []struct {
		name   string
		args   []string
		method string
		path   string
		asJSON bool
	}{
		{"set flag after", []string{"set", "30", "--socket", socket}, "POST", "/set?percent=30", false},
		{"set flag before", []string{"set", "--socket", socket, "30"}, "POST", "/set?percent=30", false},
		{"auto", []string{"auto", "--socket", socket}, "POST", "/auto", false},
		{"release", []string{"release", "--socket", socket}, "POST", "/release", false},
		{"status", []string{"status", "--socket", socket}, "GET", "/status", false},
		{"status json", []string{"status", "--socket", socket, "--json"}, "GET", "/status", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewCommand()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got := <-requests; got != (request{tc.method, tc.path}) {
				t.Fatalf("request = %+v", got)
			}
			if tc.asJSON {
				var report ilofan.Report
				if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.Override != ilofan.Auto {
					t.Fatalf("invalid JSON report %q: %v", &out, err)
				}
			} else if !strings.Contains(out.String(), "mode      ") {
				t.Fatalf("missing text report: %s", &out)
			}
		})
	}
}
