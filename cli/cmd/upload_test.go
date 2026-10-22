package cmd

import (
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeDirLister struct {
	files    []driver.File
	listErr  error
	gotDirID string
}

func (f *fakeDirLister) List(dirID string, opts ...driver.ListOption) (*[]driver.File, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	f.gotDirID = dirID
	return &f.files, nil
}

func TestFindDirConflicts(t *testing.T) {
	r := assert.New(t)
	files := []driver.File{
		{Name: "videos", IsDirectory: true},
		{Name: "a.mkv", IsDirectory: false},
		{Name: "docs", IsDirectory: true},
	}

	r.Equal([]string{"videos"}, findDirConflicts(files, []string{"videos", "missing"}))
	r.Equal([]string{"videos", "docs"}, findDirConflicts(files, []string{"videos", "docs"}))
	r.Empty(findDirConflicts(files, []string{"a.mkv"})) // 同名文件不算目录冲突
	r.Empty(findDirConflicts(files, nil))
}

func TestConflictsInRemote(t *testing.T) {
	r := require.New(t)
	l := &fakeDirLister{files: []driver.File{
		{Name: "mydir", IsDirectory: true},
	}}

	conflicts, err := conflictsInRemote(l, "123", []string{"mydir"})
	r.NoError(err)
	r.Equal([]string{"mydir"}, conflicts)
	r.Equal("123", l.gotDirID)

	conflicts, err = conflictsInRemote(l, "123", []string{"other"})
	r.NoError(err)
	r.Empty(conflicts)

	_, err = conflictsInRemote(&fakeDirLister{listErr: assert.AnError}, "123", []string{"x"})
	r.Error(err)
}
