package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"debug/macho"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	installXattr     = "io.github.kbwumin13-sudo.kownload.mcp-install"
	quarantineXattr  = "com.apple.quarantine"
	helperName       = "kownload-mcp"
	helperRelative   = "Contents/Helpers/kownload-mcp"
	maxHelperBytes   = 128 << 20
	maxManifestBytes = 64 << 10
	maxInstallRecord = 2048
)

var (
	errInstall           = errors.New("helper installation rejected")
	errInstallDurability = errors.New("helper published; directory sync failed")
	sourcePattern        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	releasePattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,95}$`)
)

type helperManifest struct {
	ManifestVersion int    `json:"manifestVersion"`
	HelperPath      string `json:"helperPath"`
	Version         string `json:"version"`
	SourceCommit    string `json:"sourceCommit"`
	ReadAPIVersion  int    `json:"readApiVersion"`
	SDKVersion      string `json:"sdkVersion"`
	Architecture    string `json:"architecture"`
	SHA256          string `json:"sha256"`
}
type installRecord struct {
	RecordVersion  int    `json:"recordVersion"`
	Product        string `json:"product"`
	SourceCommit   string `json:"sourceCommit"`
	Version        string `json:"version"`
	ReadAPIVersion int    `json:"readApiVersion"`
	Architecture   string `json:"architecture"`
	SHA256         string `json:"sha256"`
}

func (m helperManifest) record() installRecord {
	return installRecord{1, helperName, m.SourceCommit, m.Version, m.ReadAPIVersion, m.Architecture, m.SHA256}
}
func validRecord(r installRecord) bool {
	suffix := "+" + r.SourceCommit
	return r.RecordVersion == 1 && r.Product == helperName && sourcePattern.MatchString(r.SourceCommit) &&
		strings.HasSuffix(r.Version, suffix) && releasePattern.MatchString(strings.TrimSuffix(r.Version, suffix)) &&
		r.ReadAPIVersion == 1 && r.Architecture == "arm64" && digestPattern.MatchString(r.SHA256)
}
func decodeStrict(data []byte, dst any) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return errInstall
	}
	allowed := ""
	switch dst.(type) {
	case *helperManifest:
		allowed = "manifestVersion helperPath version sourceCommit readApiVersion sdkVersion architecture sha256"
	case *installRecord:
		allowed = "recordVersion product sourceCommit version readApiVersion architecture sha256"
	default:
		return errInstall
	}
	for key := range fields {
		if !strings.Contains(" "+allowed+" ", " "+key+" ") {
			return errInstall
		}
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if rejectDuplicateKeys(data) != nil || d.Decode(dst) != nil || d.Decode(new(any)) != io.EOF {
		return errInstall
	}
	return nil
}

// Repeated keys are ambiguous for signed metadata/ownership records even when
// the standard JSON decoder would silently accept the last value.
func rejectDuplicateKeys(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 16 {
			return errInstall
		}
		token, err := d.Token()
		if err != nil {
			return errInstall
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return errInstall
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errInstall
				}
				seen[name] = true
				if value(depth+1) != nil {
					return errInstall
				}
			}
		case '[':
			for d.More() {
				if value(depth+1) != nil {
					return errInstall
				}
			}
		default:
			return errInstall
		}
		_, err = d.Token()
		return err
	}
	if value(0) != nil {
		return errInstall
	}
	if _, err := d.Token(); err != io.EOF {
		return errInstall
	}
	return nil
}
func parseManifest(data []byte) (helperManifest, error) {
	// Builder metadata can grow independently; the MCP sub-contract is closed.
	var top struct {
		SourceCommit   string          `json:"sourceCommit"`
		ReleaseVersion string          `json:"releaseVersion"`
		BuildNumber    int             `json:"buildNumber"`
		Architecture   string          `json:"architecture"`
		Preview        *bool           `json:"preview"`
		MCP            json.RawMessage `json:"mcp"`
	}
	var m helperManifest
	if len(data) > maxManifestBytes || rejectDuplicateKeys(data) != nil || json.Unmarshal(data, &top) != nil || decodeStrict(top.MCP, &m) != nil ||
		top.Preview == nil || top.BuildNumber < 1 || !releasePattern.MatchString(top.ReleaseVersion) ||
		!validRecord(m.record()) || m.ManifestVersion != 1 || m.ReadAPIVersion != 1 || m.HelperPath != helperRelative || m.SDKVersion != "v1.7.0" ||
		m.SourceCommit != top.SourceCommit || m.Architecture != top.Architecture || m.Version != top.ReleaseVersion+"+"+top.SourceCommit {
		return m, errInstall
	}
	return m, nil
}
func managedRoot(home string) string {
	return filepath.Join(home, "Library", "Application Support", "io.github.kbwumin13-sudo.kownload")
}

func runInstall(args []string) error {
	flags := flag.NewFlagSet("install-helper", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	bundle := flags.String("bundle", "", "explicitly trusted App bundle")
	root := flags.String("data-root", "", "existing default managed Kownload root")
	if flags.Parse(args) != nil || flags.NArg() != 0 || *bundle == "" || *root == "" {
		return errInstall
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return errInstall
	}
	m, err := installHelper(*bundle, *root, home, defaultInstallOps())
	if err != nil {
		return err
	}
	fmt.Println("kownload-mcp installed " + m.Version + " read-api=1")
	return nil
}

// Operations are explicit per transaction so tests can inject pre-publication
// failures without global mutable hooks or a second deployment implementation.
type installOps struct {
	verify    func(string, bool) error
	version   func(string, string) error
	setRecord func(int, []byte) error
	syncFile  func(*os.File) error
	publish   func(int, string, bool) error
}

func defaultInstallOps() installOps {
	return installOps{
		verify: func(path string, bundle bool) error {
			if !bundle {
				binary, err := macho.Open(path)
				if err != nil {
					return errInstall
				}
				valid := binary.Cpu == macho.CpuArm64 && binary.Type == macho.TypeExec
				binary.Close()
				if !valid {
					return errInstall
				}
			}
			args := []string{"--verify", "--strict"}
			if bundle {
				args = append(args, "--deep")
			}
			args = append(args, path)
			_, err := installCommand("/usr/bin/codesign", args...)
			return err
		},
		version: func(path, want string) error {
			out, err := installCommand(path, "--version")
			if err != nil || string(out) != "kownload-mcp "+want+" read-api=1\n" {
				return errInstall
			}
			return nil
		},
		setRecord: func(fd int, b []byte) error { return unix.Fsetxattr(fd, installXattr, b, unix.XATTR_CREATE) },
		syncFile:  func(f *os.File) error { return f.Sync() },
		publish: func(fd int, name string, existing bool) error {
			if !existing {
				return unix.RenameatxNp(fd, name, fd, helperName, unix.RENAME_EXCL)
			}
			return unix.Renameat(fd, name, fd, helperName)
		},
	}
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4096 {
		return 0, errInstall
	}
	return b.Buffer.Write(p)
}
func installCommand(path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	// No inherited DYLD, proxy, token or client configuration environment.
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, errInstall
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "HOME=" + home}
	var out limitedOutput
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return nil, errInstall
	}
	return out.Bytes(), nil
}
func canonical(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}
func sameFile(a, b *unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }
func trustedDir(s *unix.Stat_t) bool {
	return s.Mode&unix.S_IFMT == unix.S_IFDIR && (s.Uid == 0 || int(s.Uid) == os.Getuid()) && (s.Mode&0022 == 0 || s.Mode&unix.S_ISVTX != 0)
}

// Walk every component through directory descriptors. Never follow links or
// repair permissions; a sticky shared /private/tmp ancestor is allowed.
func openInstallDir(path string) (int, error) {
	if !canonical(path) {
		return -1, errInstall
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, errInstall
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if e != nil {
			return -1, errInstall
		}
		fd = next
		var s unix.Stat_t
		if unix.Fstat(fd, &s) != nil || !trustedDir(&s) {
			unix.Close(fd)
			return -1, errInstall
		}
	}
	return fd, nil
}
func openInstallFile(dir int, name string, private bool, max int64) (*os.File, unix.Stat_t, error) {
	var s unix.Stat_t
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, s, err
	}
	if unix.Fstat(fd, &s) != nil || !validInstallFile(&s, private, max) {
		unix.Close(fd)
		return nil, s, errInstall
	}

	return os.NewFile(uintptr(fd), name), s, nil
}
func validInstallFile(s *unix.Stat_t, private bool, max int64) bool {
	return s.Mode&unix.S_IFMT == unix.S_IFREG && s.Nlink == 1 && s.Size > 0 && s.Size <= max &&
		(int(s.Uid) == os.Getuid() || (!private && s.Uid == 0)) && s.Mode&0022 == 0 && s.Mode&07000 == 0 && (!private || s.Mode&0777 == 0700)
}
func hashHelper(f *os.File) (string, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", errInstall
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxHelperBytes+1))
	if err != nil || n < 1 || n > maxHelperBytes {
		return "", errInstall
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func readAttr(fd int, key string, max int) ([]byte, error) {
	b := make([]byte, max+1)
	n, err := unix.Fgetxattr(fd, key, b)
	if err != nil || n > max {
		return nil, errInstall
	}
	return b[:n], nil
}
func optionalQuarantine(fd int) ([]byte, error) {
	b := make([]byte, 4097)
	n, err := unix.Fgetxattr(fd, quarantineXattr, b)
	if errors.Is(err, unix.ENOATTR) {
		return nil, nil
	}
	if err != nil || n > 4096 {
		return nil, errInstall
	}
	return b[:n], nil
}
func existingInstall(dir int) (*unix.Stat_t, []byte, error) {
	f, s, err := openInstallFile(dir, helperName, true, maxHelperBytes)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, errInstall
	}
	defer f.Close()
	b, err := readAttr(int(f.Fd()), installXattr, maxInstallRecord)
	if err != nil {
		return nil, nil, errInstall
	}
	var r installRecord
	if decodeStrict(b, &r) != nil || !validRecord(r) {
		return nil, nil, errInstall
	}
	hash, err := hashHelper(f)
	if err != nil || hash != r.SHA256 {
		return nil, nil, errInstall
	}
	q, err := optionalQuarantine(int(f.Fd()))
	return &s, q, err
}

func installHelper(bundle, root, home string, ops installOps) (helperManifest, error) {
	var empty helperManifest
	if !canonical(home) || !canonical(bundle) || root != managedRoot(home) || os.Getuid() == 0 {
		return empty, errInstall
	}
	rootFD, err := openInstallDir(root)
	if err != nil {
		return empty, errInstall
	}
	defer unix.Close(rootFD)
	var rootStat unix.Stat_t
	if unix.Fstat(rootFD, &rootStat) != nil || int(rootStat.Uid) != os.Getuid() || rootStat.Mode&0022 != 0 {
		return empty, errInstall
	}
	bundleFD, err := openInstallDir(bundle)
	if err != nil {
		return empty, errInstall
	}
	defer unix.Close(bundleFD)
	helperDir, err := openInstallDir(filepath.Join(bundle, "Contents", "Helpers"))
	if err != nil {
		return empty, errInstall
	}
	defer unix.Close(helperDir)
	resources, err := openInstallDir(filepath.Join(bundle, "Contents", "Resources"))
	if err != nil {
		return empty, errInstall
	}
	defer unix.Close(resources)
	manifest, _, err := openInstallFile(resources, "build-manifest.json", false, maxManifestBytes)
	if err != nil {
		return empty, errInstall
	}
	data, err := io.ReadAll(io.LimitReader(manifest, maxManifestBytes+1))
	manifest.Close()
	if err != nil {
		return empty, errInstall
	}
	m, err := parseManifest(data)
	if err != nil {
		return empty, errInstall
	}
	source, sourceStat, err := openInstallFile(helperDir, helperName, false, maxHelperBytes)
	if err != nil {
		return empty, errInstall
	}
	defer source.Close()
	helperPath := filepath.Join(bundle, helperRelative)
	if sourceStat.Mode&0111 == 0 || ops.verify(bundle, true) != nil || ops.verify(helperPath, false) != nil {
		return empty, errInstall
	}
	hash, err := hashHelper(source)
	if err != nil || hash != m.SHA256 {
		return empty, errInstall
	}
	// Preserve quarantine instead of silently stripping system trust metadata.
	q, err := optionalQuarantine(int(source.Fd()))
	if err != nil {
		return empty, errInstall
	}
	bundleQ, err := optionalQuarantine(bundleFD)
	if err != nil {
		return empty, errInstall
	}
	if len(q) == 0 {
		q = bundleQ
	} else if len(bundleQ) != 0 && !bytes.Equal(q, bundleQ) {
		return empty, errInstall
	}
	if err = unix.Mkdirat(rootFD, "bin", 0700); err != nil && !errors.Is(err, unix.EEXIST) {
		return empty, errInstall
	} else if err == nil && unix.Fsync(rootFD) != nil {
		return empty, errInstall
	}
	bin, err := unix.Openat(rootFD, "bin", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return empty, errInstall
	}
	defer unix.Close(bin)
	var binStat unix.Stat_t
	if unix.Fstat(bin, &binStat) != nil || int(binStat.Uid) != os.Getuid() || binStat.Mode&07777 != 0700 || unix.Flock(bin, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return empty, errInstall
	}
	defer unix.Flock(bin, unix.LOCK_UN)
	old, oldQ, err := existingInstall(bin)
	if err != nil {
		return empty, errInstall
	}
	if len(oldQ) != 0 {
		if len(q) == 0 {
			q = oldQ
		} else if !bytes.Equal(q, oldQ) {
			return empty, errInstall
		}
	}
	nonce := make([]byte, 16)
	if _, err = rand.Read(nonce); err != nil {
		return empty, errInstall
	}
	stageName := ".kownload-mcp-" + hex.EncodeToString(nonce)
	stageFD, err := unix.Openat(bin, stageName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0700)
	if err != nil {
		return empty, errInstall
	}
	stage := os.NewFile(uintptr(stageFD), stageName)
	defer stage.Close()
	var stageStat unix.Stat_t
	if unix.Fstat(stageFD, &stageStat) != nil {
		return empty, errInstall
	}
	// Keep a read descriptor pinned through cleanup, even after closing the
	// writable descriptor for execution; inode reuse cannot identify a replacement
	// as our temporary file.
	var stagePin *os.File
	// Clean up only this transaction's inode, never a replacement at its name.
	defer func() {
		var s unix.Stat_t
		if unix.Fstatat(bin, stageName, &s, unix.AT_SYMLINK_NOFOLLOW) == nil && sameFile(&s, &stageStat) {
			unix.Unlinkat(bin, stageName, 0)
		}
		if stagePin != nil {
			stagePin.Close()
		}
	}()
	if stage.Chmod(0700) != nil {
		return empty, errInstall
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		return empty, errInstall
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(stage, h), io.LimitReader(source, maxHelperBytes+1))
	if err != nil || n != sourceStat.Size || hex.EncodeToString(h.Sum(nil)) != m.SHA256 {
		return empty, errInstall
	}
	if len(q) > 0 && unix.Fsetxattr(stageFD, quarantineXattr, q, unix.XATTR_CREATE) != nil {
		return empty, errInstall
	}
	record, _ := json.Marshal(m.record())
	if ops.setRecord(stageFD, record) != nil {
		return empty, errInstall
	}
	got, err := readAttr(stageFD, installXattr, maxInstallRecord)
	if err != nil || !bytes.Equal(got, record) {
		return empty, errInstall
	}
	if ops.syncFile(stage) != nil {
		return empty, errInstall
	}
	var pinnedStat unix.Stat_t
	stagePin, pinnedStat, err = openInstallFile(bin, stageName, true, maxHelperBytes)
	if err != nil || !sameFile(&pinnedStat, &stageStat) {
		return empty, errInstall
	}
	// Close the writable descriptor before executing the trusted staged helper.
	if stage.Close() != nil {
		return empty, errInstall
	}
	stagePath := filepath.Join(root, "bin", stageName)
	if ops.verify(stagePath, false) != nil || ops.version(stagePath, m.Version) != nil {
		return empty, errInstall
	}
	// Reopen by pinned dir FD and verify the sealed payload after execution.
	checked, s, err := openInstallFile(bin, stageName, true, maxHelperBytes)
	if err != nil {
		return empty, errInstall
	}
	digest, hashErr := hashHelper(checked)
	sealed, attrErr := readAttr(int(checked.Fd()), installXattr, maxInstallRecord)
	checked.Close()
	if !sameFile(&s, &stageStat) || hashErr != nil || attrErr != nil || digest != m.SHA256 || !bytes.Equal(sealed, record) {
		return empty, errInstall
	}
	// Recheck both directory names and old entry immediately before publication.
	currentRoot, e := openInstallDir(root)
	if e != nil {
		return empty, errInstall
	}
	defer unix.Close(currentRoot)
	var nowRoot, nowBin unix.Stat_t
	if unix.Fstat(currentRoot, &nowRoot) != nil || !sameFile(&nowRoot, &rootStat) || unix.Fstatat(rootFD, "bin", &nowBin, unix.AT_SYMLINK_NOFOLLOW) != nil || !sameFile(&nowBin, &binStat) || nowBin.Mode&07777 != 0700 {
		return empty, errInstall
	}
	latest, latestQ, err := existingInstall(bin)
	if err != nil || (old == nil) != (latest == nil) || (old != nil && !sameFile(old, latest)) || !bytes.Equal(oldQ, latestQ) {
		return empty, errInstall
	}
	if ops.publish(bin, stageName, old != nil) != nil {
		return empty, errInstall
	}
	// Publication is atomic. A failure here means installed-but-not-durable,
	// not rollback: never overwrite a newly published entry with a stale inode.
	if unix.Fsync(bin) != nil {
		return m, errInstallDurability
	}
	return m, nil
}
