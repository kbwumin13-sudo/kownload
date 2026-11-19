package download

import (
	"context"
	"encoding/xml"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/transfer"
	"github.com/GopeedLab/gopeed/pkg/base"
)

const (
	diskImageChecking   = "checking"
	diskImageVerified   = "verified"
	diskImageUnverified = "unverified"
	diskImageFailed     = "failed"
)

var errDiskImageIntegrity = errors.New("DMG 完整性校验失败，已保留下载文件")

// Read the checksum result from hdiutil's structured output. An exit code
// alone does not establish that the image contained a checksum.
func verifyDiskImage(ctx context.Context, file *os.File) (string, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/hdiutil", "verify", "-nocache", "-plist", "-stdinpass", "/dev/fd/3")
	cmd.ExtraFiles = []*os.File{file}
	output, err := cmd.Output()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	var response struct {
		Dict struct {
			Checksums []struct {
				Values []string `xml:"string"`
			} `xml:"dict"`
		} `xml:"dict"`
	}
	if xml.Unmarshal(output, &response) == nil && len(response.Dict.Checksums) == 2 {
		a, b := response.Dict.Checksums[0].Values, response.Dict.Checksums[1].Values
		if err == nil && len(a) == 2 && a[0] != "" && a[1] != "" && slices.Equal(a, b) {
			return diskImageVerified, nil
		}
		return diskImageFailed, errDiskImageIntegrity
	}
	// Legacy or raw images may have no embedded checksum. For UDIF images
	// with a trailer, an unsuccessful verifier still fails the task.
	info, statErr := file.Stat()
	if statErr == nil && info.Size() >= 512 {
		var magic [4]byte
		if _, readErr := file.ReadAt(magic[:], info.Size()-512); readErr == nil && string(magic[:]) != "koly" {
			return diskImageUnverified, nil
		}
	}
	return diskImageFailed, errDiskImageIntegrity
}

type diskImageTarget struct {
	index int
	path  string
}

func diskImageTargets(meta *fetcher.FetcherMeta) []diskImageTarget {
	if meta == nil || meta.Opts == nil || meta.Res == nil {
		return nil
	}
	var targets []diskImageTarget
	for i, file := range meta.Res.Files {
		if file == nil || len(meta.Opts.SelectFiles) > 0 && !slices.Contains(meta.Opts.SelectFiles, i) {
			continue
		}
		path := filepath.Join(meta.RootDirPath(), file.Path, file.Name)
		if len(meta.Res.Files) == 1 && meta.Res.Name == "" {
			path = meta.SingleFilepath()
		}
		if strings.EqualFold(filepath.Ext(path), ".dmg") {
			targets = append(targets, diskImageTarget{index: i, path: path})
		}
	}
	return targets
}

func (d *Downloader) openDiskImage(task *Task, pathState *TaskPathFreeze, target diskImageTarget) (*os.File, bool, error) {
	if pathState != nil {
		value, ok := d.pathOutputs.Load(task.ID)
		if !ok {
			return nil, false, ErrPathFreezeHeld
		}
		output := value.(*pathOutputRuntime)
		if err := output.check(); err != nil {
			return nil, false, err
		}
		if pathState.Output != nil && pathState.Output.Binding != nil {
			file, err := transfer.OpenVerifiedMCPOutput(output.lease, *pathState.Output.Binding)
			return file, true, err
		}
		if output.storage == nil {
			return nil, false, ErrPathFreezeHeld
		}
		file, err := output.storage.NativeFile(target.index)
		return file, false, err
	}
	info, err := os.Lstat(target.path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false, errDiskImageIntegrity
	}
	file, err := os.Open(target.path)
	if err != nil {
		return nil, false, errDiskImageIntegrity
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, false, errDiskImageIntegrity
	}
	return file, true, nil
}

func (d *Downloader) verifyDownloadedDiskImages(ctx context.Context, task *Task, f fetcher.Fetcher, generation uint64) error {
	if runtime.GOOS != "darwin" || f == nil {
		return nil
	}
	task.dataLock.RLock()
	// MCP-controlled tasks restrict subprocess effects. Explicit SHA-256
	// tasks verify their own staged descriptor inside their completion path.
	if task.MCPWrite != nil || task.Completion != nil {
		task.dataLock.RUnlock()
		return nil
	}
	pathState := task.PathFreeze
	meta := task.Meta
	if task.Protocol == "netdisk" && pathState == nil {
		meta = f.Meta()
	}
	targets := diskImageTargets(meta)
	task.dataLock.RUnlock()
	if len(targets) == 0 {
		return nil
	}
	if !setDiskImageStatus(task, generation, diskImageChecking) {
		return context.Canceled
	}
	aggregate := diskImageVerified
	for _, target := range targets {
		file, closeFile, err := d.openDiskImage(task, pathState, target)
		if err != nil {
			setDiskImageStatus(task, generation, diskImageFailed)
			return errDiskImageIntegrity
		}
		result, err := verifyDiskImage(ctx, file)
		if closeFile {
			_ = file.Close()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			setDiskImageStatus(task, generation, diskImageFailed)
			return err
		}
		if result == diskImageUnverified {
			aggregate = result
		}
	}
	if !setDiskImageStatus(task, generation, aggregate) {
		return context.Canceled
	}
	return nil
}

func setDiskImageStatus(task *Task, generation uint64, status string) bool {
	task.statusLock.Lock()
	defer task.statusLock.Unlock()
	task.dataLock.Lock()
	defer task.dataLock.Unlock()
	if task.runGeneration != generation || task.Status != base.DownloadStatusRunning || task.Progress == nil {
		return false
	}
	task.Progress.DiskImageStatus = status
	return true
}
