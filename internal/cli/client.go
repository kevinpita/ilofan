package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kevinpita/ilofan/internal/ilofan"
)

func post(socket, path string, w io.Writer) error {
	r, err := call(socket, http.MethodPost, path)
	if err != nil {
		return err
	}
	printReport(w, r)
	return nil
}

func call(socket, method, path string) (ilofan.Report, error) {
	var r ilofan.Report
	client := http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, _ := http.NewRequest(method, "http://ilofan"+path, nil)
	resp, err := client.Do(req)
	if err != nil {
		return r, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(resp.Body)
		return r, fmt.Errorf("%s", strings.TrimSpace(string(msg)))
	}
	return r, json.NewDecoder(resp.Body).Decode(&r)
}

func printReport(w io.Writer, r ilofan.Report) {
	override := string(r.Override)
	if r.Override == ilofan.Manual {
		override = fmt.Sprintf("manual %d%%", r.Manual)
	}
	fmt.Fprintf(w, "mode      %s, %s\n", r.Mode, override)
	fmt.Fprintf(w, "level     %s\n", r.Level)
	for _, reason := range r.Reasons {
		fmt.Fprintf(w, "          %s\n", reason)
	}
	fmt.Fprintf(w, "fans      %v%% (target %s, commanded %s)\n", r.Fans, percent(r.Target), percent(r.Commanded))
	fmt.Fprintf(w, "read      %s, %d errors\n", since(r.LastRead), r.ReadErrors)
	fmt.Fprintf(w, "write     %s, %d errors\n", since(r.LastWrite), r.WriteErrors)
	if r.Fault != "" {
		fmt.Fprintf(w, "FAULT     %s\n", r.Fault)
	}
	names := make([]string, 0, len(r.Temps))
	for name := range r.Temps {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "  %-18s %5.1f °C\n", name, r.Temps[name])
	}
}

func percent(p int) string {
	if p < 0 {
		return "unknown"
	}
	return fmt.Sprintf("%d%%", p)
}

func since(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return time.Since(t).Round(time.Second).String() + " ago"
}
