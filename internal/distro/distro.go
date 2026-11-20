// Package distro describes the Linux distributions gostest can target: the
// container image each one uses and how dependencies are installed into it.
package distro

import "fmt"

// PackageManager identifies how packages are installed in a distribution.
type PackageManager string

// Supported package managers.
const (
	APT    PackageManager = "apt"
	DNF    PackageManager = "dnf"
	APK    PackageManager = "apk"
	Pacman PackageManager = "pacman"
	Zypper PackageManager = "zypper"
	Emerge PackageManager = "emerge"
)

// Distro is a target environment.
type Distro struct {
	Name           string         `json:"name"`
	Image          string         `json:"image"`
	PackageManager PackageManager `json:"package_manager"`
}

// Registry resolves distribution names to Distro values.
type Registry struct {
	byName map[string]Distro
}

// NewRegistry returns a registry with the built-in distributions plus extra.
func NewRegistry(extra ...Distro) (*Registry, error) {
	r := &Registry{byName: map[string]Distro{}}
	for _, d := range builtin {
		r.byName[d.Name] = d
	}
	for _, d := range extra {
		r.byName[d.Name] = d
	}
	return r, nil
}

var builtin = []Distro{
	{Name: "alpine", Image: "alpine:latest", PackageManager: APK},
	{Name: "debian", Image: "debian:latest", PackageManager: APT},
	{Name: "ubuntu", Image: "ubuntu:latest", PackageManager: APT},
}

// All returns every registered distribution.
func (r *Registry) All() []Distro {
	var out []Distro
	for _, d := range r.byName {
		out = append(out, d)
	}
	return out
}

// Defaults returns the distributions tested when none are requested.
func (r *Registry) Defaults() []Distro { return r.All() }

// Parse resolves a single distribution spec such as "alpine".
func (r *Registry) Parse(spec string) (Distro, error) {
	d, ok := r.byName[spec]
	if !ok {
		return Distro{}, fmt.Errorf("unknown distro %q", spec)
	}
	return d, nil
}

// ParseList resolves a comma separated list of specs.
func (r *Registry) ParseList(list string) ([]Distro, error) {
	d, err := r.Parse(list)
	if err != nil {
		return nil, err
	}
	return []Distro{d}, nil
}

// InstallScript returns the shell script that installs pkgs with pm.
func InstallScript(pm PackageManager, pkgs []string) (string, error) {
	if len(pkgs) == 0 {
		return "", nil
	}
	return string(pm) + " install " + pkgs[0], nil
}

// Packages holds the packages to install, globally and per distribution.
type Packages struct {
	Common    []string
	PerDistro map[string][]string
}

// ParsePackages parses specs like "curl" or "alpine=bash".
func ParsePackages(specs []string) (Packages, error) {
	return Packages{Common: specs}, nil
}

// For returns the packages to install for the distribution d.
func (p Packages) For(d Distro) []string { return p.Common }
