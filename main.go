package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const usage = `Usage:
  ilofan setup [-config FILE]  ask for the iLO details and write the config
  ilofan daemon [-config FILE] run the controller
  ilofan status [-json]        show sensors, fans and the active override
  ilofan set PERCENT           hold fans at PERCENT or more (temperatures may raise it)
  ilofan auto                  follow the fan curve
  ilofan release               return fans to iLO firmware control

FILE defaults to /etc/ilofan/config.json. Client commands accept
-socket PATH (default /run/ilofan/ilofan.sock).
`

const defaultConfigPath = "/etc/ilofan/config.json"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "ilofan:", err)
		os.Exit(1)
	}
}

func run(command string, args []string) error {
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	socket := flags.String("socket", "/run/ilofan/ilofan.sock", "control socket")

	switch command {
	case "setup":
		path := flags.String("config", defaultConfigPath, "config file to write")
		parseInterspersed(flags, args)
		return runSetup(*path, os.Stdin, os.Stdout)
	case "daemon":
		path := flags.String("config", defaultConfigPath, "config file")
		parseInterspersed(flags, args)
		cfg, err := loadConfig(*path)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		return runDaemon(ctx, cfg)
	case "status":
		asJSON := flags.Bool("json", false, "print JSON")
		parseInterspersed(flags, args)
		r, err := call(*socket, http.MethodGet, "/status")
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(r)
		}
		printReport(os.Stdout, r)
		return nil
	case "set":
		positional := parseInterspersed(flags, args)
		if len(positional) != 1 {
			return fmt.Errorf("set takes one percentage, got %q", positional)
		}
		if _, err := parsePercent(positional[0]); err != nil {
			return err
		}
		return post(*socket, "/set?percent="+positional[0])
	case "auto", "release":
		parseInterspersed(flags, args)
		return post(*socket, "/"+command)
	}
	fmt.Fprint(os.Stderr, usage)
	os.Exit(2)
	return nil
}

func post(socket, path string) error {
	r, err := call(socket, http.MethodPost, path)
	if err != nil {
		return err
	}
	printReport(os.Stdout, r)
	return nil
}

func call(socket, method, path string) (Report, error) {
	var r Report
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

func printReport(w io.Writer, r Report) {
	override := string(r.Override)
	if r.Override == Manual {
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

// parseInterspersed accepts flags before or after positional arguments, so
// "set 30 -socket X" and "set -socket X 30" both work.
func parseInterspersed(flags *flag.FlagSet, args []string) []string {
	var positional []string
	for {
		flags.Parse(args)
		if flags.NArg() == 0 {
			return positional
		}
		positional = append(positional, flags.Arg(0))
		args = flags.Args()[1:]
	}
}
