// Package checkpoint persists resumable upload state on local disk so
// interrupted uploads can be continued by a later invocation.
package checkpoint

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

// DefaultDirName is the directory (relative to the user's config dir)
// where checkpoint files are stored.
const DefaultDirName = "uploads"

// Store manages checkpoint files inside a directory.
type Store struct {
	Dir string
}

// DefaultDir returns the default checkpoint directory
// (~/.115driver/uploads).
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot resolve home directory: %w", err)
	}
	return filepath.Join(home, ".115driver", DefaultDirName), nil
}

// NewStore returns a Store rooted at the default directory.
func NewStore() (*Store, error) {
	dir, err := DefaultDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// key returns the checkpoint file name for a local path: the hex-encoded
// SHA1 of the absolute path.
func key(absPath string) string {
	sum := sha1.Sum([]byte(absPath))
	return hex.EncodeToString(sum[:]) + ".json"
}

// Save atomically persists the checkpoint (tmp file + rename).
func (s *Store) Save(cp *driver.UploadCheckpoint) error {
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	data, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	name := key(cp.Path)
	tmp := filepath.Join(s.Dir, fmt.Sprintf(".%s.%d.tmp", name, os.Getpid()))
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.Dir, name))
}

// Remove deletes the checkpoint recorded for the given local path, if any.
func (s *Store) Remove(absPath string) error {
	err := os.Remove(filepath.Join(s.Dir, key(absPath)))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Match returns the newest checkpoint whose file name, size and mod time all
// match the given local file — no file content is read or hashed. The second
// return value is the local path recorded inside the checkpoint (which may
// differ from the current file's path if the file was moved or renamed).
func (s *Store) Match(fileName string, fileSize, modTime int64) (*driver.UploadCheckpoint, string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	var best *driver.UploadCheckpoint
	bestPath := ""
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.Dir, entry.Name()))
		if err != nil {
			continue // tolerate unreadable entries
		}
		var cp driver.UploadCheckpoint
		if err := json.Unmarshal(data, &cp); err != nil {
			continue
		}
		if cp.Version != driver.CheckpointVersion ||
			cp.FileName != fileName || cp.FileSize != fileSize || cp.ModTime != modTime {
			continue
		}
		if best == nil || cp.UpdatedAt > best.UpdatedAt {
			cpCopy := cp
			best = &cpCopy
			bestPath = cp.Path
		}
	}
	if best == nil {
		return nil, "", nil
	}
	return best, bestPath, nil
}
