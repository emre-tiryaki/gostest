package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadFull(t *testing.T) {
	p := writeConfig(t, `{
		"distros": ["alpine", "ubuntu:22.04", "mine"],
		"custom_distros": [{"name": "mine", "image": "registry.local/mine:1", "package_manager": "apk"}],
		"packages": ["curl", "alpine=bash"],
		"concurrency": 3,
		"timeout": "90s",
		"engine": "podman",
		"ignore_stderr": true,
		"strict_order": true,
		"no_auto_install": true,
		"cleanup": true,
		"dependencies": ["jq", "curl"]
	}`)
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Config{
		Distros:       []string{"alpine", "ubuntu:22.04", "mine"},
		CustomDistros: []distro.Distro{{Name: "mine", Image: "registry.local/mine:1", PackageManager: distro.APK}},
		Packages:      []string{"curl", "alpine=bash"},
		Concurrency:   3,
		Timeout:       Duration{90 * time.Second},
		Engine:        "podman",
		IgnoreStderr:  true,
		StrictOrder:   true,
		NoAutoInstall: true,
		Cleanup:       true,
		Dependencies:  []string{"jq", "curl"},
	}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("Load = %+v\nwant  %+v", c, want)
	}
}

func TestLoadEmptyObject(t *testing.T) {
	c, err := Load(writeConfig(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c, Config{}) {
		t.Fatalf("Load({}) = %+v, want zero config", c)
	}
}

func TestDurationForms(t *testing.T) {
	tests := map[string]time.Duration{
		`"2m"`:    2 * time.Minute,
		`"1m30s"`: 90 * time.Second,
		`30`:      30 * time.Second,
		`"0s"`:    0,
		`0`:       0,
		`2.5`:     2500 * time.Millisecond,
	}
	for in, want := range tests {
		c, err := Load(writeConfig(t, `{"timeout": `+in+`}`))
		if err != nil {
			t.Errorf("timeout %s: %v", in, err)
			continue
		}
		if c.Timeout.Duration != want {
			t.Errorf("timeout %s = %v, want %v", in, c.Timeout.Duration, want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]string{
		"empty file":           ``,
		"not an object":        `[]`,
		"syntax error":         `{"distros": [}`,
		"unknown field":        `{"distro": ["alpine"]}`,
		"trailing data":        `{} {}`,
		"bad duration":         `{"timeout": "soon"}`,
		"negative duration":    `{"timeout": "-1s"}`,
		"negative number":      `{"timeout": -5}`,
		"bool duration":        `{"timeout": true}`,
		"negative concurrency": `{"concurrency": -1}`,
		"unknown engine":       `{"engine": "lxc"}`,
		"bad package":          `{"packages": ["curl;id"]}`,
		"bad custom distro":    `{"custom_distros": [{"name": "x", "image": "x:1", "package_manager": "brew"}]}`,
		"unknown distro":       `{"distros": ["nope"]}`,
		"bad distro tag":       `{"distros": ["ubuntu:a b"]}`,
		"wrong type":           `{"concurrency": "3"}`,
		"bad dependency":       `{"dependencies": ["jq; rm -rf /"]}`,
		"empty dependency":     `{"dependencies": [""]}`,
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, content)
			_, err := Load(p)
			if err == nil {
				t.Fatalf("Load(%s) should fail", content)
			}
			if !strings.Contains(err.Error(), p) {
				t.Errorf("error %q should name the file", err)
			}
		})
	}
}

func TestLoadUnknownFieldIsNamed(t *testing.T) {
	_, err := Load(writeConfig(t, `{"distro": ["alpine"]}`))
	if err == nil || !strings.Contains(err.Error(), "distro") {
		t.Fatalf("err = %v, want the unknown field named", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestEngines(t *testing.T) {
	for _, e := range []string{"", "docker", "podman"} {
		if _, err := Load(writeConfig(t, `{"engine": "`+e+`"}`)); err != nil {
			t.Errorf("engine %q: %v", e, err)
		}
	}
}

func TestRegistryIncludesCustomDistros(t *testing.T) {
	c := Config{CustomDistros: []distro.Distro{{Name: "mine", Image: "m:1", PackageManager: distro.APK}}}
	r, err := c.Registry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Parse("mine"); err != nil {
		t.Fatalf("custom distro not registered: %v", err)
	}
	if _, err := r.Parse("alpine"); err != nil {
		t.Fatalf("built-ins must remain: %v", err)
	}
}

func TestFindInDirectory(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	_ = os.WriteFile(p, []byte(`{}`), 0o644)
	got, ok := Find(dir)
	if !ok || got != p {
		t.Fatalf("Find = %q, %v; want %q", got, ok, p)
	}
}

func TestFindInParent(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, FileName)
	_ = os.WriteFile(p, []byte(`{}`), 0o644)
	sub := filepath.Join(root, "a", "b", "c")
	_ = os.MkdirAll(sub, 0o755)
	got, ok := Find(sub)
	if !ok || got != p {
		t.Fatalf("Find = %q, %v; want %q", got, ok, p)
	}
}

func TestFindPrefersNearest(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, FileName), []byte(`{}`), 0o644)
	sub := filepath.Join(root, "sub")
	_ = os.MkdirAll(sub, 0o755)
	near := filepath.Join(sub, FileName)
	_ = os.WriteFile(near, []byte(`{}`), 0o644)
	if got, ok := Find(sub); !ok || got != near {
		t.Fatalf("Find = %q, %v; want nearest %q", got, ok, near)
	}
}

func TestFindIgnoresDirectoriesNamedLikeTheFile(t *testing.T) {
	root := t.TempDir()
	_ = os.Mkdir(filepath.Join(root, FileName), 0o755)
	if p, ok := Find(root); ok && p == filepath.Join(root, FileName) {
		t.Fatal("a directory is not a config file")
	}
}

func TestFindNothing(t *testing.T) {
	// Only guaranteed if no ancestor of the temp dir has a config file.
	dir := t.TempDir()
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, FileName)); err == nil {
			t.Skipf("an ancestor (%s) has a %s", d, FileName)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if p, ok := Find(dir); ok {
		t.Fatalf("Find = %q, want nothing", p)
	}
}

func TestLoadDependencies(t *testing.T) {
	c, err := Load(writeConfig(t, `{"dependencies": ["jq", "python3", "apt-get"]}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if want := []string{"jq", "python3", "apt-get"}; !reflect.DeepEqual(c.Dependencies, want) {
		t.Errorf("Dependencies = %q, want %q", c.Dependencies, want)
	}
}

func TestSaveDependenciesCreatesFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	added, created, err := SaveDependencies(p, []string{"sh", "jq"})
	if err != nil {
		t.Fatalf("SaveDependencies: %v", err)
	}
	if !created || !reflect.DeepEqual(added, []string{"sh", "jq"}) {
		t.Errorf("added %q, created %v: want [sh jq] and true", added, created)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"dependencies\": [\n    \"sh\",\n    \"jq\"\n  ]\n}\n"; string(b) != want {
		t.Errorf("file =\n%s\nwant\n%s", b, want)
	}
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, %v; want 0644", info.Mode(), err)
	}
}

func TestSaveDependenciesCreatesEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	added, created, err := SaveDependencies(p, nil)
	if err != nil {
		t.Fatalf("SaveDependencies: %v", err)
	}
	if !created || len(added) != 0 {
		t.Errorf("added %q, created %v: want nothing and true", added, created)
	}
	b, _ := os.ReadFile(p)
	if want := "{\n  \"dependencies\": []\n}\n"; string(b) != want {
		t.Errorf("file = %q, want %q", b, want)
	}
	if _, err := Load(p); err != nil {
		t.Errorf("the created file does not load: %v", err)
	}
}

func TestSaveDependenciesKeepsOtherSettings(t *testing.T) {
	p := writeConfig(t, `{"timeout": "90s", "distros": ["alpine"], "dependencies": ["jq"], "cleanup": true}`)
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	added, created, err := SaveDependencies(p, []string{"curl", "jq", "curl", "git"})
	if err != nil {
		t.Fatalf("SaveDependencies: %v", err)
	}
	if created || !reflect.DeepEqual(added, []string{"curl", "git"}) {
		t.Errorf("added %q, created %v: want [curl git] and false", added, created)
	}
	b, _ := os.ReadFile(p)
	want := `{
  "timeout": "90s",
  "distros": [
    "alpine"
  ],
  "dependencies": [
    "jq",
    "curl",
    "git"
  ],
  "cleanup": true
}
`
	if string(b) != want {
		t.Errorf("file =\n%s\nwant\n%s", b, want)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Timeout.Duration != 90*time.Second || !c.Cleanup {
		t.Errorf("settings changed: %+v", c)
	}
	if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the original 0600", info.Mode())
	}
}

func TestSaveDependenciesAddsKey(t *testing.T) {
	p := writeConfig(t, `{"engine": "docker"}`)
	if _, _, err := SaveDependencies(p, []string{"jq"}); err != nil {
		t.Fatalf("SaveDependencies: %v", err)
	}
	b, _ := os.ReadFile(p)
	if want := "{\n  \"engine\": \"docker\",\n  \"dependencies\": [\n    \"jq\"\n  ]\n}\n"; string(b) != want {
		t.Errorf("file = %q, want %q", b, want)
	}
}

func TestSaveDependenciesNothingNew(t *testing.T) {
	content := `{"dependencies": ["jq"]}`
	p := writeConfig(t, content)
	added, created, err := SaveDependencies(p, []string{"jq"})
	if err != nil || created || len(added) != 0 {
		t.Fatalf("added %q, created %v, err %v: want nothing", added, created, err)
	}
	if b, _ := os.ReadFile(p); string(b) != content {
		t.Errorf("file rewritten without changes: %q", b)
	}
}

func TestSaveDependenciesEmptyExistingFile(t *testing.T) {
	p := writeConfig(t, "  \n")
	if _, created, err := SaveDependencies(p, []string{"jq"}); err != nil || created {
		t.Fatalf("created %v, err %v: want an update of the empty file", created, err)
	}
	c, err := Load(p)
	if err != nil || !reflect.DeepEqual(c.Dependencies, []string{"jq"}) {
		t.Errorf("Load = %+v, %v", c, err)
	}
}

func TestSaveDependenciesRefusesInvalidFile(t *testing.T) {
	for name, content := range map[string]string{
		"syntax error":  `{"dependencies": [`,
		"unknown field": `{"distro": ["alpine"]}`,
		"not an object": `["jq"]`,
	} {
		t.Run(name, func(t *testing.T) {
			p := writeConfig(t, content)
			if _, _, err := SaveDependencies(p, []string{"jq"}); err == nil || !strings.Contains(err.Error(), p) {
				t.Errorf("err = %v, want an error naming the file", err)
			}
			if b, _ := os.ReadFile(p); string(b) != content {
				t.Errorf("invalid file was changed: %q", b)
			}
		})
	}
}

func TestSaveDependenciesRefusesInvalidNames(t *testing.T) {
	p := filepath.Join(t.TempDir(), FileName)
	if _, _, err := SaveDependencies(p, []string{"jq", "a b"}); err == nil {
		t.Error("an invalid command name should be refused")
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("file written despite the error: %v", err)
	}
}

func TestSaveDependenciesMissingDirectory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope", FileName)
	if _, _, err := SaveDependencies(p, []string{"jq"}); err == nil {
		t.Error("want an error for a missing directory")
	}
}
