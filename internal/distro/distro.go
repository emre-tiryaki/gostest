// Package distro describes the Linux distributions gostest can target: the
// container image each one uses and how dependencies are installed into it.
package distro

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

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

// Valid reports whether pm is a supported package manager.
func (pm PackageManager) Valid() bool {
	switch pm {
	case APT, DNF, APK, Pacman, Zypper, Emerge:
		return true
	}
	return false
}

// Distro is a target environment.
type Distro struct {
	// Name is the display name, e.g. "ubuntu" or "ubuntu:22.04".
	Name string `json:"name"`
	// Image is the container image reference the distribution runs in.
	Image string `json:"image"`
	// PackageManager installs dependencies inside Image.
	PackageManager PackageManager `json:"package_manager"`
}

// Base returns the name without a tag: "ubuntu:22.04" -> "ubuntu".
func (d Distro) Base() string {
	base, _, _ := strings.Cut(d.Name, ":")
	return base
}

var builtin = []Distro{
	{Name: "almalinux", Image: "almalinux:latest", PackageManager: DNF},
	{Name: "alpine", Image: "alpine:latest", PackageManager: APK},
	{Name: "archlinux", Image: "archlinux:latest", PackageManager: Pacman},
	{Name: "debian", Image: "debian:latest", PackageManager: APT},
	{Name: "fedora", Image: "fedora:latest", PackageManager: DNF},
	{Name: "gentoo", Image: "gentoo/stage3:latest", PackageManager: Emerge},
	{Name: "opensuse", Image: "opensuse/tumbleweed:latest", PackageManager: Zypper},
	{Name: "ubuntu", Image: "ubuntu:latest", PackageManager: APT},
}

// defaultNames are tested when the user does not choose distributions. They
// cover the three /bin/sh flavours (dash, bash, busybox ash) and the most
// common package managers while staying reasonably quick to pull.
var defaultNames = []string{"alpine", "archlinux", "debian", "fedora", "ubuntu"}

// aliases maps alternative spellings to canonical names.
var aliases = map[string]string{
	"arch": "archlinux",
	"suse": "opensuse",
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	tagRe  = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	// pkgRe accepts plain package names plus an optional version operator
	// prefix used by Gentoo atoms (=, >=, <=, ~). Anything that could be
	// interpreted by a shell is rejected.
	pkgRe = regexp.MustCompile(`^[<>=~]{0,2}[A-Za-z0-9][A-Za-z0-9._+:@/~-]*$`)
	// cmdRe accepts command names that are safe to quote in a script and
	// to use as a package name or a file name under /usr/bin.
	cmdRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
)

// Registry resolves distribution names to Distro values.
type Registry struct {
	byName map[string]Distro
}

// NewRegistry returns a registry with the built-in distributions plus extra.
// An extra distribution may override a built-in one with the same name.
func NewRegistry(extra ...Distro) (*Registry, error) {
	r := &Registry{byName: make(map[string]Distro, len(builtin)+len(extra))}
	for _, d := range builtin {
		r.byName[d.Name] = d
	}
	seen := map[string]bool{}
	for _, d := range extra {
		if err := validate(d); err != nil {
			return nil, err
		}
		if seen[d.Name] {
			return nil, fmt.Errorf("distro %q is defined more than once", d.Name)
		}
		if _, ok := aliases[d.Name]; ok {
			return nil, fmt.Errorf("distro name %q is reserved as an alias for %q", d.Name, aliases[d.Name])
		}
		seen[d.Name] = true
		r.byName[d.Name] = d
	}
	return r, nil
}

func validate(d Distro) error {
	if !nameRe.MatchString(d.Name) {
		return fmt.Errorf("invalid distro name %q: use lowercase letters, digits, '.', '_' or '-'", d.Name)
	}
	if d.Image == "" || strings.ContainsFunc(d.Image, isSpace) {
		return fmt.Errorf("distro %q: invalid image %q", d.Name, d.Image)
	}
	if !d.PackageManager.Valid() {
		return fmt.Errorf("distro %q: unsupported package manager %q (supported: apt, dnf, apk, pacman, zypper, emerge)", d.Name, d.PackageManager)
	}
	return nil
}

func isSpace(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || r == '\r' }

// All returns every registered distribution sorted by name.
func (r *Registry) All() []Distro {
	out := make([]Distro, 0, len(r.byName))
	for _, d := range r.byName {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Defaults returns the distributions tested when none are requested.
func (r *Registry) Defaults() []Distro {
	out := make([]Distro, 0, len(defaultNames))
	for _, n := range defaultNames {
		out = append(out, r.byName[n])
	}
	return out
}

func (r *Registry) knownNames() string {
	all := r.All()
	ns := make([]string, len(all))
	for i, d := range all {
		ns[i] = d.Name
	}
	return strings.Join(ns, ", ")
}

// Parse resolves a single distribution spec. A spec is a distribution name
// ("alpine"), an alias ("arch"), or a name with an image tag ("ubuntu:22.04").
func (r *Registry) Parse(spec string) (Distro, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Distro{}, fmt.Errorf("empty distro name")
	}
	name, tag, hasTag := strings.Cut(spec, ":")
	name = canonical(name)
	if hasTag && !tagRe.MatchString(tag) {
		return Distro{}, fmt.Errorf("invalid tag %q in distro %q", tag, spec)
	}
	d, ok := r.byName[name]
	if !ok {
		return Distro{}, fmt.Errorf("unknown distro %q (known: %s)", spec, r.knownNames())
	}
	if hasTag {
		d.Name = name + ":" + tag
		d.Image = repository(d.Image) + ":" + tag
	}
	return d, nil
}

func canonical(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if c, ok := aliases[name]; ok {
		return c
	}
	return name
}

// repository strips the tag and digest from an image reference, keeping any
// registry host and port: "host:5000/a/b:1" -> "host:5000/a/b".
func repository(image string) string {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image = image[:i]
	}
	return image
}

// ParseList resolves a comma separated list of specs, skipping empty items
// and dropping duplicates while keeping the order of first appearance.
func (r *Registry) ParseList(list string) ([]Distro, error) {
	var out []Distro
	seen := map[string]bool{}
	for item := range strings.SplitSeq(list, ",") {
		if strings.TrimSpace(item) == "" {
			continue
		}
		d, err := r.Parse(item)
		if err != nil {
			return nil, err
		}
		if !seen[d.Name] {
			seen[d.Name] = true
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no distros given")
	}
	return out, nil
}

// ValidPackageName reports whether p is safe to pass to a package manager.
func ValidPackageName(p string) bool { return pkgRe.MatchString(p) }

// InstallScript returns the shell script that installs pkgs with pm, or an
// empty string when there is nothing to install.
func InstallScript(pm PackageManager, pkgs []string) (string, error) {
	if !pm.Valid() {
		return "", fmt.Errorf("unsupported package manager %q", pm)
	}
	if len(pkgs) == 0 {
		return "", nil
	}
	quoted := make([]string, len(pkgs))
	for i, p := range pkgs {
		if !ValidPackageName(p) {
			return "", fmt.Errorf("invalid package name %q", p)
		}
		quoted[i] = "'" + p + "'"
	}
	list := strings.Join(quoted, " ")
	switch pm {
	case APK:
		return "apk add --no-cache " + list, nil
	case APT:
		return "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends " + list + " && rm -rf /var/lib/apt/lists/*", nil
	case DNF:
		return "dnf install -y " + list + " && dnf clean all", nil
	case Pacman:
		return "pacman -Syu --noconfirm --needed " + list, nil
	case Zypper:
		return "zypper --non-interactive refresh && zypper --non-interactive install --no-recommends " + list + " && zypper clean --all", nil
	default: // Emerge
		return "(emerge-webrsync --quiet || emerge --sync --quiet) && emerge --quiet --noreplace " + list, nil
	}
}

// ValidCommandName reports whether c is a command name gostest can look
// up a package for.
func ValidCommandName(c string) bool { return cmdRe.MatchString(c) && len(c) <= 128 }

// ProvideScript returns the shell script that tries to install, with pm, a
// package providing each of cmds. A command no package provides is skipped,
// so the script fails only when the package index cannot be used at all.
// It returns an empty string when cmds is empty.
func ProvideScript(pm PackageManager, cmds []string) (string, error) {
	if !pm.Valid() {
		return "", fmt.Errorf("unsupported package manager %q", pm)
	}
	if len(cmds) == 0 {
		return "", nil
	}
	quoted := make([]string, len(cmds))
	for i, c := range cmds {
		if !ValidCommandName(c) {
			return "", fmt.Errorf("invalid command name %q", c)
		}
		quoted[i] = "'" + c + "'"
	}
	list := strings.Join(quoted, " ")
	switch pm {
	case APK:
		return "for c in " + list + `; do apk add --no-cache "cmd:$c" || true; done`, nil
	default:
		return "", fmt.Errorf("installing missing commands is not supported with %s", pm)
	}
}

// Packages holds the packages to install, globally and per distribution.
type Packages struct {
	// Common packages are installed in every distribution.
	Common []string
	// PerDistro maps a distro name ("alpine") or tagged name
	// ("ubuntu:22.04") to extra packages for it.
	PerDistro map[string][]string
}

// ParsePackages parses package specs. Each spec is a comma separated list of
// items; an item is either a package name ("curl"), installed everywhere, or
// "distro=package" ("alpine=bash"), installed only in that distribution.
func ParsePackages(specs []string) (Packages, error) {
	var p Packages
	for _, spec := range specs {
		for item := range strings.SplitSeq(spec, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			key, pkg, scoped := strings.Cut(item, "=")
			if !scoped {
				if !ValidPackageName(item) {
					return Packages{}, fmt.Errorf("invalid package name %q", item)
				}
				p.Common = appendUnique(p.Common, item)
				continue
			}
			key, err := packageKey(key)
			if err != nil {
				return Packages{}, fmt.Errorf("invalid package spec %q: %w", item, err)
			}
			if !ValidPackageName(pkg) || strings.Contains(strings.TrimLeft(pkg, "<>=~"), "=") {
				return Packages{}, fmt.Errorf("invalid package spec %q: bad package name %q", item, pkg)
			}
			if p.PerDistro == nil {
				p.PerDistro = map[string][]string{}
			}
			p.PerDistro[key] = appendUnique(p.PerDistro[key], pkg)
		}
	}
	return p, nil
}

func packageKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", fmt.Errorf("missing distro name before '='")
	}
	name, tag, hasTag := strings.Cut(key, ":")
	name = canonical(name)
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("invalid distro name %q", name)
	}
	if hasTag {
		if !tagRe.MatchString(tag) {
			return "", fmt.Errorf("invalid tag %q", tag)
		}
		return name + ":" + tag, nil
	}
	return name, nil
}

func appendUnique(list []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(list, it) {
			list = append(list, it)
		}
	}
	return list
}

// For returns the packages to install for d: the common packages, then those
// for its base name, then those for its exact (tagged) name.
func (p Packages) For(d Distro) []string {
	out := appendUnique(nil, p.Common...)
	out = appendUnique(out, p.PerDistro[d.Base()]...)
	if d.Name != d.Base() {
		out = appendUnique(out, p.PerDistro[d.Name]...)
	}
	return out
}

// Merge returns the union of p and o; p is left unchanged.
func (p Packages) Merge(o Packages) Packages {
	var out Packages
	out.Common = appendUnique(appendUnique(nil, p.Common...), o.Common...)
	for _, src := range []map[string][]string{p.PerDistro, o.PerDistro} {
		for k, v := range src {
			if out.PerDistro == nil {
				out.PerDistro = map[string][]string{}
			}
			out.PerDistro[k] = appendUnique(out.PerDistro[k], v...)
		}
	}
	return out
}

// Empty reports whether no packages are requested at all.
func (p Packages) Empty() bool {
	if len(p.Common) > 0 {
		return false
	}
	for _, v := range p.PerDistro {
		if len(v) > 0 {
			return false
		}
	}
	return true
}
