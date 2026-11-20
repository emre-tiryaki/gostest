// Package config loads the optional .gostest.json project configuration.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// FileName is the configuration file looked up by Find.
const FileName = ".gostest.json"

// Duration is a time.Duration written as a string such as "2m".
type Duration struct{ time.Duration }

// UnmarshalJSON parses a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	d.Duration = v
	return err
}

// Config is the project configuration.
type Config struct {
	Distros       []string        `json:"distros"`
	CustomDistros []distro.Distro `json:"custom_distros"`
	Packages      []string        `json:"packages"`
	Concurrency   int             `json:"concurrency"`
	Timeout       Duration        `json:"timeout"`
	Engine        string          `json:"engine"`
	IgnoreStderr  bool            `json:"ignore_stderr"`
}

// Load reads the configuration file at path.
func Load(path string) (Config, error) {
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	return c, json.Unmarshal(b, &c)
}

// Find looks for FileName in dir.
func Find(dir string) (string, bool) {
	p := filepath.Join(dir, FileName)
	_, err := os.Stat(p)
	return p, err == nil
}

// Registry returns a distro registry including the custom distributions.
func (c Config) Registry() (*distro.Registry, error) {
	return distro.NewRegistry(c.CustomDistros...)
}
