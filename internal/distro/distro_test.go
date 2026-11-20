package distro

import (
	"reflect"
	"strings"
	"testing"
)

func mustRegistry(t *testing.T, extra ...Distro) *Registry {
	t.Helper()
	r, err := NewRegistry(extra...)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r
}

func names(ds []Distro) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

func TestBuiltinDistros(t *testing.T) {
	r := mustRegistry(t)
	want := map[string]Distro{
		"almalinux": {Name: "almalinux", Image: "almalinux:latest", PackageManager: DNF},
		"alpine":    {Name: "alpine", Image: "alpine:latest", PackageManager: APK},
		"archlinux": {Name: "archlinux", Image: "archlinux:latest", PackageManager: Pacman},
		"debian":    {Name: "debian", Image: "debian:latest", PackageManager: APT},
		"fedora":    {Name: "fedora", Image: "fedora:latest", PackageManager: DNF},
		"gentoo":    {Name: "gentoo", Image: "gentoo/stage3:latest", PackageManager: Emerge},
		"opensuse":  {Name: "opensuse", Image: "opensuse/tumbleweed:latest", PackageManager: Zypper},
		"ubuntu":    {Name: "ubuntu", Image: "ubuntu:latest", PackageManager: APT},
	}
	all := r.All()
	if len(all) != len(want) {
		t.Fatalf("All() returned %d distros %v, want %d", len(all), names(all), len(want))
	}
	for _, d := range all {
		if w, ok := want[d.Name]; !ok || w != d {
			t.Errorf("builtin %q = %+v, want %+v", d.Name, d, w)
		}
	}
}

func TestAllIsSortedByName(t *testing.T) {
	r := mustRegistry(t, Distro{Name: "aaa", Image: "x:1", PackageManager: APK})
	got := names(r.All())
	want := []string{"aaa", "almalinux", "alpine", "archlinux", "debian", "fedora", "gentoo", "opensuse", "ubuntu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
}

func TestDefaults(t *testing.T) {
	r := mustRegistry(t)
	got := names(r.Defaults())
	want := []string{"alpine", "archlinux", "debian", "fedora", "ubuntu"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Defaults() = %v, want %v", got, want)
	}
}

func TestDefaultsReflectOverrides(t *testing.T) {
	r := mustRegistry(t, Distro{Name: "ubuntu", Image: "ubuntu:22.04", PackageManager: APT})
	for _, d := range r.Defaults() {
		if d.Name == "ubuntu" && d.Image != "ubuntu:22.04" {
			t.Fatalf("default ubuntu image = %q, want override", d.Image)
		}
	}
}

func TestNewRegistryValidation(t *testing.T) {
	tests := []struct {
		name  string
		extra []Distro
	}{
		{"empty name", []Distro{{Name: "", Image: "x:1", PackageManager: APK}}},
		{"name with space", []Distro{{Name: "my distro", Image: "x:1", PackageManager: APK}}},
		{"name with colon", []Distro{{Name: "my:distro", Image: "x:1", PackageManager: APK}}},
		{"uppercase name", []Distro{{Name: "Mine", Image: "x:1", PackageManager: APK}}},
		{"name starting with dash", []Distro{{Name: "-x", Image: "x:1", PackageManager: APK}}},
		{"empty image", []Distro{{Name: "mine", Image: "", PackageManager: APK}}},
		{"image with space", []Distro{{Name: "mine", Image: "a b", PackageManager: APK}}},
		{"unknown package manager", []Distro{{Name: "mine", Image: "x:1", PackageManager: "brew"}}},
		{"empty package manager", []Distro{{Name: "mine", Image: "x:1"}}},
		{"duplicate", []Distro{
			{Name: "mine", Image: "x:1", PackageManager: APK},
			{Name: "mine", Image: "x:2", PackageManager: APK},
		}},
		{"alias collision", []Distro{{Name: "arch", Image: "x:1", PackageManager: Pacman}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewRegistry(tt.extra...); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNewRegistryAcceptsValidCustom(t *testing.T) {
	r := mustRegistry(t,
		Distro{Name: "void-musl", Image: "ghcr.io/void-linux/void-musl:latest", PackageManager: APK},
		Distro{Name: "rocky9", Image: "rockylinux:9", PackageManager: DNF},
		Distro{Name: "my.distro_1", Image: "registry.local:5000/team/img@sha256:abc", PackageManager: APT},
	)
	for _, n := range []string{"void-musl", "rocky9", "my.distro_1"} {
		if _, err := r.Parse(n); err != nil {
			t.Errorf("Parse(%q): %v", n, err)
		}
	}
}

func TestRegistryDoesNotLeakInternalState(t *testing.T) {
	r := mustRegistry(t)
	all := r.All()
	all[0].Image = "mutated"
	if r.All()[0].Image == "mutated" {
		t.Fatal("All() must return a copy")
	}
}

func TestPackageManagerValid(t *testing.T) {
	for _, pm := range []PackageManager{APT, DNF, APK, Pacman, Zypper, Emerge} {
		if !pm.Valid() {
			t.Errorf("%q should be valid", pm)
		}
	}
	for _, pm := range []PackageManager{"", "brew", "APT", "yum"} {
		if pm.Valid() {
			t.Errorf("%q should be invalid", pm)
		}
	}
}

func TestDistroBase(t *testing.T) {
	tests := map[string]string{
		"ubuntu":         "ubuntu",
		"ubuntu:22.04":   "ubuntu",
		"archlinux:base": "archlinux",
	}
	for name, want := range tests {
		if got := (Distro{Name: name}).Base(); got != want {
			t.Errorf("Base(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	r := mustRegistry(t,
		Distro{Name: "mine", Image: "registry.local:5000/team/img:1", PackageManager: APK},
		Distro{Name: "pinned", Image: "team/img@sha256:abcdef", PackageManager: APT},
		Distro{Name: "notag", Image: "team/img", PackageManager: DNF},
	)
	tests := []struct {
		spec string
		want Distro
	}{
		{"alpine", Distro{Name: "alpine", Image: "alpine:latest", PackageManager: APK}},
		{"  Alpine  ", Distro{Name: "alpine", Image: "alpine:latest", PackageManager: APK}},
		{"UBUNTU", Distro{Name: "ubuntu", Image: "ubuntu:latest", PackageManager: APT}},
		{"arch", Distro{Name: "archlinux", Image: "archlinux:latest", PackageManager: Pacman}},
		{"suse", Distro{Name: "opensuse", Image: "opensuse/tumbleweed:latest", PackageManager: Zypper}},
		{"ubuntu:22.04", Distro{Name: "ubuntu:22.04", Image: "ubuntu:22.04", PackageManager: APT}},
		{"Ubuntu:22.04", Distro{Name: "ubuntu:22.04", Image: "ubuntu:22.04", PackageManager: APT}},
		{"alpine:3.18", Distro{Name: "alpine:3.18", Image: "alpine:3.18", PackageManager: APK}},
		{"arch:base-devel", Distro{Name: "archlinux:base-devel", Image: "archlinux:base-devel", PackageManager: Pacman}},
		{"gentoo:amd64-openrc", Distro{Name: "gentoo:amd64-openrc", Image: "gentoo/stage3:amd64-openrc", PackageManager: Emerge}},
		{"opensuse:20240101", Distro{Name: "opensuse:20240101", Image: "opensuse/tumbleweed:20240101", PackageManager: Zypper}},
		{"mine", Distro{Name: "mine", Image: "registry.local:5000/team/img:1", PackageManager: APK}},
		{"mine:2", Distro{Name: "mine:2", Image: "registry.local:5000/team/img:2", PackageManager: APK}},
		{"pinned:v3", Distro{Name: "pinned:v3", Image: "team/img:v3", PackageManager: APT}},
		{"notag:7", Distro{Name: "notag:7", Image: "team/img:7", PackageManager: DNF}},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := r.Parse(tt.spec)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.spec, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tt.spec, got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	r := mustRegistry(t)
	tests := []string{"", "   ", "nope", "ubuntu:", ":22.04", "ubuntu:a b", "ubuntu:-x", "ubuntu:1:2", "ubu ntu"}
	for _, spec := range tests {
		t.Run(spec, func(t *testing.T) {
			if _, err := r.Parse(spec); err == nil {
				t.Fatalf("Parse(%q) should fail", spec)
			}
		})
	}
}

func TestParseUnknownListsKnownNames(t *testing.T) {
	r := mustRegistry(t)
	_, err := r.Parse("nope")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, n := range []string{"alpine", "ubuntu", "fedora"} {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("error %q should mention known distro %q", err, n)
		}
	}
}

func TestParseList(t *testing.T) {
	r := mustRegistry(t)
	tests := []struct {
		list string
		want []string
	}{
		{"alpine", []string{"alpine"}},
		{"alpine,ubuntu", []string{"alpine", "ubuntu"}},
		{"ubuntu,alpine", []string{"ubuntu", "alpine"}},
		{" alpine , ubuntu ,", []string{"alpine", "ubuntu"}},
		{"alpine,alpine", []string{"alpine"}},
		{"arch,archlinux", []string{"archlinux"}},
		{"ubuntu,ubuntu:22.04", []string{"ubuntu", "ubuntu:22.04"}},
	}
	for _, tt := range tests {
		t.Run(tt.list, func(t *testing.T) {
			got, err := r.ParseList(tt.list)
			if err != nil {
				t.Fatalf("ParseList(%q): %v", tt.list, err)
			}
			if !reflect.DeepEqual(names(got), tt.want) {
				t.Fatalf("ParseList(%q) = %v, want %v", tt.list, names(got), tt.want)
			}
		})
	}
}

func TestParseListErrors(t *testing.T) {
	r := mustRegistry(t)
	for _, list := range []string{"", " , ,", "alpine,nope"} {
		t.Run(list, func(t *testing.T) {
			if _, err := r.ParseList(list); err == nil {
				t.Fatalf("ParseList(%q) should fail", list)
			}
		})
	}
}

func TestInstallScript(t *testing.T) {
	tests := []struct {
		pm   PackageManager
		pkgs []string
		want string
	}{
		{APK, []string{"curl", "bash"}, "apk add --no-cache 'curl' 'bash'"},
		{APT, []string{"curl"}, "apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends 'curl' && rm -rf /var/lib/apt/lists/*"},
		{DNF, []string{"curl", "git"}, "dnf install -y 'curl' 'git' && dnf clean all"},
		{Pacman, []string{"curl"}, "pacman -Syu --noconfirm --needed 'curl'"},
		{Zypper, []string{"curl"}, "zypper --non-interactive refresh && zypper --non-interactive install --no-recommends 'curl' && zypper clean --all"},
		{Emerge, []string{"app-shells/bash"}, "(emerge-webrsync --quiet || emerge --sync --quiet) && emerge --quiet --noreplace 'app-shells/bash'"},
	}
	for _, tt := range tests {
		t.Run(string(tt.pm), func(t *testing.T) {
			got, err := InstallScript(tt.pm, tt.pkgs)
			if err != nil {
				t.Fatalf("InstallScript: %v", err)
			}
			if got != tt.want {
				t.Fatalf("InstallScript(%s, %v)\n got: %s\nwant: %s", tt.pm, tt.pkgs, got, tt.want)
			}
		})
	}
}

func TestInstallScriptNoPackages(t *testing.T) {
	for _, pm := range []PackageManager{APT, DNF, APK, Pacman, Zypper, Emerge} {
		got, err := InstallScript(pm, nil)
		if err != nil || got != "" {
			t.Errorf("InstallScript(%s, nil) = %q, %v; want empty, nil", pm, got, err)
		}
	}
}

func TestInstallScriptErrors(t *testing.T) {
	if _, err := InstallScript("brew", []string{"curl"}); err == nil {
		t.Error("unknown package manager should fail")
	}
	bad := []string{"", "a b", "curl;rm -rf /", "$(id)", "`id`", "'q'", "\"q\"", "a|b", "a&b", "-rf", "a\nb", "a\\b"}
	for _, p := range bad {
		if _, err := InstallScript(APK, []string{"curl", p}); err == nil {
			t.Errorf("package %q should be rejected", p)
		}
	}
}

func TestValidPackageName(t *testing.T) {
	good := []string{"curl", "python3-pip", "g++", "libstdc++6", "app-shells/bash", "=dev-lang/go-1.22", ">=sys-apps/sed-4", "perl:5", "lib32-glibc", "ca_certificates", "node@20", "pkg~1"}
	for _, p := range good {
		if !ValidPackageName(p) {
			t.Errorf("%q should be valid", p)
		}
	}
}

func TestParsePackages(t *testing.T) {
	tests := []struct {
		name  string
		specs []string
		want  Packages
	}{
		{"empty", nil, Packages{}},
		{"common", []string{"curl"}, Packages{Common: []string{"curl"}}},
		{"per distro", []string{"alpine=bash"}, Packages{PerDistro: map[string][]string{"alpine": {"bash"}}}},
		{"mixed", []string{"curl", "alpine=bash"}, Packages{Common: []string{"curl"}, PerDistro: map[string][]string{"alpine": {"bash"}}}},
		{"comma separated", []string{"curl,git", "alpine=bash,coreutils"}, Packages{
			Common:    []string{"curl", "git", "coreutils"},
			PerDistro: map[string][]string{"alpine": {"bash"}},
		}},
		{"whitespace and empties", []string{" curl , ,git ", ""}, Packages{Common: []string{"curl", "git"}}},
		{"dedupe", []string{"curl,curl", "alpine=bash", "alpine=bash"}, Packages{
			Common:    []string{"curl"},
			PerDistro: map[string][]string{"alpine": {"bash"}},
		}},
		{"alias and case", []string{"Arch=base-devel", "ALPINE=bash"}, Packages{PerDistro: map[string][]string{
			"archlinux": {"base-devel"},
			"alpine":    {"bash"},
		}}},
		{"tagged distro", []string{"ubuntu:22.04=python3"}, Packages{PerDistro: map[string][]string{"ubuntu:22.04": {"python3"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePackages(tt.specs)
			if err != nil {
				t.Fatalf("ParsePackages(%v): %v", tt.specs, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParsePackages(%v) = %#v, want %#v", tt.specs, got, tt.want)
			}
		})
	}
}

func TestParsePackagesErrors(t *testing.T) {
	for _, spec := range []string{"alpine=", "=bash", "a=b=c", "curl;id", "alpine=$(id)", "bad distro=curl"} {
		t.Run(spec, func(t *testing.T) {
			if _, err := ParsePackages([]string{spec}); err == nil {
				t.Fatalf("ParsePackages(%q) should fail", spec)
			}
		})
	}
}

func TestPackagesFor(t *testing.T) {
	p := Packages{
		Common: []string{"curl"},
		PerDistro: map[string][]string{
			"ubuntu":       {"python3"},
			"ubuntu:22.04": {"python3-old", "curl"},
			"alpine":       {"bash"},
		},
	}
	tests := []struct {
		distro string
		want   []string
	}{
		{"ubuntu", []string{"curl", "python3"}},
		{"ubuntu:22.04", []string{"curl", "python3", "python3-old"}},
		{"alpine", []string{"curl", "bash"}},
		{"fedora", []string{"curl"}},
	}
	for _, tt := range tests {
		t.Run(tt.distro, func(t *testing.T) {
			got := p.For(Distro{Name: tt.distro})
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("For(%s) = %v, want %v", tt.distro, got, tt.want)
			}
		})
	}
}

func TestPackagesForEmpty(t *testing.T) {
	if got := (Packages{}).For(Distro{Name: "alpine"}); len(got) != 0 {
		t.Fatalf("For on empty Packages = %v, want empty", got)
	}
}

func TestPackagesForReturnsCopy(t *testing.T) {
	p := Packages{Common: []string{"curl", "git"}}
	got := p.For(Distro{Name: "alpine"})
	got[0] = "mutated"
	if p.Common[0] != "curl" {
		t.Fatal("For must not alias the Common slice")
	}
}

func TestPackagesMerge(t *testing.T) {
	a := Packages{Common: []string{"curl"}, PerDistro: map[string][]string{"alpine": {"bash"}}}
	b := Packages{Common: []string{"git", "curl"}, PerDistro: map[string][]string{"alpine": {"coreutils"}, "fedora": {"which"}}}
	got := a.Merge(b)
	want := Packages{
		Common:    []string{"curl", "git"},
		PerDistro: map[string][]string{"alpine": {"bash", "coreutils"}, "fedora": {"which"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Merge = %#v, want %#v", got, want)
	}
	if len(a.PerDistro["alpine"]) != 1 {
		t.Fatal("Merge must not modify its receiver")
	}
}

func TestPackagesEmpty(t *testing.T) {
	if !(Packages{}).Empty() {
		t.Error("zero Packages should be empty")
	}
	if (Packages{Common: []string{"curl"}}).Empty() {
		t.Error("Packages with common entries is not empty")
	}
	if (Packages{PerDistro: map[string][]string{"alpine": {"bash"}}}).Empty() {
		t.Error("Packages with per-distro entries is not empty")
	}
}
