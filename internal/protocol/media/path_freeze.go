package media

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/internal/transfer"
	model "github.com/GopeedLab/gopeed/pkg/protocol/media"
)

// This checkpoint is a layout description, never an execution permit. Core's
// typed Task/F record and an ACK-bound runtime guard remain authoritative.
type mediaPathLayout struct {
	OwnerID       string              `json:"ownerId"`
	Layout        pathpolicy.LayoutV1 `json:"layout"`
	WorkDirectory string              `json:"workDirectory"`
}

func cloneMediaPathLayout(v *mediaPathLayout) *mediaPathLayout {
	if v == nil {
		return nil
	}
	out := *v
	out.Layout.Members = append([]pathpolicy.Member(nil), v.Layout.Members...)
	return &out
}

// RequirePathFreeze is called before first Resolve. Restored strict fetchers
// may reassert the same owner; a legacy resolved task cannot be silently opted in.
func (f *Fetcher) RequirePathFreeze(ownerID string) error {
	if ownerID == "" || len(ownerID) > 256 || strings.ContainsAny(ownerID, "\x00\r\n") {
		return transfer.ErrRecoveryHeld
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.starting || f.resolveCancel != nil {
		return transfer.ErrRecoveryHeld
	}
	if f.pathLayout != nil && f.pathLayout.OwnerID != ownerID {
		return transfer.ErrRecoveryHeld
	}
	if f.pathOwner != "" && f.pathOwner != ownerID {
		return transfer.ErrRecoveryHeld
	}
	if f.pathOwner == "" && f.pathLayout == nil && f.meta != nil && f.meta.Res != nil {
		return transfer.ErrRecoveryHeld
	}
	f.pathOwner = ownerID
	f.pathExecution = nil
	return nil
}

func (f *Fetcher) MediaOutputLayout() (pathpolicy.LayoutV1, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pathOwner == "" || f.pathLayout == nil || f.pathLayout.OwnerID != f.pathOwner {
		return pathpolicy.LayoutV1{}, "", transfer.ErrRecoveryHeld
	}
	owned := cloneMediaPathLayout(f.pathLayout)
	return owned.Layout, owned.WorkDirectory, nil
}

func resolvedMediaLayout(ownerID, target string, extra *model.ReqExtra, info *videoInfo) (*mediaPathLayout, error) {
	if ownerID == "" || !filepath.IsAbs(target) || filepath.Clean(target) != target || extra == nil || info == nil {
		return nil, transfer.ErrRecoveryHeld
	}
	main := filepath.Base(target)
	members := []pathpolicy.Member{{Index: 0, RelativePath: main, Selected: true}}
	stem := strings.TrimSuffix(main, filepath.Ext(main))
	seen := map[string]bool{}
	for _, language := range extra.SubtitleLanguages {
		if seen[language] {
			return nil, transfer.ErrRecoveryHeld
		}
		seen[language] = true
		formats := info.Subtitles[language]
		if len(formats) == 0 && extra.WriteAutomaticSubtitles {
			formats = info.AutomaticCaptions[language]
		}
		selected := ""
		for _, wanted := range []string{"srt", "vtt", "ass", "ssa"} {
			for _, format := range formats {
				if format.Ext == wanted {
					selected = wanted
					break
				}
			}
			if selected != "" {
				break
			}
		}
		if selected == "" {
			return nil, transfer.ErrRecoveryHeld
		}
		members = append(members, pathpolicy.Member{Index: len(members), RelativePath: stem + "." + language + "." + selected, Selected: true})
	}
	layout, err := pathpolicy.BuildLayoutV1(filepath.Dir(target), members)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(ownerID))
	return &mediaPathLayout{OwnerID: ownerID, Layout: layout, WorkDirectory: fmt.Sprintf(".kownload-media-%x", digest[:12])}, nil
}

func sameMediaPathLayout(a, b *mediaPathLayout) bool {
	if a == nil || b == nil || a.OwnerID != b.OwnerID || a.WorkDirectory != b.WorkDirectory {
		return false
	}
	left, err := pathpolicy.LayoutFingerprint(a.Layout)
	if err != nil {
		return false
	}
	right, err := pathpolicy.LayoutFingerprint(b.Layout)
	return err == nil && left == right
}

// BindPathFreezeExecution receives only Core's in-process known-ACK object.
// No persisted checkpoint, caller JSON or owner string can create one here.
func (f *Fetcher) BindPathFreezeExecution(execution *transfer.PathFreezeExecution) error {
	if execution == nil {
		return transfer.ErrRecoveryHeld
	}
	if err := execution.Check(); err != nil {
		return err
	}
	layout := execution.Layout()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running || f.starting || f.resolveCancel != nil || f.pathLayout == nil || execution.OwnerID() != f.pathOwner {
		return transfer.ErrRecoveryHeld
	}
	expected := cloneMediaPathLayout(f.pathLayout)
	candidate := &mediaPathLayout{OwnerID: execution.OwnerID(), Layout: layout, WorkDirectory: expected.WorkDirectory}
	if !sameMediaPathLayout(expected, candidate) || execution.WorkPath() != filepath.Join(layout.ParentRoot, expected.WorkDirectory) {
		return transfer.ErrRecoveryHeld
	}
	f.pathExecution = execution
	return nil
}

func publishMediaLayout(ctx context.Context, execution *transfer.PathFreezeExecution, finalPath string) error {
	if execution == nil {
		return transfer.ErrRecoveryHeld
	}
	layout := execution.Layout()
	if len(layout.Members) == 0 || finalPath != filepath.Join(execution.WorkPath(), layout.Members[0].RelativePath) {
		return transfer.ErrRecoveryHeld
	}
	// The subprocess and both pipes have joined before this function is called.
	// Validate every expected output before publishing any. Core owns identity,
	// durable transitions and noReplace; this does not claim group atomic publish.
	for _, member := range layout.Members {
		if err := mediaRecoveryCheck(ctx); err != nil {
			return err
		}
		stat, err := os.Lstat(filepath.Join(execution.WorkPath(), member.RelativePath))
		if err != nil || !stat.Mode().IsRegular() || stat.Size() == 0 {
			return transfer.ErrRecoveryHeld
		}
	}
	for _, member := range layout.Members {
		if err := mediaRecoveryCheck(ctx); err != nil {
			return err
		}
		if err := execution.Publish(member.Index); err != nil {
			return err
		}
	}
	return nil
}
