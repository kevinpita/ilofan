package ilofan

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"
)

type Config struct {
	Host         string `json:"host"`
	Username     string `json:"username"`
	PasswordFile string `json:"passwordFile"`
	// passwordFromSystemd marks a credential whose access systemd already
	// restricts to this service.
	passwordFromSystemd bool
	// HostKey is the iLO SSH public key, e.g. "ssh-rsa AAAA...".
	HostKey        string `json:"hostKey"`
	TLSFingerprint string `json:"tlsFingerprint"`
	InsecureTLS    bool   `json:"insecureTLS"`

	// Mode "observe" reads and computes targets but never writes.
	Mode           string `json:"mode"`
	Socket         string `json:"socket"`
	MetricsAddress string `json:"metricsAddress"`
	ReleaseOnStop  bool   `json:"releaseOnStop"`

	PollSeconds    int     `json:"pollSeconds"`
	MinimumPercent int     `json:"minimumPercent"`
	LowerAfter     int     `json:"lowerAfter"`
	Hysteresis     float64 `json:"hysteresis"`
	Tolerance      float64 `json:"tolerance"`
	// Curves maps a sensor name to temperature -> percent points.
	Curves  map[string][]CurvePoint `json:"curves"`
	Sensors map[string]Limit        `json:"sensors"`
}

// Sensor names and thresholds for an ML310e Gen8 v2. These are conservative
// operating limits, not manufacturer limits.
var defaultSensors = map[string]Limit{
	"01-Inlet Ambient": {34, 38},
	"02-CPU":           {58, 64},
	"03-P1 DIMM 1-4":   {65, 75},
	"05-Chipset":       {62, 70},
	"06-Chipset Zone":  {60, 70},
	"09-VR P1":         {70, 80},
	"12-iLO Zone":      {65, 75},
	"17-PCI 1 Zone":    {48, 54},
	"18-PCI 2 Zone":    {48, 54},
	"19-PCI 3 Zone":    {45, 50},
	"20-PCI 4 Zone":    {45, 50},
	"21-LOM Zone":      {58, 65},
	"22-LOM Zone":      {58, 65},
}

// DefaultConfig returns the controller defaults.
func DefaultConfig() Config {
	return Config{
		Mode:           "observe",
		Socket:         "/run/ilofan/ilofan.sock",
		ReleaseOnStop:  true,
		PollSeconds:    15,
		MinimumPercent: 9,
		LowerAfter:     6,
		Hysteresis:     3,
		Tolerance:      2,
		Curves: map[string][]CurvePoint{
			"01-Inlet Ambient": {{24, 9}, {28, 15}, {31, 25}, {34, 40}},
			"02-CPU":           {{43, 9}, {48, 15}, {53, 25}, {58, 40}},
		},
		Sensors: defaultSensors,
	}
}

// LoadConfig reads and validates a JSON configuration file.
func LoadConfig(path string) (*Config, error) {
	c := DefaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Decoding into a map merges keys, so clear the defaults first: a
	// configured curves or sensors map replaces them.
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if _, ok := keys["curves"]; ok {
		c.Curves = nil
	}
	if _, ok := keys["sensors"]; ok {
		c.Sensors = nil
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Under systemd, a credential named "password" wins over passwordFile.
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		if cred := filepath.Join(dir, "password"); fileExists(cred) {
			c.PasswordFile, c.passwordFromSystemd = cred, true
		}
	}
	return &c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	check := func(ok bool, msg string) {
		if !ok {
			errs = append(errs, errors.New(msg))
		}
	}
	check(c.Host != "", "host is required")
	check(c.Username != "", "username is required")
	check(c.PasswordFile != "", `passwordFile is required unless systemd passes a "password" credential`)
	check(c.Mode == "observe" || c.Mode == "control", `mode must be "observe" or "control"`)
	check(c.Mode == "observe" || c.HostKey != "", "hostKey is required in control mode")
	check((c.TLSFingerprint != "") != c.InsecureTLS, "set exactly one of tlsFingerprint or insecureTLS")
	check(c.PollSeconds > 0, "pollSeconds must be positive")
	check(c.MinimumPercent >= 0 && c.MinimumPercent <= 100, "minimumPercent must be 0..100")
	check(c.LowerAfter > 0, "lowerAfter must be positive")
	check(c.Hysteresis >= 0 && c.Tolerance >= 0, "hysteresis and tolerance must not be negative")
	check(len(c.Sensors) > 0, "sensors must not be empty")
	for name, points := range c.Curves {
		_, ok := c.Sensors[name]
		check(ok, fmt.Sprintf("curve %q needs a matching entry in sensors", name))
		check(len(points) > 0, fmt.Sprintf("curve %q needs at least one point", name))
		slices.SortFunc(points, func(a, b CurvePoint) int { return cmp.Compare(a.Temp, b.Temp) })
		for _, p := range points {
			check(p.Percent >= 0 && p.Percent <= 100, fmt.Sprintf("curve %q: percent must be 0..100", name))
		}
	}
	return errors.Join(errs...)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (c *Config) poll() time.Duration { return time.Duration(c.PollSeconds) * time.Second }
