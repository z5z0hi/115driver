package driver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	hash "github.com/SheltonZhu/115driver/pkg/crypto"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// CheckpointVersion is the schema version of UploadCheckpoint. A checkpoint
// recorded by a different version is never resumed.
const CheckpointVersion = 1

// checkpointVersion is an alias kept for internal readability.
const checkpointVersion = CheckpointVersion

// errRapidUploaded is an internal sentinel signaling that the file has been
// stored via rapid upload (server-side deduplication), so no OSS transfer is
// needed.
var errRapidUploaded = errors.New("rapid uploaded")

// UploadedPart records a single completed OSS multipart part.
type UploadedPart struct {
	Number int    `json:"number"`
	ETag   string `json:"etag"`
	Size   int64  `json:"size"`
}

// UploadCheckpoint carries the resumable state of a multipart upload. It is
// updated in place by [Pan115Client.RapidUploadOrByMultipartResumable] and is
// JSON-serializable so callers can persist it between attempts.
type UploadCheckpoint struct {
	Version   int               `json:"version"`
	Path      string            `json:"path,omitempty"` // local file path (as opened)
	FileName  string            `json:"file_name"`
	FileSize  int64             `json:"file_size"`
	ModTime   int64             `json:"mod_time"` // local file mtime, unix nanoseconds
	DirID     string            `json:"dir_id"`   // target directory of the upload session
	Digest    hash.DigestResult `json:"digest"`   // stored digest so resumes skip re-hashing
	OSSParams UploadOSSParams   `json:"oss_params"`
	UploadID  string            `json:"upload_id"`
	PartSize  int64             `json:"part_size"` // chunk layout validation
	Parts     []UploadedPart    `json:"parts"`
	UpdatedAt int64             `json:"updated_at,omitempty"`
}

// UploadedBytes returns the number of bytes covered by the contiguous prefix
// of uploaded parts.
func (cp *UploadCheckpoint) UploadedBytes() int64 {
	var n int64
	for _, p := range contiguousUploadedParts(cp.Parts) {
		n += p.Size
	}
	return n
}

// contiguousUploadedParts returns the longest prefix of parts numbered
// 1..N. Parts are order tolerant; gaps and duplicates are dropped, matching
// the sequential upload mode used by multipart uploads.
func contiguousUploadedParts(parts []UploadedPart) []UploadedPart {
	if len(parts) == 0 {
		return nil
	}
	sorted := make([]UploadedPart, len(parts))
	copy(sorted, parts)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Number < sorted[j].Number })
	out := make([]UploadedPart, 0, len(sorted))
	expect := 1
	for _, p := range sorted {
		if p.Number < expect {
			continue // duplicate
		}
		if p.Number != expect {
			break // gap: everything after is unusable
		}
		out = append(out, p)
		expect++
	}
	return out
}

// sessionFor returns the OSS session encoded in cp if it is still valid for
// the given target and chunk layout.
func (cp *UploadCheckpoint) sessionFor(dirID, fileName string, fileSize int64, chunks []oss.FileChunk) (uploadID string, params *UploadOSSParams, parts []UploadedPart, ok bool) {
	if cp == nil || cp.Version != checkpointVersion || cp.UploadID == "" {
		return "", nil, nil, false
	}
	if cp.DirID != dirID || cp.FileName != fileName || cp.FileSize != fileSize {
		return "", nil, nil, false
	}
	if cp.OSSParams.Bucket == "" || cp.OSSParams.Object == "" {
		return "", nil, nil, false
	}
	if len(chunks) == 0 || chunks[0].Size != cp.PartSize {
		return "", nil, nil, false
	}
	parts = contiguousUploadedParts(cp.Parts)
	if len(parts) > len(chunks) {
		return "", nil, nil, false
	}
	return cp.UploadID, &cp.OSSParams, parts, true
}

// RapidUploadOrByMultipartResumable uploads a file with checkpoint support.
//
// The upload first tries rapid upload (server-side deduplication). When that
// fails, the file is uploaded sequentially via OSS multipart upload and
// progress is recorded into ckpt after every part. If a previous attempt was
// interrupted, pass its checkpoint back: the upload resumes from the recorded
// parts without re-reading the file to compute hashes, provided name, size
// and target directory are unchanged. When the target directory differs from
// the checkpointed one, the stored digest is reused to re-initiate the upload
// into the new directory and already uploaded parts are discarded.
//
// ckpt may be nil, in which case the upload behaves like
// [RapidUploadOrByMultipart] without any persistence. onSave, if not nil, is
// called after the OSS session is created and after each completed part so
// the caller can persist ckpt. The caller is responsible for removing any
// persisted checkpoint once this function returns nil.
func (c *Pan115Client) RapidUploadOrByMultipartResumable(
	dirID, fileName string, fileSize int64, f *os.File,
	ckpt *UploadCheckpoint,
	onSave func(*UploadCheckpoint) error,
	opts ...UploadMultipartOption,
) error {
	if ok, err := c.UploadAvailable(); err != nil || !ok {
		return err
	}
	if fileSize > c.UploadMetaInfo.SizeLimit {
		return ErrUploadTooLarge
	}

	// SplitFile is deterministic given the file size, so the layout recorded
	// in a checkpoint always matches the layout recomputed here. It fails for
	// files too small to split (<100KB); those go through the plain upload
	// path in resumableMultipartOnce.
	var chunks []oss.FileChunk
	if ckpt != nil && ckpt.UploadID != "" {
		chunks, _ = SplitFile(f.Name(), fileSize)
	}

	// Classify the checkpoint: resume the OSS session, keep only the stored
	// digest (e.g. target directory changed), or start from scratch.
	var (
		digest   *hash.DigestResult
		params   *UploadOSSParams
		uploadID string
		parts    []UploadedPart
	)
	uploadID, params, parts, _ = ckpt.sessionFor(dirID, fileName, fileSize, chunks)
	// Always keep the stored digest around: it powers the re-init paths
	// (target directory changed, or the OSS session disappeared) without
	// re-hashing the file.
	if ckpt != nil && ckpt.Version == checkpointVersion && ckpt.Digest.QuickID != "" {
		digest = &ckpt.Digest
	}

	options := DefaultUploadMultipartOptions()
	if len(opts) > 0 {
		for _, opt := range opts {
			opt(options)
		}
	}

	for attempt := 0; ; attempt++ {
		err := c.resumableMultipartOnce(dirID, fileName, fileSize, f, ckpt, onSave,
			chunks, digest, params, uploadID, parts, options)
		if err == errRapidUploaded {
			return nil
		}
		// A resumed session may no longer exist server-side (e.g. cleaned up
		// while the upload was paused): retry once with a fresh session
		// initiated from the stored digest.
		if attempt == 0 && isOSSNoSuchUpload(err) && digest != nil {
			uploadID, params, parts = "", nil, nil
			continue
		}
		return err
	}
}

// resumableMultipartOnce performs one full upload attempt: initiating the OSS
// session when needed, uploading all remaining parts sequentially and
// completing the upload via the 115 callback.
func (c *Pan115Client) resumableMultipartOnce(
	dirID, fileName string, fileSize int64, f *os.File,
	ckpt *UploadCheckpoint,
	onSave func(*UploadCheckpoint) error,
	chunks []oss.FileChunk,
	digest *hash.DigestResult,
	params *UploadOSSParams,
	uploadID string,
	parts []UploadedPart,
	options *UploadMultipartOptions,
) error {
	if params == nil {
		// (Re-)initiate the upload, reusing a stored digest when available
		// to avoid hashing the file again.
		if digest == nil {
			d, err := c.GetDigestResult(f)
			if err != nil {
				return err
			}
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			digest = d
		}
		fastInfo, err := c.RapidUpload(digest.Size, fileName, dirID, digest.PreID, digest.QuickID, f)
		if err != nil {
			return err
		}
		ok, err := fastInfo.Ok()
		if err != nil {
			return err
		}
		if ok {
			// 闪传成功
			if ckpt != nil {
				resetCheckpoint(ckpt) // uploaded; caller should remove it
			}
			return errRapidUploaded
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		params = &fastInfo.UploadOSSParams
		uploadID = ""
		parts = nil
		if len(chunks) == 0 {
			// 文件太小无法分片（<100KB）：改用普通模式上传，不支持续传
			if cs, cerr := SplitFile(f.Name(), fileSize); cerr == nil && len(cs) > 0 {
				chunks = cs
			} else {
				return c.UploadByOSS(params, f, dirID)
			}
		}
		if ckpt != nil {
			ckpt.Version = checkpointVersion
			ckpt.FileName = fileName
			ckpt.FileSize = fileSize
			ckpt.DirID = dirID
			ckpt.Digest = *digest
			ckpt.OSSParams = *params
			ckpt.UploadID = ""
			ckpt.PartSize = 0
			ckpt.Parts = nil
			fillFileIdentity(ckpt, f)
		}
	}

	ossToken, err := c.GetOSSToken()
	if err != nil {
		return err
	}
	ossClient, err := oss.New(
		c.getOSSEndpoint(c.UseInternalUpload),
		ossToken.AccessKeyID,
		ossToken.AccessKeySecret,
		oss.EnableMD5(true),
		oss.EnableCRC(true),
	)
	if err != nil {
		return err
	}
	bucket, err := ossClient.Bucket(params.Bucket)
	if err != nil {
		return err
	}

	uploadedBytes := int64(0)
	var imur oss.InitiateMultipartUploadResult
	if uploadID == "" {
		if imur, err = bucket.InitiateMultipartUpload(params.Object,
			oss.SetHeader(OssSecurityTokenHeaderName, ossToken.SecurityToken),
			oss.UserAgentHeader(OSSUserAgent),
			oss.EnableSha1(),
			oss.Sequential(), // oss 启用Sequential必须按顺序上传
		); err != nil {
			return err
		}
		uploadID = imur.UploadID
		if ckpt != nil {
			ckpt.UploadID = uploadID
			ckpt.PartSize = chunks[0].Size
			if err := saveCheckpoint(onSave, ckpt); err != nil {
				return err
			}
		}
	} else {
		imur = oss.InitiateMultipartUploadResult{
			Bucket:   params.Bucket,
			Key:      params.Object,
			UploadID: uploadID,
		}
		// 以服务端分片记录为准：进程被杀时可能存在"请求已到达但未记录"的分片
		if serverParts, lerr := reconcileUploadedParts(bucket, imur, chunks, ossToken); lerr == nil && len(serverParts) >= len(parts) {
			parts = serverParts
			if ckpt != nil && len(ckpt.Parts) != len(parts) {
				ckpt.Parts = parts
				if err := saveCheckpoint(onSave, ckpt); err != nil {
					return err
				}
			}
		}
		for _, p := range parts {
			uploadedBytes += p.Size
		}
		// 跳过已上传分片覆盖的字节
		if _, err := f.Seek(uploadedBytes, io.SeekStart); err != nil {
			return err
		}
	}

	// ossToken一小时后就会失效，所以定时重新获取
	ticker := time.NewTicker(options.TokenRefreshTime)
	defer ticker.Stop()
	timeout := time.NewTimer(options.Timeout)
	defer timeout.Stop()

	uploaded := make([]oss.UploadPart, 0, len(chunks))
	for _, p := range parts {
		uploaded = append(uploaded, oss.UploadPart{PartNumber: p.Number, ETag: p.ETag})
	}

	for len(parts) < len(chunks) {
		chunk := chunks[len(parts)]
		select {
		case <-timeout.C:
			return fmt.Errorf("time out")
		default:
		}

		var part oss.UploadPart
		for retry := 0; retry < 3; retry++ { // 出现错误就继续尝试，共尝试3次
			select {
			case <-ticker.C:
				// 到时重新获取ossToken
				if ossToken, err = c.GetOSSToken(); err != nil {
					return err
				}
			default:
			}

			buf := make([]byte, chunk.Size)
			if _, err = f.ReadAt(buf, chunk.Offset); err != nil && !errors.Is(err, io.EOF) {
				continue
			}
			if part, err = bucket.UploadPart(
				imur,
				bytes.NewBuffer(buf),
				chunk.Size,
				chunk.Number,
				OssOption(params, ossToken)...); err == nil {
				break
			}
			if isOSSNoSuchUpload(err) {
				return err
			}
		}
		if err != nil {
			// 分片可能已上传成功但响应丢失（顺序模式下重传同片号会报
			// PartAlreadyExist）：以服务端记录为准对账补齐后继续。
			if isOSSPartAlreadyExist(err) {
				if serverParts, lerr := reconcileUploadedParts(bucket, imur, chunks, ossToken); lerr == nil && len(serverParts) > len(parts) {
					for _, sp := range serverParts[len(parts):] {
						uploaded = append(uploaded, oss.UploadPart{PartNumber: sp.Number, ETag: sp.ETag})
					}
					parts = serverParts
					uploadedBytes = 0
					for _, p := range parts {
						uploadedBytes += p.Size
					}
					if ckpt != nil {
						ckpt.Parts = parts
						if serr := saveCheckpoint(onSave, ckpt); serr != nil {
							return serr
						}
					}
					if options.ProgressFunc != nil {
						options.ProgressFunc(uploadedBytes)
					}
					continue
				}
			}
			return fmt.Errorf("上传 %s 的第%d个分片时出现错误：%w", f.Name(), chunk.Number, err)
		}
		uploaded = append(uploaded, part)
		parts = append(parts, UploadedPart{Number: chunk.Number, ETag: part.ETag, Size: chunk.Size})
		uploadedBytes += chunk.Size
		if ckpt != nil {
			ckpt.Parts = parts
			if err := saveCheckpoint(onSave, ckpt); err != nil {
				return err
			}
		}
		if options.ProgressFunc != nil {
			options.ProgressFunc(uploadedBytes)
		}
	}

	var bodyBytes []byte
	if _, err := bucket.CompleteMultipartUpload(imur, uploaded,
		append(
			OssOption(params, ossToken),
			oss.CallbackResult(&bodyBytes),
		)...); err != nil {
		return err
	}

	var uploadResult UploadResult
	if err = json.Unmarshal(bodyBytes, &uploadResult); err != nil {
		return err
	}
	return uploadResult.Err(string(bodyBytes))
}

// isOSSNoSuchUpload reports whether err is an OSS error indicating the upload
// session (uploadId) no longer exists on the server.
func isOSSNoSuchUpload(err error) bool {
	var svcErr oss.ServiceError
	return errors.As(err, &svcErr) && svcErr.Code == "NoSuchUpload"
}

// isOSSPartAlreadyExist reports whether err is an OSS error indicating the
// part number has already been uploaded (sequential mode forbids overwrites).
func isOSSPartAlreadyExist(err error) bool {
	var svcErr oss.ServiceError
	return errors.As(err, &svcErr) && svcErr.Code == "PartAlreadyExist"
}

// reconcileUploadedParts returns the server-authoritative contiguous prefix of
// uploaded parts, validated against the chunk layout.
func reconcileUploadedParts(bucket *oss.Bucket, imur oss.InitiateMultipartUploadResult, chunks []oss.FileChunk, ossToken *UploadOSSTokenResp) ([]UploadedPart, error) {
	serverParts, err := listOSSUploadedParts(bucket, imur, ossToken)
	if err != nil {
		return nil, err
	}
	var out []UploadedPart
	for _, sp := range serverParts {
		if sp.PartNumber != len(out)+1 || sp.PartNumber > len(chunks) {
			break
		}
		if int64(sp.Size) != chunks[sp.PartNumber-1].Size {
			break
		}
		out = append(out, UploadedPart{
			Number: sp.PartNumber,
			ETag:   strings.Trim(sp.ETag, `"`),
			Size:   int64(sp.Size),
		})
	}
	return out, nil
}

// listOSSUploadedParts lists all uploaded parts from OSS with pagination.
func listOSSUploadedParts(bucket *oss.Bucket, imur oss.InitiateMultipartUploadResult, ossToken *UploadOSSTokenResp) ([]oss.UploadedPart, error) {
	opts := []oss.Option{
		oss.SetHeader(OssSecurityTokenHeaderName, ossToken.SecurityToken),
		oss.UserAgentHeader(OSSUserAgent),
	}
	var out []oss.UploadedPart
	marker := 0
	for {
		res, err := bucket.ListUploadedParts(imur, append(opts, oss.MaxParts(1000), oss.PartNumberMarker(marker))...)
		if err != nil {
			return nil, err
		}
		out = append(out, res.UploadedParts...)
		if !res.IsTruncated {
			sort.Slice(out, func(i, j int) bool { return out[i].PartNumber < out[j].PartNumber })
			return out, nil
		}
		next, cerr := strconv.Atoi(res.NextPartNumberMarker)
		if cerr != nil {
			return nil, fmt.Errorf("invalid NextPartNumberMarker %q: %w", res.NextPartNumberMarker, cerr)
		}
		marker = next
	}
}

// saveCheckpoint stamps and persists the checkpoint via onSave.
func saveCheckpoint(onSave func(*UploadCheckpoint) error, ckpt *UploadCheckpoint) error {
	if onSave == nil {
		return nil
	}
	ckpt.UpdatedAt = time.Now().UnixNano()
	return onSave(ckpt)
}

// fillFileIdentity refreshes the local file identity fields of the checkpoint.
func fillFileIdentity(ckpt *UploadCheckpoint, f *os.File) {
	if fi, err := f.Stat(); err == nil {
		ckpt.ModTime = fi.ModTime().UnixNano()
		if ckpt.Path == "" {
			ckpt.Path = f.Name()
		}
	}
}

// resetCheckpoint clears the checkpoint after the upload completed.
func resetCheckpoint(ckpt *UploadCheckpoint) {
	*ckpt = UploadCheckpoint{}
}
