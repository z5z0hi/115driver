package driver

import (
	"errors"
	"fmt"
	"testing"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/stretchr/testify/assert"
)

func TestContiguousUploadedParts(t *testing.T) {
	r := assert.New(t)

	// empty
	r.Empty(contiguousUploadedParts(nil))

	// in-order
	parts := []UploadedPart{{1, "a", 1}, {2, "b", 2}, {3, "c", 3}}
	r.Equal(parts, contiguousUploadedParts(parts))

	// out-of-order is tolerated
	r.Equal(parts, contiguousUploadedParts([]UploadedPart{{3, "c", 3}, {1, "a", 1}, {2, "b", 2}}))

	// gap truncates the tail
	got := contiguousUploadedParts([]UploadedPart{{1, "a", 1}, {3, "c", 3}})
	r.Equal([]UploadedPart{{1, "a", 1}}, got)

	// missing first part yields nothing
	r.Empty(contiguousUploadedParts([]UploadedPart{{2, "b", 2}}))

	// duplicates are dropped
	got = contiguousUploadedParts([]UploadedPart{{1, "a", 1}, {1, "dup", 9}, {2, "b", 2}})
	r.Equal([]UploadedPart{{1, "a", 1}, {2, "b", 2}}, got)

	// parts must start at 1
	r.Empty(contiguousUploadedParts([]UploadedPart{{0, "zero", 5}}))
}

func TestUploadCheckpointUploadedBytes(t *testing.T) {
	r := assert.New(t)
	cp := &UploadCheckpoint{Parts: []UploadedPart{{1, "a", 100}, {2, "b", 200}}}
	r.Equal(int64(300), cp.UploadedBytes())

	// non-contiguous tail is not counted
	cp.Parts = append(cp.Parts, UploadedPart{4, "d", 400})
	r.Equal(int64(300), cp.UploadedBytes())
}

func TestUploadCheckpointSessionFor(t *testing.T) {
	r := assert.New(t)
	chunks := []oss.FileChunk{{Number: 1, Size: 100, Offset: 0}, {Number: 2, Size: 100, Offset: 100}}
	valid := &UploadCheckpoint{
		Version:   checkpointVersion,
		FileName:  "file.bin",
		FileSize:  200,
		DirID:     "111",
		OSSParams: UploadOSSParams{Bucket: "b", Object: "o"},
		UploadID:  "uid",
		PartSize:  100,
		Parts:     []UploadedPart{{1, "a", 100}},
	}

	// resumable session
	uploadID, params, parts, ok := valid.sessionFor("111", "file.bin", 200, chunks)
	r.True(ok)
	r.Equal("uid", uploadID)
	r.Equal("b", params.Bucket)
	r.Equal([]UploadedPart{{1, "a", 100}}, parts)

	// target directory changed -> no resume (digest-only path)
	_, _, _, ok = valid.sessionFor("222", "file.bin", 200, chunks)
	r.False(ok)

	// name or size mismatch
	_, _, _, ok = valid.sessionFor("111", "other.bin", 200, chunks)
	r.False(ok)
	_, _, _, ok = valid.sessionFor("111", "file.bin", 300, chunks)
	r.False(ok)

	// wrong version
	stale := *valid
	stale.Version = 0
	_, _, _, ok = stale.sessionFor("111", "file.bin", 200, chunks)
	r.False(ok)

	// missing upload session
	noID := *valid
	noID.UploadID = ""
	_, _, _, ok = noID.sessionFor("111", "file.bin", 200, chunks)
	r.False(ok)

	// chunk layout mismatch (e.g. different SDK chunking)
	_, _, _, ok = valid.sessionFor("111", "file.bin", 200, []oss.FileChunk{{Number: 1, Size: 50, Offset: 0}})
	r.False(ok)

	// more parts than chunks -> corrupted
	over := *valid
	over.Parts = []UploadedPart{{1, "a", 100}, {2, "b", 100}, {3, "c", 100}}
	_, _, _, ok = over.sessionFor("111", "file.bin", 200, chunks)
	r.False(ok)

	// nil checkpoint never resumes
	_, _, _, ok = (*UploadCheckpoint)(nil).sessionFor("111", "file.bin", 200, chunks)
	r.False(ok)
}

func TestIsOSSNoSuchUpload(t *testing.T) {
	r := assert.New(t)

	r.True(isOSSNoSuchUpload(oss.ServiceError{Code: "NoSuchUpload"}))

	wrapped := fmt.Errorf("upload failed: %w", oss.ServiceError{Code: "NoSuchUpload"})
	r.True(isOSSNoSuchUpload(wrapped))

	r.False(isOSSNoSuchUpload(oss.ServiceError{Code: "AccessDenied"}))
	r.False(isOSSNoSuchUpload(errors.New("some network error")))
	r.False(isOSSNoSuchUpload(nil))
}
