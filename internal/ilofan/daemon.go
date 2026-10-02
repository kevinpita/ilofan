package ilofan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

type Report struct {
	Mode        string             `json:"mode"`
	Override    Override           `json:"override"`
	Manual      int                `json:"manual,omitempty"`
	Level       string             `json:"level"`
	Reasons     []string           `json:"reasons"`
	Target      int                `json:"target"`
	Commanded   int                `json:"commanded"`
	Fans        []float64          `json:"fans"`
	Temps       map[string]float64 `json:"temperatures"`
	LastRead    time.Time          `json:"lastRead"`
	LastWrite   time.Time          `json:"lastWrite"`
	Fault       string             `json:"fault,omitempty"`
	ReadErrors  int                `json:"readErrors"`
	WriteErrors int                `json:"writeErrors"`
}

type request struct {
	override Override
	percent  int
	reply    chan error
}

type Daemon struct {
	cfg      *Config
	dev      Device
	settle   []time.Duration
	requests chan request

	state State

	mu     sync.Mutex
	report Report
}

func newDaemon(cfg *Config, dev Device) *Daemon {
	return &Daemon{
		cfg:      cfg,
		dev:      dev,
		settle:   []time.Duration{5 * time.Second, 3 * time.Second},
		requests: make(chan request),
		state:    State{Override: Auto, Commanded: -1},
		report:   Report{Mode: cfg.Mode, Override: Auto, Target: -1, Commanded: -1},
	}
}

func (d *Daemon) Report() Report {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.report
}

func (d *Daemon) update(f func(r *Report)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(&d.report)
}

// loop is the only writer of fan speeds. CLI requests are applied between
// ticks, so writes never overlap.
func (d *Daemon) loop(ctx context.Context) {
	ticker := time.NewTicker(d.cfg.poll())
	defer ticker.Stop()
	d.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.tick(ctx)
		case r := <-d.requests:
			r.reply <- d.apply(ctx, r)
		}
	}
}

func (d *Daemon) apply(ctx context.Context, r request) error {
	if d.cfg.Mode != "control" {
		return errors.New("daemon is in observe mode")
	}
	if r.override == Released {
		if err := d.dev.Run(ctx, unlockCommands...); err != nil {
			return err
		}
		slog.Info("released fans to firmware control")
	}
	d.state.Override, d.state.Manual, d.state.Commanded = r.override, r.percent, -1
	d.update(func(r *Report) { r.Commanded = -1 })
	d.tick(ctx)
	if f := d.Report().Fault; f != "" {
		return errors.New(f)
	}
	return nil
}

func (d *Daemon) tick(ctx context.Context) {
	t, err := d.dev.Thermal(ctx)
	// A read cut short by shutdown says nothing about the hardware.
	if ctx.Err() != nil {
		return
	}
	var a Assessment
	if err != nil {
		slog.Error("thermal read failed", "err", err)
		a = Assessment{Level: Danger, Reasons: []string{"thermal read failed: " + err.Error()}}
	} else {
		a = Assess(t, d.cfg.Sensors)
	}
	if d.state.Commanded >= 0 && len(a.Fans) == 2 && !d.settled(a.Fans, d.state.Commanded) {
		slog.Warn("fans drifted from commanded speed", "commanded", d.state.Commanded, "fans", a.Fans)
		d.state.Commanded = -1
	}
	target := d.state.next(a, d.cfg)

	d.update(func(r *Report) {
		if err != nil {
			r.ReadErrors++
		} else {
			r.LastRead = time.Now()
			r.Temps = t.Readings()
		}
		r.Level, r.Reasons, r.Fans = a.Level.String(), a.Reasons, a.Fans
		r.Override, r.Manual = d.state.Override, d.state.Manual
		if target >= 0 {
			r.Target = target
		}
	})
	if target < 0 || d.cfg.Mode != "control" {
		return
	}

	slog.Info("setting fans", "percent", target, "pwm", pwm(target), "level", a.Level, "reasons", a.Reasons)
	fans, err := d.write(ctx, target)
	d.update(func(r *Report) {
		if len(fans) > 0 {
			r.Fans = fans
		}
		if err != nil {
			r.Fault = err.Error()
			r.WriteErrors++
			return
		}
		r.Fault = ""
		r.LastWrite = time.Now()
		r.Commanded = target
	})
	if err != nil {
		slog.Error("fan write failed", "percent", target, "err", err)
		d.state.Commanded = -1
		return
	}
	d.state.Commanded = target
}

// write locks both channels and confirms the result through Redfish.
func (d *Daemon) write(ctx context.Context, percent int) ([]float64, error) {
	if err := d.dev.Run(ctx, lockCommands(percent)...); err != nil {
		return nil, err
	}
	var fans []float64
	for _, wait := range d.settle {
		select {
		case <-ctx.Done():
			return fans, ctx.Err()
		case <-time.After(wait):
		}
		t, err := d.dev.Thermal(ctx)
		if err != nil {
			continue
		}
		fans = Assess(t, nil).Fans
		if len(fans) == 2 && d.settled(fans, percent) {
			return fans, nil
		}
	}
	return fans, fmt.Errorf("requested %d%%, fans read %v", percent, fans)
}

func (d *Daemon) settled(fans []float64, percent int) bool {
	for _, f := range fans {
		if math.Abs(f-float64(percent)) > d.cfg.Tolerance {
			return false
		}
	}
	return true
}

func (d *Daemon) shutdown() {
	if d.cfg.Mode != "control" || !d.cfg.ReleaseOnStop || d.state.Override == Released {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	if err := d.dev.Run(ctx, unlockCommands...); err != nil {
		slog.Error("release on stop failed, fans keep their last lock", "err", err)
		return
	}
	time.Sleep(d.settle[0])
	t, err := d.dev.Thermal(ctx)
	if err != nil {
		slog.Warn("released fans, readback failed", "err", err)
		return
	}
	slog.Info("released fans to firmware control", "fans", Assess(t, nil).Fans)
}

func (d *Daemon) send(ctx context.Context, r request) error {
	r.reply = make(chan error, 1)
	select {
	case d.requests <- r:
	case <-ctx.Done():
		return ctx.Err()
	}
	return <-r.reply
}

func (d *Daemon) controlHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(d.Report())
	})
	mux.HandleFunc("POST /auto", d.handle(func(*http.Request) (request, error) {
		return request{override: Auto}, nil
	}))
	mux.HandleFunc("POST /release", d.handle(func(*http.Request) (request, error) {
		return request{override: Released}, nil
	}))
	mux.HandleFunc("POST /set", d.handle(func(r *http.Request) (request, error) {
		p, err := ParsePercent(r.URL.Query().Get("percent"))
		return request{override: Manual, percent: p}, err
	}))
	return mux
}

func (d *Daemon) handle(parse func(*http.Request) (request, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, err := parse(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.send(r.Context(), req); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		json.NewEncoder(w).Encode(d.Report())
	}
}

// ParsePercent accepts an integer fan percentage from 0 through 100.
func ParsePercent(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 0 || p > 100 {
		return 0, fmt.Errorf("percent must be an integer from 0 to 100, got %q", s)
	}
	return p, nil
}

func (d *Daemon) metrics(w http.ResponseWriter, _ *http.Request) {
	r := d.Report()
	fmt.Fprintf(w, "ilofan_level %d\n", map[string]int{"healthy": 0, "warning": 1, "danger": 2}[r.Level])
	fmt.Fprintf(w, "ilofan_target_percent %d\n", r.Target)
	fmt.Fprintf(w, "ilofan_commanded_percent %d\n", r.Commanded)
	for i, f := range r.Fans {
		fmt.Fprintf(w, "ilofan_fan_percent{fan=%q} %g\n", fanNames[i], f)
	}
	names := make([]string, 0, len(r.Temps))
	for n := range r.Temps {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "ilofan_temperature_celsius{sensor=%q} %g\n", n, r.Temps[n])
	}
	fmt.Fprintf(w, "ilofan_override{override=%q} 1\n", r.Override)
	fmt.Fprintf(w, "ilofan_mode{mode=%q} 1\n", r.Mode)
	fmt.Fprintf(w, "ilofan_control_fault %d\n", boolInt(r.Fault != ""))
	fmt.Fprintf(w, "ilofan_last_read_timestamp_seconds %d\n", unix(r.LastRead))
	fmt.Fprintf(w, "ilofan_last_write_timestamp_seconds %d\n", unix(r.LastWrite))
	fmt.Fprintf(w, "ilofan_read_errors_total %d\n", r.ReadErrors)
	fmt.Fprintf(w, "ilofan_write_errors_total %d\n", r.WriteErrors)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// RunDaemon runs the controller and its API until the context is cancelled.
func RunDaemon(ctx context.Context, cfg *Config) error {
	dev, err := NewILO(cfg)
	if err != nil {
		return err
	}
	d := newDaemon(cfg, dev)

	os.Remove(cfg.Socket)
	sock, err := net.Listen("unix", cfg.Socket)
	if err != nil {
		return err
	}
	// The socket's group controls who may run the CLI.
	if err := os.Chmod(cfg.Socket, 0o660); err != nil {
		return err
	}
	go http.Serve(sock, d.controlHandler())
	if cfg.MetricsAddress != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /metrics", d.metrics)
		go func() {
			if err := http.ListenAndServe(cfg.MetricsAddress, mux); err != nil {
				slog.Error("metrics listener stopped", "err", err)
			}
		}()
	}

	slog.Info("starting", "host", cfg.Host, "mode", cfg.Mode)
	d.loop(ctx)
	sock.Close()
	d.shutdown()
	return nil
}
