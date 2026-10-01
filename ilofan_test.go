package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr(v float64) *float64 { return &v }

// healthy returns every default sensor 20 °C below its warning threshold and
// both fans at the given speed.
func healthy(fan float64) Thermal {
	var t Thermal
	for name, l := range defaultSensors {
		t.Temperatures = append(t.Temperatures, Temperature{
			Name: name, ReadingCelsius: ptr(l.Warning - 20), Status: Status{"Enabled", "OK"},
		})
	}
	t.Temperatures = append(t.Temperatures, Temperature{
		Name: "04-HD Max", ReadingCelsius: ptr(0), Status: Status{State: "Absent"},
	})
	for _, name := range fanNames {
		t.Fans = append(t.Fans, Fan{name, ptr(fan), "Percent", Status{"Enabled", "OK"}})
	}
	return t
}

func with(t Thermal, name string, f func(*Temperature)) Thermal {
	t.Temperatures = slices.Clone(t.Temperatures)
	for i := range t.Temperatures {
		if t.Temperatures[i].Name == name {
			f(&t.Temperatures[i])
		}
	}
	return t
}

func testConfig() *Config {
	c := defaultConfig()
	c.Host, c.Username, c.PasswordFile, c.InsecureTLS, c.Mode, c.HostKey = "ilo", "admin", "/dev/null", true, "control", "ssh-rsa AAAA"
	if err := c.validate(); err != nil {
		panic(err)
	}
	return &c
}

func TestAssess(t *testing.T) {
	cases := []struct {
		name   string
		in     Thermal
		level  Level
		target int
	}{
		{"healthy runs at minimum", healthy(9), Healthy, 9},
		{"hotter CPU follows its curve", with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(53) }), Healthy, 25},
		{"curve interpolates between points", with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(50.5) }), Healthy, 20},
		{"highest curve wins", with(with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(48) }),
			"01-Inlet Ambient", func(s *Temperature) { s.ReadingCelsius = ptr(31) }), Healthy, 25},
		{"warning adds 10 over the fastest fan", with(healthy(45), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(60) }), Warning, 55},
		{"warning uses the curve when it is higher", with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(60) }), Warning, 40},
		{"danger threshold", with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(64) }), Danger, 100},
		{"critical tightens limits", with(healthy(9), "02-CPU", func(s *Temperature) {
			s.ReadingCelsius, s.UpperThresholdCritical = ptr(55), ptr(62)
		}), Warning, 31},
		{"absent required sensor", with(healthy(9), "02-CPU", func(s *Temperature) { s.Status.State = "Absent" }), Danger, 100},
		{"null reading", with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = nil }), Danger, 100},
		{"zero reading", with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(0) }), Danger, 100},
		{"unhealthy required sensor", with(healthy(9), "02-CPU", func(s *Temperature) { s.Status.Health = "Warning" }), Danger, 100},
		{"missing required sensor", with(healthy(9), "02-CPU", func(s *Temperature) { s.Name = "renamed" }), Danger, 100},
		{"unhealthy unlisted sensor", func() Thermal {
			t := healthy(9)
			t.Temperatures = append(t.Temperatures, Temperature{Name: "30-Other", ReadingCelsius: ptr(30), Status: Status{"Enabled", "Critical"}})
			return t
		}(), Danger, 100},
		{"fan wrong units", func() Thermal { t := healthy(9); t.Fans[0].Units = "RPM"; return t }(), Danger, 100},
		{"fan unhealthy", func() Thermal { t := healthy(9); t.Fans[1].Status.Health = "Critical"; return t }(), Danger, 100},
		{"fan missing", func() Thermal { t := healthy(9); t.Fans = t.Fans[:1]; return t }(), Danger, 100},
	}
	c := testConfig()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := assess(tc.in, c.Sensors)
			if a.Level != tc.level {
				t.Fatalf("level %v, want %v (%v)", a.Level, tc.level, a.Reasons)
			}
			if got := thermalTarget(a, c, 0); got != tc.target {
				t.Fatalf("target %d, want %d", got, tc.target)
			}
		})
	}
}

func TestThermalRejectsBooleanReading(t *testing.T) {
	var th Thermal
	err := json.Unmarshal([]byte(`{"Fans":[{"FanName":"Fan 1","CurrentReading":true}]}`), &th)
	if err == nil {
		t.Fatal("boolean reading decoded as a number")
	}
}

func TestNextHysteresis(t *testing.T) {
	c := testConfig()
	cpu := func(v float64) Assessment {
		return assess(with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(v) }), c.Sensors)
	}
	s := State{Override: Auto, Commanded: -1}
	if got := s.next(cpu(40), c); got != 9 {
		t.Fatalf("unknown state writes target, got %d", got)
	}
	s.Commanded = 9
	if got := s.next(cpu(53), c); got != 25 {
		t.Fatalf("raise is immediate, got %d", got)
	}
	s.Commanded = 25
	for i := 1; i < c.LowerAfter; i++ {
		if got := s.next(cpu(40), c); got != -1 {
			t.Fatalf("lowered after %d samples", i)
		}
	}
	if got := s.next(cpu(40), c); got != 9 {
		t.Fatalf("lowers after %d calm samples, got %d", c.LowerAfter, got)
	}

	s = State{Override: Auto, Commanded: 25}
	for range c.LowerAfter * 2 {
		if got := s.next(cpu(52), c); got != -1 {
			t.Fatalf("lowered within hysteresis band to %d", got)
		}
	}
}

func TestNextOverrides(t *testing.T) {
	c := testConfig()
	calm := assess(healthy(9), c.Sensors)
	hot := assess(with(healthy(9), "02-CPU", func(s *Temperature) { s.ReadingCelsius = ptr(70) }), c.Sensors)

	s := State{Override: Manual, Manual: 30, Commanded: -1}
	if got := s.next(calm, c); got != 30 {
		t.Fatalf("manual sets floor, got %d", got)
	}
	s = State{Override: Manual, Manual: 30, Commanded: 30}
	if got := s.next(hot, c); got != 100 {
		t.Fatalf("danger beats manual, got %d", got)
	}
	s = State{Override: Released, Commanded: -1}
	if got := s.next(calm, c); got != -1 {
		t.Fatalf("released leaves fans alone, got %d", got)
	}
	if got := s.next(hot, c); got != 100 || s.Override != Auto {
		t.Fatalf("danger retakes control, got %d %v", got, s.Override)
	}
}

func TestPWM(t *testing.T) {
	for percent, want := range map[int]int{0: 0, 9: 23, 50: 128, 100: 255} {
		if got := pwm(percent); got != want {
			t.Errorf("pwm(%d) = %d, want %d", percent, got, want)
		}
	}
	for _, bad := range []string{"-1", "101", "1.5", "x", ""} {
		if _, err := parsePercent(bad); err == nil {
			t.Errorf("parsePercent(%q) accepted", bad)
		}
	}
}

func TestConverse(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	var got []string
	go func() {
		io.WriteString(outW, "User:admin logged-in\r\n"+prompt)
		lines := bufio.NewScanner(inR)
		lines.Split(func(data []byte, eof bool) (int, []byte, error) {
			if i := strings.IndexByte(string(data), '\r'); i >= 0 {
				return i + 1, data[:i], nil
			}
			return 0, nil, nil
		})
		for lines.Scan() {
			got = append(got, lines.Text())
			io.WriteString(outW, "\r\nstatus=0\r\n"+prompt)
		}
	}()
	if err := converse(inW, outR, lockCommands(9), time.Second, time.Second); err != nil {
		t.Fatal(err)
	}
	inW.Close()
	time.Sleep(10 * time.Millisecond)
	want := []string{"fan p 0 lock 23", "fan p 1 lock 23", "exit"}
	if !slices.Equal(got, want) {
		t.Fatalf("sent %q, want %q", got, want)
	}

	silent, _ := io.Pipe()
	if err := converse(io.Discard, silent, unlockCommands, 10*time.Millisecond, time.Second); err == nil {
		t.Fatal("missing prompt was treated as success")
	}
}

type fakeILO struct {
	mu       sync.Mutex
	cancel   context.CancelFunc // when set, cancels the daemon during the next read
	fan      float64
	clamp    float64 // when set, writes land on this speed instead
	readErr  error
	commands []string
}

func (f *fakeILO) Thermal(ctx context.Context) (Thermal, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancel != nil {
		f.cancel()
		return Thermal{}, ctx.Err()
	}
	return healthy(f.fan), f.readErr
}

func (f *fakeILO) Run(_ context.Context, cmds ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, cmds...)
	for _, c := range cmds {
		var ch, v int
		if n, _ := fmt.Sscanf(c, "fan p %d lock %d", &ch, &v); n == 2 {
			f.fan = float64(v) * 100 / 255
			if f.clamp > 0 {
				f.fan = f.clamp
			}
		}
	}
	return nil
}

func daemonWith(f *fakeILO, mode string) *Daemon {
	c := testConfig()
	c.Mode = mode
	d := newDaemon(c, f)
	d.settle = []time.Duration{0, 0}
	return d
}

func TestTickWritesAndVerifies(t *testing.T) {
	f := &fakeILO{fan: 40}
	d := daemonWith(f, "control")
	d.tick(context.Background())
	r := d.Report()
	if r.Commanded != 9 || r.Fault != "" || !slices.Equal(f.commands, lockCommands(9)) {
		t.Fatalf("report %+v, commands %q", r, f.commands)
	}
	f.commands = nil
	d.tick(context.Background())
	if len(f.commands) != 0 {
		t.Fatalf("rewrote a settled target: %q", f.commands)
	}
}

func TestTickReadbackMismatchIsFault(t *testing.T) {
	f := &fakeILO{fan: 40, clamp: 20}
	d := daemonWith(f, "control")
	d.tick(context.Background())
	if r := d.Report(); r.Fault == "" || r.Commanded != -1 || r.WriteErrors != 1 {
		t.Fatalf("unconfirmed write reported as success: %+v", r)
	}
}

func TestTickReadFailureGoesFull(t *testing.T) {
	f := &fakeILO{fan: 9, readErr: errors.New("timeout")}
	d := daemonWith(f, "control")
	d.tick(context.Background())
	if !slices.Equal(f.commands[:2], lockCommands(100)) {
		t.Fatalf("read failure did not request 100%%: %q", f.commands)
	}
}

func TestTickRewritesAfterDrift(t *testing.T) {
	f := &fakeILO{fan: 40}
	d := daemonWith(f, "control")
	d.tick(context.Background())
	f.fan, f.commands = 60, nil
	d.tick(context.Background())
	if !slices.Equal(f.commands, lockCommands(9)) {
		t.Fatalf("drift not corrected: %q", f.commands)
	}
}

func TestObserveNeverWrites(t *testing.T) {
	f := &fakeILO{fan: 9, readErr: errors.New("down")}
	d := daemonWith(f, "observe")
	d.tick(context.Background())
	if err := d.apply(context.Background(), request{override: Released}); err == nil {
		t.Fatal("observe mode accepted a release")
	}
	if len(f.commands) != 0 || d.Report().Target != 100 {
		t.Fatalf("observe mode wrote %q, report %+v", f.commands, d.Report())
	}
}

func TestRequestsAreSerialized(t *testing.T) {
	f := &fakeILO{fan: 9}
	d := daemonWith(f, "control")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.loop(ctx)
	var wg sync.WaitGroup
	for _, p := range []int{30, 50, 70} {
		wg.Go(func() { d.send(ctx, request{override: Manual, percent: p}) })
	}
	wg.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands)%2 != 0 {
		t.Fatalf("interleaved writes: %q", f.commands)
	}
	for i := 0; i < len(f.commands); i += 2 {
		if f.commands[i][len("fan p 0 lock"):] != f.commands[i+1][len("fan p 1 lock"):] {
			t.Fatalf("channels written with different values: %q", f.commands)
		}
	}
}

func TestPasswordFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password")
	i := &ILO{passwordFile: path, checkMode: true}
	read := func(content string, mode os.FileMode) (string, error) {
		os.WriteFile(path, []byte(content), 0o600)
		os.Chmod(path, mode)
		return i.password()
	}
	if got, err := read("hunter2\n", 0o400); err != nil || got != "hunter2" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := read("hunter2", 0o644); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("world-readable password file: %v", err)
	}
	if _, err := read("\n", 0o600); err == nil {
		t.Fatal("accepted an empty password")
	}
}

func TestLoadConfigCurves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	write := func(body string) (*Config, error) {
		os.WriteFile(path, []byte(`{"host":"ilo","username":"u","passwordFile":"x","insecureTLS":true,`+body+`}`), 0o600)
		return loadConfig(path)
	}
	c, err := write(`"curves":{"02-CPU":[{"temp":60,"percent":50},{"temp":20,"percent":10},{"temp":30,"percent":12},{"temp":40,"percent":20}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Curves) != 1 || len(c.Sensors) != len(defaultSensors) {
		t.Fatalf("configured curves must replace the defaults, got %v", c.Curves)
	}
	if got := curve(c.Curves["02-CPU"], 35); got != 16 {
		t.Fatalf("unsorted points were not sorted, curve(35) = %g", got)
	}
	if _, err := write(`"curves":{"99-Nope":[{"temp":20,"percent":10}]}`); err == nil {
		t.Fatal("accepted a curve for a sensor without thresholds")
	}
	if _, err := write(`"curves":{"02-CPU":[{"temp":20,"percent":110}]}`); err == nil {
		t.Fatal("accepted a percent above 100")
	}
}

func TestParseInterspersed(t *testing.T) {
	for _, args := range [][]string{{"30", "-socket", "s"}, {"-socket", "s", "30"}} {
		flags := flag.NewFlagSet("set", flag.ContinueOnError)
		socket := flags.String("socket", "default", "")
		got := parseInterspersed(flags, args)
		if *socket != "s" || !slices.Equal(got, []string{"30"}) {
			t.Fatalf("%q: socket %q, positional %q", args, *socket, got)
		}
	}
}

func TestShutdownDuringReadDoesNotRaise(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeILO{fan: 10, cancel: cancel}
	d := daemonWith(f, "control")
	d.tick(ctx)
	if len(f.commands) != 0 || d.Report().ReadErrors != 0 {
		t.Fatalf("cancelled read was treated as a sensor failure: %q", f.commands)
	}
}

func TestReleaseClearsCommanded(t *testing.T) {
	f := &fakeILO{fan: 40}
	d := daemonWith(f, "control")
	d.tick(context.Background())
	if err := d.apply(context.Background(), request{override: Released}); err != nil {
		t.Fatal(err)
	}
	if r := d.Report(); r.Commanded != -1 || r.Override != Released {
		t.Fatalf("after release: %+v", r)
	}
}

func TestWriteSetup(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "etc")
	f := setupFile{Host: "ilo", Username: "admin", PasswordFile: filepath.Join(dir, "password"), HostKey: "ssh-rsa AAAA", TLSFingerprint: "AA", Mode: "observe"}
	config := filepath.Join(dir, "config.json")
	if err := writeSetup(config, f, "hunter2"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{config: 0o644, f.PasswordFile: 0o600} {
		if info, _ := os.Stat(path); info.Mode().Perm() != want {
			t.Errorf("%s has mode %o, want %o", path, info.Mode().Perm(), want)
		}
	}
	data, _ := os.ReadFile(config)
	if strings.Contains(string(data), "hunter2") {
		t.Fatal("password written into the config")
	}
	c, err := loadConfig(config)
	if err != nil {
		t.Fatalf("written config does not load: %v", err)
	}
	if got, err := (&ILO{passwordFile: c.PasswordFile, checkMode: true}).password(); got != "hunter2" || err != nil {
		t.Fatalf("password file reads %q, %v", got, err)
	}
}

func TestSystemdCredentialWins(t *testing.T) {
	dir := t.TempDir()
	// System services get credentials with mode 0440.
	os.WriteFile(filepath.Join(dir, "password"), []byte("x\n"), 0o440)
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	path := filepath.Join(dir, "config.json")
	os.WriteFile(path, []byte(`{"host":"ilo","username":"u","insecureTLS":true}`), 0o600)
	c, err := loadConfig(path)
	if err != nil || c.PasswordFile != filepath.Join(dir, "password") {
		t.Fatalf("credential not used: %v %+v", err, c)
	}
	i, err := newILO(c)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := i.password(); got != "x" || err != nil {
		t.Fatalf("systemd credential rejected: %q, %v", got, err)
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
	if got := formatFingerprint([]byte{0x2d, 0xf7, 0x05}); got != "2D:F7:05" {
		t.Fatalf("fingerprint %q", got)
	}
}
