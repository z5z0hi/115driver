package checkpoint

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCheckpoint(path string) *driver.UploadCheckpoint {
	return &driver.UploadCheckpoint{
		Version:  driver.CheckpointVersion,
		Path:     path,
		FileName: "movie.mkv",
		FileSize: 12345,
		ModTime:  1700000000000000000,
		DirID:    "111",
		UploadID: "upload-id",
		PartSize: 1024,
		Parts:    []driver.UploadedPart{{Number: 1, ETag: "a", Size: 1024}},
	}
}

func TestStoreSaveRemove(t *testing.T) {
	r := require.New(t)
	dir := t.TempDir()
	store := &Store{Dir: dir}

	cp := testCheckpoint("/data/movie.mkv")
	r.NoError(store.Save(cp))

	// file exists under the sha1-of-path key and round-trips
	name := key("/data/movie.mkv")
	data, err := os.ReadFile(filepath.Join(dir, name))
	r.NoError(err)
	r.Contains(string(data), `"upload_id":"upload-id"`)

	// second save overwrites (atomic rename)
	cp.Parts = append(cp.Parts, driver.UploadedPart{Number: 2, ETag: "b", Size: 1024})
	r.NoError(store.Save(cp))

	r.NoError(store.Remove("/data/movie.mkv"))
	_, err = os.Stat(filepath.Join(dir, name))
	r.True(os.IsNotExist(err))

	// removing again is a no-op
	r.NoError(store.Remove("/data/movie.mkv"))
}

func TestStoreMatchByIdentity(t *testing.T) {
	r := require.New(t)
	store := &Store{Dir: t.TempDir()}

	cp := testCheckpoint("/data/movie.mkv")
	r.NoError(store.Save(cp))

	// same identity matches, even under a different path
	got, gotPath, err := store.Match("movie.mkv", 12345, 1700000000000000000)
	r.NoError(err)
	r.NotNil(got)
	r.Equal("/data/movie.mkv", gotPath)
	r.Equal("upload-id", got.UploadID)
	r.Equal(int64(1024), got.UploadedBytes())

	// any differing field does not match
	for _, m := range []struct {
		name    string
		size    int64
		modTime int64
	}{
		{"other.mkv", 12345, 1700000000000000000},
		{"movie.mkv", 999, 1700000000000000000},
		{"movie.mkv", 12345, 1111},
	} {
		got, _, err := store.Match(m.name, m.size, m.modTime)
		r.NoError(err)
		r.Nil(got, "expected no match for %+v", m)
	}
}

func TestStoreMatchNewestWins(t *testing.T) {
	r := require.New(t)
	store := &Store{Dir: t.TempDir()}

	first := testCheckpoint("/data/old.mkv")
	first.UpdatedAt = 100
	r.NoError(store.Save(first))

	second := testCheckpoint("/data/new.mkv")
	second.UpdatedAt = 200
	r.NoError(store.Save(second))

	got, gotPath, err := store.Match("movie.mkv", 12345, 1700000000000000000)
	r.NoError(err)
	r.NotNil(got)
	r.Equal("/data/new.mkv", gotPath)
}

func TestStoreMatchEmptyOrCorrupt(t *testing.T) {
	r := require.New(t)

	// missing directory behaves like "no checkpoints"
	store := &Store{Dir: filepath.Join(t.TempDir(), "missing")}
	got, _, err := store.Match("movie.mkv", 1, 2)
	r.NoError(err)
	r.Nil(got)

	// corrupt json files are skipped
	store = &Store{Dir: t.TempDir()}
	r.NoError(os.WriteFile(filepath.Join(store.Dir, "garbage.json"), []byte("{not json"), 0600))
	got, _, err = store.Match("movie.mkv", 1, 2)
	r.NoError(err)
	r.Nil(got)
}

func TestDefaultDir(t *testing.T) {
	dir, err := DefaultDir()
	require.NoError(t, err)
	assert.Contains(t, dir, ".115driver")
	assert.True(t, filepath.IsAbs(dir))
}
