package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

type Level int

const (
	Healthy Level = iota
	Warning
	Danger
)

func (l Level) String() string {
	return [...]string{"healthy", "warning", "danger"}[l]
}

// Thermal is the subset of /redfish/v1/Chassis/1/Thermal/ that iLO 4 returns.
// Pointers distinguish missing or null readings from zero.
type Thermal struct {
	Temperatures []Temperature
	Fans         []Fan
}

type Temperature struct {
	Name                   string
	ReadingCelsius         *float64
	UpperThresholdCritical *float64
	Status                 Status
}

type Fan struct {
	FanName        string
	CurrentReading *float64
	Units          string
	Status         Status
}

type Status struct {
	State  string
	Health string
}

type Limit struct {
	Warning float64 `json:"warning"`
	Danger  float64 `json:"danger"`
}

var fanNames = []string{"Fan 1", "Fan 2"}

type Assessment struct {
	Level   Level
	Reasons []string
	// Temps holds the readings of required sensors that passed validation.
	Temps map[string]float64
	Fans  []float64
}

func (a *Assessment) raise(l Level, format string, args ...any) {
	a.Level = max(a.Level, l)
	a.Reasons = append(a.Reasons, fmt.Sprintf(format, args...))
}

func assess(t Thermal, limits map[string]Limit) Assessment {
	a := Assessment{Temps: map[string]float64{}}

	temps := map[string]int{}
	for i, s := range t.Temperatures {
		temps[s.Name] = i
		if s.Status.State == "Enabled" && s.Status.Health != "OK" {
			a.raise(Danger, "%s: health %q", s.Name, s.Status.Health)
		}
	}

	names := make([]string, 0, len(limits))
	for name := range limits {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		i, ok := temps[name]
		if !ok {
			a.raise(Danger, "%s: missing", name)
			continue
		}
		s := t.Temperatures[i]
		if s.Status.State != "Enabled" || s.ReadingCelsius == nil || *s.ReadingCelsius <= 0 {
			a.raise(Danger, "%s: unavailable", name)
			continue
		}
		v := *s.ReadingCelsius
		warn, high := limits[name].Warning, limits[name].Danger
		if c := s.UpperThresholdCritical; c != nil && *c > 0 {
			high = min(high, *c-5)
			warn = min(warn, high-3)
		}
		a.Temps[name] = v
		switch {
		case v >= high:
			a.raise(Danger, "%s: %g °C >= %g", name, v, high)
		case v >= warn:
			a.raise(Warning, "%s: %g °C >= %g", name, v, warn)
		}
	}

	for _, name := range fanNames {
		i := slices.IndexFunc(t.Fans, func(f Fan) bool { return f.FanName == name })
		if i < 0 {
			a.raise(Danger, "%s: missing", name)
			continue
		}
		f := t.Fans[i]
		if f.Status.State != "Enabled" || f.Status.Health != "OK" || f.Units != "Percent" ||
			f.CurrentReading == nil || *f.CurrentReading < 0 || *f.CurrentReading > 100 {
			a.raise(Danger, "%s: invalid reading", name)
			continue
		}
		a.Fans = append(a.Fans, *f.CurrentReading)
	}
	return a
}

type CurvePoint struct {
	Temp    float64 `json:"temp"`
	Percent float64 `json:"percent"`
}

// curve interpolates linearly between points sorted by ascending temperature
// and holds the end values outside them.
func curve(points []CurvePoint, temp float64) float64 {
	if temp <= points[0].Temp {
		return points[0].Percent
	}
	for i := 1; i < len(points); i++ {
		lo, hi := points[i-1], points[i]
		if temp < hi.Temp {
			return lo.Percent + (temp-lo.Temp)/(hi.Temp-lo.Temp)*(hi.Percent-lo.Percent)
		}
	}
	return points[len(points)-1].Percent
}

type Override string

const (
	Auto     Override = "auto"
	Manual   Override = "manual"
	Released Override = "released"
)

// State is owned by the daemon loop. Commanded is the last verified write,
// or -1 when the fan state is unknown and the next target must be written.
type State struct {
	Override  Override
	Manual    int
	Commanded int
	calm      int
}

// thermalTarget is the highest value of all sensor curves. extra is added to
// every temperature, which lets lowering demand a safety margin.
func thermalTarget(a Assessment, c *Config, extra float64) int {
	if a.Level == Danger {
		return 100
	}
	target := float64(c.MinimumPercent)
	for name, points := range c.Curves {
		target = max(target, curve(points, a.Temps[name]+extra))
	}
	// A warning keeps stepping fans up until it clears.
	if a.Level == Warning {
		target = max(target, slices.Max(a.Fans)+10)
	}
	return min(100, int(math.Ceil(target)))
}

// next returns the percentage to write, or -1 to leave the fans alone.
// Raises apply immediately. Lowering waits for LowerAfter calm samples and
// must still hold with temperatures Hysteresis °C higher, so the fans do
// not oscillate.
func (s *State) next(a Assessment, c *Config) int {
	if s.Override == Released {
		if a.Level != Danger {
			return -1
		}
		s.Override = Auto
	}
	up, down := thermalTarget(a, c, 0), thermalTarget(a, c, c.Hysteresis)
	if s.Override == Manual {
		up, down = max(up, s.Manual), max(down, s.Manual)
	}
	switch {
	case s.Commanded < 0 || up > s.Commanded:
		s.calm = 0
		return up
	case down < s.Commanded:
		s.calm++
		if s.calm >= c.LowerAfter {
			s.calm = 0
			return down
		}
	default:
		s.calm = 0
	}
	return -1
}
