// Package cache stores results on disk so that unchanged runs can be reused,
// and fingerprints the working directory so that changes invalidate them.
package cache

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// FormatVersion is bumped whenever the stored layout changes; entries from
// other versions are ignored.
const FormatVersion = 1

// ErrTooManyFiles is returned by Fingerprint for directories with more
// entries than allowed.
var ErrTooManyFiles = errors.New("too many files to fingerprint")

var keyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// ownFileRe matches the files Clear may remove: entries stored under a Key
// and the temporary files of Put.
var ownFileRe = regexp.MustCompile(`^([0-9a-f]{64}\.json|\.tmp-[0-9]+)$`)

// Store is a directory of cached JSON values. It is safe for concurrent use
// by multiple goroutines and processes: writes are atomic renames.
type Store struct {
	dir string
}

type entry struct {
	Version int             `json:"version"`
	Created time.Time       `json:"created"`
	Value   json.RawMessage `json:"value"`
}

// Open returns a store in dir, creating it if needed.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("opening cache: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir returns the directory backing the store.
func (s *Store) Dir() string { return s.dir }

// DefaultDir returns $GOSTEST_CACHE_DIR, or "gostest" inside the user cache
// directory ($XDG_CACHE_HOME or ~/.cache on Linux).
func DefaultDir() (string, error) {
	if d := os.Getenv("GOSTEST_CACHE_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating the cache directory: %w", err)
	}
	return filepath.Join(d, "gostest"), nil
}

// Key derives a cache key from parts. Parts are length-prefixed, so
// Key("ab", "c") and Key("a", "bc") differ.
func Key(parts ...string) string {
	h := sha256.New()
	var n [8]byte
	for _, p := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		io.WriteString(h, p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *Store) path(key string) (string, error) {
	if !keyRe.MatchString(key) {
		return "", fmt.Errorf("invalid cache key %q", key)
	}
	return filepath.Join(s.dir, key+".json"), nil
}

// Get loads the value stored under key into v. Missing, corrupt and
// foreign-version entries are reported as a miss.
func (s *Store) Get(key string, v any) (bool, error) {
	_, ok, err := s.GetWithTime(key, v)
	return ok, err
}

// GetWithTime is Get that also returns when the entry was stored.
func (s *Store) GetWithTime(key string, v any) (time.Time, bool, error) {
	p, err := s.path(key)
	if err != nil {
		return time.Time{}, false, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("reading cache entry: %w", err)
	}
	var e entry
	if json.Unmarshal(b, &e) != nil || e.Version != FormatVersion || json.Unmarshal(e.Value, v) != nil {
		return time.Time{}, false, nil
	}
	return e.Created, true, nil
}

// Put stores v under key, replacing any previous value atomically.
func (s *Store) Put(key string, v any) error {
	p, err := s.path(key)
	if err != nil {
		return err
	}
	value, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encoding cache entry: %w", err)
	}
	b, err := json.Marshal(entry{Version: FormatVersion, Created: time.Now().UTC(), Value: value})
	if err != nil {
		return fmt.Errorf("encoding cache entry: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("writing cache entry: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("writing cache entry: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing cache entry: %w", err)
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		return fmt.Errorf("writing cache entry: %w", err)
	}
	return nil
}

// Clear removes the entries stored under a Key and leftover temporary files.
// Other files are kept: the directory may be shared with something else.
func (s *Store) Clear() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return fmt.Errorf("clearing cache: %w", err)
	}
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && ownFileRe.MatchString(n) {
			if err := os.Remove(filepath.Join(s.dir, n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("clearing cache: %w", err)
			}
		}
	}
	return nil
}

// MaxHashedFileSize is the largest file whose content is hashed by
// Fingerprint; larger files are represented by their size and mtime.
const MaxHashedFileSize = 1 << 20

// Fingerprint summarises the tree under dir: names, modes, symlink targets
// and file contents (size and mtime for files above MaxHashedFileSize). It
// follows dir when it is a symlink but no symlinks below it, and skips .git
// directories. The result does not
// depend on where dir is. It fails with ErrTooManyFiles when the tree has
// more than maxFiles entries.
func Fingerprint(dir string, maxFiles int) (string, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", dir)
	}
	// WalkDir does not descend into a root that is a symlink.
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return "", err
	}

	h := sha256.New()
	count := 0
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if p == dir {
			return walkErr
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if walkErr != nil {
			// Unreadable directory: record it and move on.
			fmt.Fprintf(h, "E %q\n", rel)
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if count++; count > maxFiles {
			return ErrTooManyFiles
		}
		info, err := d.Info()
		if err != nil {
			fmt.Fprintf(h, "E %q\n", rel)
			return nil
		}
		mode := info.Mode()
		switch {
		case mode&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			fmt.Fprintf(h, "L %q %q\n", rel, target)
		case mode.IsDir():
			fmt.Fprintf(h, "D %q %o\n", rel, mode.Perm())
		case mode.IsRegular():
			fmt.Fprintf(h, "F %q %o %s\n", rel, mode.Perm(), fileDigest(p, info))
		default:
			fmt.Fprintf(h, "O %q %v\n", rel, mode)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileDigest(p string, info fs.FileInfo) string {
	if info.Size() > MaxHashedFileSize {
		return fmt.Sprintf("size=%d mtime=%d", info.Size(), info.ModTime().UnixNano())
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Sprintf("unreadable size=%d mtime=%d", info.Size(), info.ModTime().UnixNano())
	}
	defer f.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Sprintf("unreadable size=%d mtime=%d", info.Size(), info.ModTime().UnixNano())
	}
	return hex.EncodeToString(sum.Sum(nil))
}
