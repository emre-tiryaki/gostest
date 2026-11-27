// Package config loads the optional .gostest.json project configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	// Timeout bounds every run; 0 disables the timeout and omitting it keeps the default.
	Timeout *Duration `json:"timeout"`
	// Engine is "docker" or "podman"; empty means auto-detect.
	Engine string `json:"engine"`
	// IgnoreStderr compares only stdout and exit codes.
	IgnoreStderr bool `json:"ignore_stderr"`
	// StrictOrder fails streams whose lines only differ in order.
	StrictOrder bool `json:"strict_order"`
	// NoAutoInstall disables installing missing commands automatically.
	NoAutoInstall bool `json:"no_auto_install"`
	// Cleanup removes the images pulled or built for a run when it ends.
	Cleanup bool `json:"cleanup"`
	// Dependencies are commands the tested command needs; with automatic
	// installation, those a distribution lacks are installed before the
	// first run. --save-deps fills it in by scanning the command.
	Dependencies []string `json:"dependencies"`
}

// SaveDependencies adds deps to the "dependencies" list of the
// configuration file at path, creating the file when it does not exist.
// The other settings are kept as written, in their order. It returns the
// commands that were not listed yet and whether the file was created; a
// file that already lists every command is not rewritten.
func SaveDependencies(path string, deps []string) (added []string, created bool, err error) {
	for _, d := range deps {
		if !distro.ValidCommandName(d) {
			return nil, false, fmt.Errorf("invalid command name %q", d)
		}
	}
	b, err := os.ReadFile(path)
	created = errors.Is(err, fs.ErrNotExist)
	if err != nil && !created {
		return nil, false, err
	}
	empty := len(bytes.TrimSpace(b)) == 0

	mode := fs.FileMode(0o644)
	var fields []field
	var list []string
	if !created {
		if info, err := os.Stat(path); err == nil {
			mode = info.Mode().Perm()
		}
		if !empty {
			c, err := parse(b)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			if fields, err = objectFields(b); err != nil {
				return nil, false, fmt.Errorf("%s: %w", path, err)
			}
			list = c.Dependencies
		}
	}
	for _, d := range deps {
		if !slices.Contains(list, d) {
			list = append(list, d)
			added = append(added, d)
		}
	}
	if !created && !empty && len(added) == 0 {
		return nil, false, nil
	}

	raw, err := json.Marshal(append([]string{}, list...))
	if err != nil {
		return nil, false, err
	}
	i := slices.IndexFunc(fields, func(f field) bool { return f.key == "dependencies" })
	if i >= 0 {
		fields[i].value = raw
	} else {
		fields = append(fields, field{"dependencies", raw})
	}
	out, err := render(fields)
	if err != nil {
		return nil, false, err
	}
	// Renaming over a symlink would replace the link itself, so write to
	// the file it points to.
	if !created {
		if path, err = filepath.EvalSymlinks(path); err != nil {
			return nil, false, err
		}
	}
	if err := writeFile(path, out, mode); err != nil {
		return nil, false, err
	}
	return added, created, nil
}

// field is a member of a JSON object, with its value as written.
type field struct {
	key   string
	value json.RawMessage
}

// objectFields returns the members of the JSON object b in order.
func objectFields(b []byte) ([]field, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errors.New("the configuration is not a JSON object")
	}
	var fields []field
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, field{key, value})
	}
	return fields, nil
}

// render writes fields as an object indented by two spaces.
func render(fields []field) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, f := range fields {
		key, err := json.Marshal(f.key)
		if err != nil {
			return nil, err
		}
		buf.WriteString("  ")
		buf.Write(key)
		buf.WriteString(": ")
		if err := json.Indent(&buf, f.value, "  ", "  "); err != nil {
			return nil, err
		}
		if i < len(fields)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

// writeFile replaces path atomically with data.
func writeFile(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gostest-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
	for _, d := range c.Dependencies {
		if !distro.ValidCommandName(d) {
			return fmt.Errorf("invalid dependency %q: use a command name such as jq", d)
		}
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
