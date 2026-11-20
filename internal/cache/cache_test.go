package cache

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type value struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "nested", "cache")
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, dir
}

func TestKey(t *testing.T) {
	k := Key("a", "b")
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(k) {
		t.Fatalf("Key = %q, want 64 hex characters", k)
	}
	if Key("a", "b") != k {
		t.Error("Key must be deterministic")
	}
	distinct := [][]string{{"a", "b"}, {"b", "a"}, {"ab"}, {"a", "b", ""}, {}, {""}, {"ab", "c"}, {"a", "bc"}}
	seen := map[string][]string{}
	for _, parts := range distinct {
		key := Key(parts...)
		if prev, ok := seen[key]; ok {
			t.Errorf("Key(%q) == Key(%q)", parts, prev)
		}
		seen[key] = parts
	}
}

func TestOpenCreatesDirectory(t *testing.T) {
	_, dir := openStore(t)
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("cache dir not created: %v", err)
	}
}

func TestOpenFailsOnFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(f); err == nil {
		t.Fatal("Open on a regular file should fail")
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	s, _ := openStore(t)
	key := Key("x")
	if err := s.Put(key, value{Name: "alpine", Count: 3}); err != nil {
		t.Fatal(err)
	}
	var got value
	ok, err := s.Get(key, &got)
	if err != nil || !ok {
		t.Fatalf("Get = %v, %v", ok, err)
	}
	if got != (value{Name: "alpine", Count: 3}) {
		t.Fatalf("Get = %+v", got)
	}
}

func TestGetMissing(t *testing.T) {
	s, _ := openStore(t)
	var v value
	ok, err := s.Get(Key("missing"), &v)
	if ok || err != nil {
		t.Fatalf("Get(missing) = %v, %v; want false, nil", ok, err)
	}
}

func TestPutOverwrites(t *testing.T) {
	s, _ := openStore(t)
	key := Key("x")
	_ = s.Put(key, value{Count: 1})
	_ = s.Put(key, value{Count: 2})
	var v value
	if ok, _ := s.Get(key, &v); !ok || v.Count != 2 {
		t.Fatalf("Get = %+v, %v; want the last value", v, ok)
	}
}

func TestCorruptEntryIsAMiss(t *testing.T) {
	s, dir := openStore(t)
	key := Key("x")
	if err := s.Put(key, value{Count: 1}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected one entry file, got %d", len(entries))
	}
	if err := os.WriteFile(filepath.Join(dir, entries[0].Name()), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var v value
	ok, err := s.Get(key, &v)
	if ok || err != nil {
		t.Fatalf("corrupt entry: Get = %v, %v; want false, nil", ok, err)
	}
}

func TestEntryFromOtherFormatVersionIsAMiss(t *testing.T) {
	s, dir := openStore(t)
	key := Key("x")
	if err := s.Put(key, value{Count: 1}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	path := filepath.Join(dir, entries[0].Name())
	old, _ := json.Marshal(map[string]any{"version": FormatVersion + 1, "value": value{Count: 1}})
	if err := os.WriteFile(path, old, 0o644); err != nil {
		t.Fatal(err)
	}
	var v value
	if ok, _ := s.Get(key, &v); ok {
		t.Fatal("entries written by another format version must be ignored")
	}
}

func TestPutLeavesNoTemporaryFiles(t *testing.T) {
	s, dir := openStore(t)
	for i := range 5 {
		if err := s.Put(Key(fmt.Sprint(i)), value{Count: i}); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 5 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 5 entries, got %v", names)
	}
}

func TestInvalidKeys(t *testing.T) {
	s, _ := openStore(t)
	for _, key := range []string{"", "../escape", "a/b", "with space", ".hidden"} {
		if err := s.Put(key, value{}); err == nil {
			t.Errorf("Put(%q) should fail", key)
		}
		var v value
		if _, err := s.Get(key, &v); err == nil {
			t.Errorf("Get(%q) should fail", key)
		}
	}
}

func TestPutUnmarshalableValue(t *testing.T) {
	s, _ := openStore(t)
	if err := s.Put(Key("x"), make(chan int)); err == nil {
		t.Fatal("Put of a channel should fail")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s, _ := openStore(t)
	key := Key("shared")
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.Put(key, value{Name: "v", Count: i}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			var v value
			if _, err := s.Get(key, &v); err != nil {
				t.Error(err)
			}
			if v.Name != "" && v.Name != "v" {
				t.Errorf("read a torn value: %+v", v)
			}
		}()
	}
	wg.Wait()
}

func TestClear(t *testing.T) {
	s, dir := openStore(t)
	for i := range 3 {
		_ = s.Put(Key(fmt.Sprint(i)), value{Count: i})
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	var v value
	if ok, _ := s.Get(Key("0"), &v); ok {
		t.Fatal("Clear should remove entries")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("Clear should keep the cache directory usable")
	}
	if err := s.Put(Key("after"), value{}); err != nil {
		t.Fatalf("Put after Clear: %v", err)
	}
}

func TestEntryRecordsCreationTime(t *testing.T) {
	s, _ := openStore(t)
	before := time.Now().Add(-time.Second)
	_ = s.Put(Key("x"), value{})
	var v value
	created, ok, err := s.GetWithTime(Key("x"), &v)
	if err != nil || !ok {
		t.Fatalf("GetWithTime = %v, %v", ok, err)
	}
	if created.Before(before) || created.After(time.Now().Add(time.Second)) {
		t.Fatalf("created = %v", created)
	}
}

func TestDefaultDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOSTEST_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", tmp)
	got, err := DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(tmp, "gostest") {
		t.Fatalf("DefaultDir = %q, want %q", got, filepath.Join(tmp, "gostest"))
	}
}

func TestDefaultDirOverride(t *testing.T) {
	t.Setenv("GOSTEST_CACHE_DIR", "/custom/place")
	got, err := DefaultDir()
	if err != nil || got != "/custom/place" {
		t.Fatalf("DefaultDir = %q, %v", got, err)
	}
}

// --- Fingerprint ---

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	writeFile(t, filepath.Join(dir, "lib", "util.sh"), "util() { :; }\n", 0o644)
	return dir
}

func fingerprint(t *testing.T, dir string) string {
	t.Helper()
	fp, err := Fingerprint(dir, 1000)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	return fp
}

func TestFingerprintDeterministic(t *testing.T) {
	dir := project(t)
	if fingerprint(t, dir) != fingerprint(t, dir) {
		t.Fatal("Fingerprint must be deterministic")
	}
}

func TestFingerprintIndependentOfLocation(t *testing.T) {
	if fingerprint(t, project(t)) != fingerprint(t, project(t)) {
		t.Fatal("identical trees in different places should have the same fingerprint")
	}
}

func TestFingerprintDetectsChanges(t *testing.T) {
	tests := map[string]func(t *testing.T, dir string){
		"content change, same size": func(t *testing.T, dir string) {
			p := filepath.Join(dir, "run.sh")
			info, _ := os.Stat(p)
			writeFile(t, p, "#!/bin/sh\necho ho\n", 0o755)
			// Restore the modification time so only the content differs.
			_ = os.Chtimes(p, info.ModTime(), info.ModTime())
		},
		"new file":     func(t *testing.T, dir string) { writeFile(t, filepath.Join(dir, "new.txt"), "", 0o644) },
		"removed file": func(t *testing.T, dir string) { _ = os.Remove(filepath.Join(dir, "lib", "util.sh")) },
		"renamed file": func(t *testing.T, dir string) {
			_ = os.Rename(filepath.Join(dir, "run.sh"), filepath.Join(dir, "start.sh"))
		},
		"mode change":   func(t *testing.T, dir string) { _ = os.Chmod(filepath.Join(dir, "run.sh"), 0o644) },
		"new empty dir": func(t *testing.T, dir string) { _ = os.Mkdir(filepath.Join(dir, "empty"), 0o755) },
		"symlink":       func(t *testing.T, dir string) { _ = os.Symlink("run.sh", filepath.Join(dir, "link")) },
		"moved to subdir": func(t *testing.T, dir string) {
			_ = os.Rename(filepath.Join(dir, "run.sh"), filepath.Join(dir, "lib", "run.sh"))
		},
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			dir := project(t)
			before := fingerprint(t, dir)
			change(t, dir)
			if fingerprint(t, dir) == before {
				t.Fatal("fingerprint did not change")
			}
		})
	}
}

func TestFingerprintSymlinkTarget(t *testing.T) {
	dir := project(t)
	link := filepath.Join(dir, "link")
	_ = os.Symlink("run.sh", link)
	before := fingerprint(t, dir)
	_ = os.Remove(link)
	_ = os.Symlink("lib/util.sh", link)
	if fingerprint(t, dir) == before {
		t.Fatal("changing a symlink target should change the fingerprint")
	}
}

func TestFingerprintDoesNotFollowSymlinks(t *testing.T) {
	dir := project(t)
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "big.txt"), "a", 0o644)
	_ = os.Symlink(outside, filepath.Join(dir, "outside"))
	before := fingerprint(t, dir)
	writeFile(t, filepath.Join(outside, "big.txt"), "b", 0o644)
	if fingerprint(t, dir) != before {
		t.Fatal("files behind a directory symlink must not be walked")
	}
}

func TestFingerprintIgnoresGitDirectory(t *testing.T) {
	dir := project(t)
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	before := fingerprint(t, dir)
	writeFile(t, filepath.Join(dir, ".git", "HEAD"), "ref: refs/heads/other\n", 0o644)
	writeFile(t, filepath.Join(dir, ".git", "objects", "ab"), "x", 0o644)
	if fingerprint(t, dir) != before {
		t.Fatal(".git changes must not change the fingerprint")
	}
}

func TestFingerprintLargeFilesUseMetadata(t *testing.T) {
	dir := project(t)
	big := filepath.Join(dir, "big.bin")
	writeFile(t, big, strings.Repeat("a", MaxHashedFileSize+1), 0o644)
	before := fingerprint(t, dir)
	// Grow the file: size changes, so the fingerprint must change.
	writeFile(t, big, strings.Repeat("a", MaxHashedFileSize+2), 0o644)
	if fingerprint(t, dir) == before {
		t.Fatal("size change of a large file should change the fingerprint")
	}
}

func TestFingerprintTooManyFiles(t *testing.T) {
	dir := t.TempDir()
	for i := range 11 {
		writeFile(t, filepath.Join(dir, fmt.Sprintf("f%d", i)), "", 0o644)
	}
	if _, err := Fingerprint(dir, 10); !errors.Is(err, ErrTooManyFiles) {
		t.Fatalf("err = %v, want ErrTooManyFiles", err)
	}
	if _, err := Fingerprint(dir, 11); err != nil {
		t.Fatalf("11 files within a limit of 11: %v", err)
	}
}

func TestFingerprintMissingDirectory(t *testing.T) {
	if _, err := Fingerprint(filepath.Join(t.TempDir(), "nope"), 10); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestFingerprintUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read everything")
	}
	dir := project(t)
	p := filepath.Join(dir, "secret")
	writeFile(t, p, "x", 0o000)
	if _, err := Fingerprint(dir, 1000); err != nil {
		t.Fatalf("an unreadable file must not fail the fingerprint: %v", err)
	}
}
