package media

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
)

const (
	managedReleaseAPI = "https://api.github.com/repos/yt-dlp/yt-dlp/releases/latest"
	managedReleaseURL = "https://github.com/yt-dlp/yt-dlp/releases/download/"
	managedAsset      = "yt-dlp"
	managedSums       = "SHA2-256SUMS"
	managedSignature  = managedSums + ".sig"
	// The upstream release README links public.key. Its independently pinned
	// fingerprint is also checked in MDM commit 31882d2's openpgp tests.
	managedKeyFingerprint = "AC0CBBE6848D6A873464AF4E57CF65933B5A7581"
	maxManagedZip         = 12 << 20
	maxManagedFiles       = 4096
	maxManagedExpanded    = 48 << 20
)

//go:embed yt_dlp_release_key.asc
var managedReleaseKey []byte

var managedTag = regexp.MustCompile(`^[0-9]{4}\.[0-9]{2}\.[0-9]{2}$`)
var managedDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

type managedRelease struct {
	version, digest string
	asset           []byte
}

func officialYtDlp(ctx context.Context) (managedRelease, error) {
	client := &http.Client{
		Timeout: 35 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 5 || req.URL.Scheme != "https" || req.URL.User != nil {
				return ErrManagedYtDlpDownload
			}
			switch req.URL.Host {
			case "release-assets.githubusercontent.com", "objects.githubusercontent.com":
				return nil
			default:
				return ErrManagedYtDlpDownload
			}
		},
	}
	return downloadManagedRelease(ctx, client, managedReleaseAPI, managedReleaseURL, managedReleaseKey, managedKeyFingerprint)
}

func downloadManagedRelease(ctx context.Context, client *http.Client, apiURL, assetBase string, key []byte, fingerprint string) (managedRelease, error) {
	var release managedRelease
	metadata, err := getManagedBytes(ctx, client, apiURL, 256<<10)
	if err != nil {
		return release, err
	}
	var tag struct {
		Name       string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if json.Unmarshal(metadata, &tag) != nil || tag.Draft || tag.Prerelease || !validManagedVersion(tag.Name) {
		return release, ErrManagedYtDlpAsset
	}
	base := assetBase + tag.Name + "/"
	sums, err := getManagedBytes(ctx, client, base+managedSums, 64<<10)
	if err != nil {
		return release, err
	}
	sig, err := getManagedBytes(ctx, client, base+managedSignature, 16<<10)
	if err != nil {
		return release, err
	}
	want, err := signedManagedDigest(key, fingerprint, sums, sig)
	if err != nil {
		return release, err
	}
	asset, err := getManagedBytes(ctx, client, base+managedAsset, maxManagedZip)
	if err != nil {
		return release, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(asset)) != want {
		return release, ErrManagedYtDlpAsset
	}
	return managedRelease{version: tag.Name, digest: want, asset: asset}, nil
}

func validManagedVersion(version string) bool {
	if !managedTag.MatchString(version) {
		return false
	}
	_, err := time.Parse("2006.01.02", version)
	return err == nil
}

func getManagedBytes(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, ErrManagedYtDlpDownload
	}
	req.Header.Set("User-Agent", "Kownload-media-tools")
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrManagedYtDlpDownload
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > limit {
		return nil, ErrManagedYtDlpDownload
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(data)) > limit || len(data) == 0 {
		return nil, ErrManagedYtDlpDownload
	}
	return data, nil
}

func signedManagedDigest(key []byte, fingerprint string, sums, sig []byte) (string, error) {
	keys, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(key))
	if err != nil || len(keys) != 1 || fmt.Sprintf("%X", keys[0].PrimaryKey.Fingerprint) != fingerprint {
		return "", ErrManagedYtDlpSignature
	}
	if _, err := openpgp.CheckDetachedSignatureAndHash(keys, bytes.NewReader(sums), bytes.NewReader(sig),
		[]crypto.Hash{crypto.SHA256, crypto.SHA512}, nil); err != nil {
		return "", ErrManagedYtDlpSignature
	}
	var match string
	for _, line := range strings.Split(string(sums), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == managedAsset {
			if match != "" || !managedDigest.MatchString(fields[0]) {
				return "", ErrManagedYtDlpAsset
			}
			match = fields[0]
		}
	}
	if match == "" {
		return "", ErrManagedYtDlpAsset
	}
	return match, nil
}

func extractManagedModule(asset []byte, site string) error {
	archive, err := zip.NewReader(bytes.NewReader(asset), int64(len(asset)))
	if err != nil || len(archive.File) > maxManagedFiles {
		return ErrManagedYtDlpAsset
	}
	var expanded uint64
	seen := make(map[string]bool, len(archive.File))
	for _, file := range archive.File {
		name := strings.TrimSuffix(file.Name, "/")
		// Reject names with ambiguous slash or case behavior before touching disk.
		if name == "" || path.Clean(name) != name || name == ".." ||
			strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") ||
			strings.ContainsAny(name, "\\\x00\r\n:") {
			return ErrManagedYtDlpAsset
		}
		if name != "yt_dlp" && !strings.HasPrefix(name, "yt_dlp/") {
			continue
		}
		folded := strings.ToLower(name)
		if _, exists := seen[folded]; exists || file.UncompressedSize64 > maxManagedExpanded-expanded {
			return ErrManagedYtDlpAsset
		}
		expanded += file.UncompressedSize64
		mode := file.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			return ErrManagedYtDlpAsset
		}
		seen[folded] = mode.IsRegular()
		target := filepath.Join(site, filepath.FromSlash(name))
		if mode.IsDir() {
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
			continue
		}
		if name == "yt_dlp" {
			return ErrManagedYtDlpAsset
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		if err := extractManagedFile(file, target); err != nil {
			return err
		}
	}
	if !seen["yt_dlp/__init__.py"] || !seen["yt_dlp/version.py"] {
		return ErrManagedYtDlpAsset
	}
	return nil
}

func extractManagedFile(file *zip.File, target string) error {
	src, err := file.Open()
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	n, copyErr := io.Copy(dst, io.LimitReader(src, int64(file.UncompressedSize64)+1))
	closeErr := dst.Close()
	if copyErr != nil || closeErr != nil || n != int64(file.UncompressedSize64) {
		return ErrManagedYtDlpAsset
	}
	return nil
}
