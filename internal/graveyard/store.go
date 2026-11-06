// Package graveyard keeps a tombstone for every spirit whose postmortem was
// generated (Requirement 9). The tombstones live in one local JSON file. The
// package never talks to the cluster.
package graveyard

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/zhangjuji/k8s-seance/internal/autopsy"
)

const (
	// dirMode and fileMode keep the file private: postmortems hold log lines.
	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
	// idHexLen is the length of a tombstone ID in hex characters.
	idHexLen = 12
)

// idPattern matches a tombstone ID.
var idPattern = regexp.MustCompile(`^[0-9a-f]{12}$`)

// ErrNotFound is returned by Get when no tombstone has the ID.
var ErrNotFound = errors.New("graveyard: no such tombstone")

// Tombstone is one buried spirit. A Pod (namespace, name and UID) has at most
// one tombstone. DiedAt is the zero time when the death time is unknown.
type Tombstone struct {
	ID           string               `json:"id"`
	PodName      string               `json:"podName"`
	Namespace    string               `json:"namespace"`
	UID          string               `json:"uid,omitempty"`
	BornAt       time.Time            `json:"bornAt,omitzero"`
	DiedAt       time.Time            `json:"diedAt,omitzero"`
	Cause        autopsy.CauseOfDeath `json:"cause"`
	Epitaph      string               `json:"epitaph"`
	PostmortemMD string               `json:"postmortemMD"`
	BuriedAt     time.Time            `json:"buriedAt"`
}

// ID returns the tombstone ID of a Pod: the first 12 hex characters of the
// SHA-256 of namespace, name and UID.
func ID(namespace, name, uid string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + name + "\x00" + uid))
	return hex.EncodeToString(sum[:])[:idHexLen]
}

// ValidID reports whether id has the shape of a tombstone ID.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Store reads and writes the graveyard file. The methods are safe for
// concurrent use within one process.
type Store struct {
	path string
	now  func() time.Time
	mu   sync.Mutex
}

// Open returns a store for the file at path. The file and its directory are
// created on the first Bury. A missing file reads as an empty graveyard.
func Open(path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("graveyard: empty path")
	}
	return &Store{path: path, now: time.Now}, nil
}

// Path returns the file the store uses.
func (s *Store) Path() string { return s.path }

// List returns every tombstone, the most recent death first. Tombstones with an
// unknown death time come last. Ties are broken by burial time, then by ID.
func (s *Store) List() ([]Tombstone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	sortByDeath(all)
	return all, nil
}

// Get returns the tombstone with id, or ErrNotFound.
func (s *Store) Get(id string) (Tombstone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return Tombstone{}, err
	}
	for _, t := range all {
		if t.ID == id {
			return t, nil
		}
	}
	return Tombstone{}, ErrNotFound
}

// Bury stores t and returns it with its ID and burial time set. When the Pod
// already has a tombstone, it is replaced. The file is read in full, merged and
// written to a temporary file that then replaces the original, so a crash never
// leaves half a file. If the file cannot be read or parsed, nothing is written.
func (s *Store) Bury(t Tombstone) (Tombstone, error) {
	if t.Namespace == "" || t.PodName == "" {
		return Tombstone{}, errors.New("graveyard: tombstone needs a namespace and a pod name")
	}
	t.ID = ID(t.Namespace, t.PodName, t.UID)
	t.BuriedAt = s.now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()
	all, err := s.load()
	if err != nil {
		return Tombstone{}, err
	}
	replaced := false
	for i := range all {
		if all[i].ID == t.ID {
			all[i] = t
			replaced = true
			break
		}
	}
	if !replaced {
		all = append(all, t)
	}
	if err := s.save(all); err != nil {
		return Tombstone{}, err
	}
	return t, nil
}

// load reads the file. A missing file is an empty graveyard. The caller holds mu.
func (s *Store) load() ([]Tombstone, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("graveyard: read %s: %w", s.path, err)
	}
	var all []Tombstone
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, fmt.Errorf("graveyard: parse %s: %w", s.path, err)
	}
	return all, nil
}

// save writes all to a temporary file in the same directory and renames it over
// the graveyard file. The caller holds mu.
func (s *Store) save(all []Tombstone) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("graveyard: mkdir %s: %w", dir, err)
	}
	data, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return fmt.Errorf("graveyard: encode: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(dir, ".graveyard-*.json")
	if err != nil {
		return fmt.Errorf("graveyard: temp file: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("graveyard: chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("graveyard: write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("graveyard: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("graveyard: close: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("graveyard: replace %s: %w", s.path, err)
	}
	ok = true
	return nil
}

// sortByDeath orders tombstones by death time, newest first; unknown deaths last.
func sortByDeath(all []Tombstone) {
	sort.SliceStable(all, func(i, j int) bool {
		a, b := all[i], all[j]
		if a.DiedAt.IsZero() != b.DiedAt.IsZero() {
			return !a.DiedAt.IsZero()
		}
		if !a.DiedAt.Equal(b.DiedAt) {
			return a.DiedAt.After(b.DiedAt)
		}
		if !a.BuriedAt.Equal(b.BuriedAt) {
			return a.BuriedAt.After(b.BuriedAt)
		}
		return a.ID < b.ID
	})
}
