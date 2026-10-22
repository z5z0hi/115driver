package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/checkpoint"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/cheggaaa/pb/v3"
	"github.com/spf13/cobra"
)

var uploadCmd = &cobra.Command{
	Use:   "upload <local_path>... <remote_dir>",
	Short: "Upload files or directories to a remote directory (supports resume)",
	Long: `Upload one or more local files or directories to a remote directory.

The last argument is the remote destination directory; every preceding
argument is a local file or directory. Directories are uploaded recursively;
if a remote directory with the same name already exists at the destination,
the command fails without uploading anything.

Uploads are resumable: if an upload is interrupted, re-running the same
command continues from where it stopped. A checkpoint matches by file name,
size and modification time (no re-hashing). If the destination directory
differs from the interrupted attempt, the file is uploaded into the current
one.`,
	Args: cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		remoteDir := args[len(args)-1]
		localArgs := args[:len(args)-1]

		dirID, err := resolver.ResolveDir(client, remoteDir)
		if err != nil {
			return &exitError{code: output.ExitNotFound, msg: fmt.Sprintf("Remote directory not found: %s", remoteDir)}
		}

		store, err := checkpoint.NewStore()
		if err != nil {
			return &exitError{code: output.ExitError, msg: err.Error()}
		}

		type localItem struct {
			absPath string
			info    os.FileInfo
		}
		items := make([]localItem, 0, len(localArgs))
		for _, arg := range localArgs {
			abs, err := filepath.Abs(arg)
			if err != nil {
				return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot resolve path %s: %v", arg, err)}
			}
			info, err := os.Stat(abs)
			if err != nil {
				return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot open local path: %v", err)}
			}
			items = append(items, localItem{absPath: abs, info: info})
		}

		// 远程已有同名目录时直接报错退出，不开始任何上传
		var dirNames []string
		for _, it := range items {
			if it.info.IsDir() {
				dirNames = append(dirNames, filepath.Base(it.absPath))
			}
		}
		if len(dirNames) > 0 {
			conflicts, err := conflictsInRemote(client, dirID, dirNames)
			if err != nil {
				return &exitError{code: output.ExitError, msg: fmt.Sprintf("Failed to check remote directory: %v", err)}
			}
			if len(conflicts) > 0 {
				return &exitError{code: output.ExitError, msg: fmt.Sprintf("Remote directory already exists: %s. Remove or rename it first.", strings.Join(conflicts, ", "))}
			}
		}

		up := &uploader{client: client, store: store, jsonMode: jsonOutput, remoteDir: remoteDir}
		for _, it := range items {
			if it.info.IsDir() {
				if err := up.uploadDir(dirID, it.absPath); err != nil {
					up.record(it.absPath, remoteDir, 0, err)
				}
			} else {
				up.uploadFile(dirID, it.absPath, remoteDir)
			}
		}

		failed := 0
		for _, o := range up.outcomes {
			if o.Error != "" {
				failed++
			}
		}
		if failed == 0 {
			if len(items) == 1 && !items[0].info.IsDir() {
				printer.PrintSuccess(map[string]interface{}{
					"local_path": items[0].absPath,
					"remote_dir": remoteDir,
					"size":       items[0].info.Size(),
				})
			} else {
				printer.PrintSuccess(map[string]interface{}{
					"remote_dir": remoteDir,
					"total":      len(up.outcomes),
					"results":    up.outcomes,
				})
			}
			return nil
		}

		var details strings.Builder
		for _, o := range up.outcomes {
			if o.Error != "" {
				details.WriteString(fmt.Sprintf("\n- %s: %s", o.LocalPath, o.Error))
			}
		}
		return &exitError{code: output.ExitError, msg: fmt.Sprintf("%d of %d upload(s) failed:%s", failed, len(up.outcomes), details.String())}
	},
}

type uploadOutcome struct {
	LocalPath string `json:"local_path"`
	RemoteDir string `json:"remote_dir"`
	Size      int64  `json:"size"`
	Error     string `json:"error,omitempty"`
}

type uploader struct {
	client    *driver.Pan115Client
	store     *checkpoint.Store
	jsonMode  bool
	remoteDir string
	outcomes  []uploadOutcome
}

// uploadDir uploads a local directory recursively into dirID. The caller
// must have verified that no same-name remote directory exists under dirID.
func (u *uploader) uploadDir(dirID, absDir string) error {
	base := filepath.Base(absDir)
	destID, err := u.client.Mkdir(dirID, base)
	if err != nil {
		return fmt.Errorf("cannot create remote directory %s: %w", base, err)
	}
	return u.walkDir(absDir, destID, "")
}

// walkDir uploads the contents of localDir into remoteID. relDir is the
// local directory's path relative to the upload root (for display only).
func (u *uploader) walkDir(localDir, remoteID, relDir string) error {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	displayDir := u.remoteDir
	if relDir != "" {
		displayDir = u.remoteDir + "/" + filepath.ToSlash(relDir)
	}
	for _, entry := range entries {
		full := filepath.Join(localDir, entry.Name())
		switch {
		case entry.IsDir():
			childID, err := u.ensureDir(remoteID, full)
			if err != nil {
				return err
			}
			if err := u.walkDir(full, childID, filepath.Join(relDir, entry.Name())); err != nil {
				return err
			}
		case entry.Type().IsRegular():
			u.uploadFile(remoteID, full, displayDir)
		default:
			// 跳过符号链接等非常规文件
		}
	}
	return nil
}

// ensureDir returns the remote directory ID for name under parentID,
// reusing an existing same-name directory at nested levels (cp -r semantics).
func (u *uploader) ensureDir(parentID, absDir string) (string, error) {
	name := filepath.Base(absDir)
	children, err := u.client.List(parentID)
	if err != nil {
		return "", fmt.Errorf("cannot list remote directory: %w", err)
	}
	for _, f := range *children {
		if f.IsDirectory && f.Name == name {
			return f.FileID, nil
		}
	}
	return u.client.Mkdir(parentID, name)
}

// uploadFile uploads one local file and records the outcome.
func (u *uploader) uploadFile(dirID, absPath, displayDir string) {
	info, err := os.Stat(absPath)
	if err != nil {
		u.record(absPath, displayDir, 0, err)
		return
	}
	u.record(absPath, displayDir, info.Size(), u.doUploadFile(dirID, absPath, info))
}

func (u *uploader) doUploadFile(dirID, absPath string, info os.FileInfo) error {
	f, err := os.Open(absPath)
	if err != nil {
		return fmt.Errorf("cannot open local file: %w", err)
	}
	defer f.Close()

	fileName := filepath.Base(absPath)

	var (
		ckptPath     string
		resumedBytes int64
	)

	// 断点匹配：文件名、大小、修改时间一致即可续传，不重新校验 hash。
	// 始终传入非 nil checkpoint（预填文件身份），SDK 才会填充并持久化会话状态。
	ckpt := &driver.UploadCheckpoint{
		Version:  driver.CheckpointVersion,
		Path:     absPath,
		FileName: fileName,
		FileSize: info.Size(),
		ModTime:  info.ModTime().UnixNano(),
	}
	if m, matchedPath, err := u.store.Match(fileName, info.Size(), info.ModTime().UnixNano()); err == nil && m != nil {
		ckpt = m
		ckptPath = matchedPath
		resumedBytes = m.UploadedBytes()
	}

	if !u.jsonMode {
		switch {
		case resumedBytes > 0 && ckpt.DirID == dirID && ckpt.UploadID != "":
			fmt.Printf("Resuming %s (%s) — %s already uploaded...\n",
				fileName, output.FormatFileSize(info.Size()), output.FormatFileSize(resumedBytes))
		case ckpt.DirID != "" && ckpt.DirID != dirID && ckpt.UploadID != "":
			fmt.Printf("Re-uploading %s (%s) into the new target (previous upload targeted another directory)...\n",
				fileName, output.FormatFileSize(info.Size()))
		default:
			if ckpt.Digest.QuickID == "" {
				fmt.Printf("Uploading %s (%s) — computing SHA1 checksum, please wait...\n",
					fileName, output.FormatFileSize(info.Size()))
			} else {
				fmt.Printf("Uploading %s (%s)...\n", fileName, output.FormatFileSize(info.Size()))
			}
		}
	}

	// 进度条在传输真正开始后才创建（懒加载）：计算 hash 期间不显示误导性的 0% 进度条
	var opts []driver.UploadMultipartOption
	if !u.jsonMode {
		var bar *pb.ProgressBar
		defer func() { output.FinishProgress(bar) }()
		opts = append(opts, driver.UploadMultipartWithProgressFunc(func(done int64) {
			if bar == nil {
				bar = output.CreateProgressBar(info.Size())
			}
			if bar != nil {
				bar.SetCurrent(done)
			}
		}))
	}

	if err := u.client.RapidUploadOrByMultipartResumable(dirID, fileName, info.Size(), f, ckpt, u.store.Save, opts...); err != nil {
		return err
	}

	// 成功后清理 checkpoint（目录变更时记录在旧路径下）
	if ckptPath == "" {
		ckptPath = absPath
	}
	return u.store.Remove(ckptPath)
}

func (u *uploader) record(localPath, remoteDir string, size int64, err error) {
	o := uploadOutcome{LocalPath: localPath, RemoteDir: remoteDir, Size: size}
	if err != nil {
		o.Error = err.Error()
	}
	u.outcomes = append(u.outcomes, o)
	if u.jsonMode {
		return
	}
	if err != nil {
		fmt.Printf("Upload failed: %s: %v\n", localPath, err)
		return
	}
	fmt.Printf("Upload complete: %s -> %s\n", filepath.Base(localPath), remoteDir)
}

type dirLister interface {
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
}

// conflictsInRemote returns the wanted names that already exist as
// directories under dirID.
func conflictsInRemote(l dirLister, dirID string, names []string) ([]string, error) {
	children, err := l.List(dirID)
	if err != nil {
		return nil, err
	}
	return findDirConflicts(*children, names), nil
}

func findDirConflicts(files []driver.File, names []string) []string {
	var conflicts []string
	for _, name := range names {
		for _, f := range files {
			if f.IsDirectory && f.Name == name {
				conflicts = append(conflicts, name)
				break
			}
		}
	}
	return conflicts
}

func init() {
	rootCmd.AddCommand(uploadCmd)
}
