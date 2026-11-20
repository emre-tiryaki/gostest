// Package config loads the optional .gostest.json project configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// FileName is the configuration file looked up by Find.
const FileName = ".gostest.json"

// Duration is a time.Duration written either as a string such as "2m" or
// as a number of seconds.
type Duration struct{ time.Duration }

// UnmarshalJSON parses a duration string or a number of seconds.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch x := v.(type) {
	case string:
		parsed, err := time.ParseDuration(x)
		if err != nil {
			return fmt.Errorf("invalid duration %q", x)
		}
		d.Duration = parsed
	case float64:
		d.Duration = time.Duration(x * float64(time.Second))
	default:
		return fmt.Errorf("invalid duration %s: use a string such as \"2m\" or a number of seconds", b)
	}
	if d.Duration < 0 {
		return fmt.Errorf("duration %s must not be negative", b)
	}
	return nil
}

// Config is the project configuration. Every field is optional; command
// line flags take precedence over it.
type Config struct {
	// Distros to test, as accepted by --distros.
	Distros []string `json:"distros"`
	// CustomDistros adds or overrides distributions.
	CustomDistros []distro.Distro `json:"custom_distros"`
	// Packages to install, as accepted by --pkg.
	Packages []string `json:"packages"`
	// Concurrency limits parallel distributions; 0 means no limit.
	Concurrency int `json:"concurrency"`
	// Timeout bounds every run.
	Timeout Duration `json:"timeout"`
	// Engine is "docker" or "podman"; empty means auto-detect.
	Engine string `json:"engine"`
	// IgnoreStderr compares only stdout and exit codes.
	IgnoreStderr bool `json:"ignore_stderr"`
}

// Load reads and validates the configuration file at path.
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	c, err := parse(b)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func parse(b []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(b)) == 0 {
		return c, errors.New("empty file: write {} for an empty configuration")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Config{}, errors.New("unexpected data after the configuration object")
	}
	return c, c.validate()
}

func (c Config) validate() error {
	if c.Concurrency < 0 {
		return fmt.Errorf("concurrency must not be negative, got %d", c.Concurrency)
	}
	switch c.Engine {
	case "", "docker", "podman":
	default:
		return fmt.Errorf("unknown engine %q: use docker or podman", c.Engine)
	}
	if _, err := distro.ParsePackages(c.Packages); err != nil {
		return err
	}
	reg, err := c.Registry()
	if err != nil {
		return err
	}
	if len(c.Distros) > 0 {
		if _, err := reg.ParseList(strings.Join(c.Distros, ",")); err != nil {
			return err
		}
	}
	return nil
}

// Find looks for FileName in dir and then in each parent directory, and
// returns the nearest one.
func Find(dir string) (string, bool) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, FileName)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// Registry returns a distro registry including the custom distributions.
func (c Config) Registry() (*distro.Registry, error) {
	return distro.NewRegistry(c.CustomDistros...)
}
