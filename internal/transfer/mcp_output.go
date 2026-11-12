package transfer

import (
	"errors"
	"os"
	"strings"
	"unicode/utf8"
)

// MCPOutputBinding describes one caller-owned, exclusively created staging
// file in an already pinned directory. OwnerID identifies the caller's durable
// binding; this value does not prove root-registry permission or ownership by
// itself. The caller must check that authority and ACK this binding before the
// first body byte. CreateStaging supplies the initial exclusive writable fd.
//
// There is no checksum, verification phase, Task state or persistence format
// here. The caller must exclude namespace mutators in its managed staging area,
// as required by CompletionDirectory, and retain the original binding on error.
type MCPOutputBinding struct {
	OwnerID           string
	DirectoryIdentity CompletionFileIdentity
	StagingIdentity   CompletionFileIdentity
	StagingName       string
	FinalName         string
}

// MCPOutputSeal is supplied only after this exact run's writers have joined.
// A readonly fd or a size alone cannot prove writer quiescence. No digest match
// or remote resource identity is inferred from this filesystem evidence.
type MCPOutputSeal struct {
	Binding    MCPOutputBinding
	SealedSize int64
}

// MCPOutputPublishOutcome concerns only this filesystem call. It is neither a
// persisted phase nor a fresh permit, and cannot authorize Task done/effects.
type MCPOutputPublishOutcome uint8

const (
	// No link was attempted by this call; this does not assert final is absent.
	MCPOutputNotAttempted MCPOutputPublishOutcome = iota
	// An existing final is never adopted, including the same staging inode.
	MCPOutputConflict
	// A link was attempted; final may exist. Keep artifacts and reconcile via
	// the caller's existing authority. Do not assume rollback or overwrite.
	MCPOutputUncertain
	// Identity/size, no-replace link and directory Sync succeeded. This is not
	// a checksum result, durable Task commit or permission to send effects.
	MCPOutputConfirmed
)

func (d *CompletionDirectory) checkMCPOutputBinding(binding MCPOutputBinding) error {
	if binding.OwnerID == "" || len(binding.OwnerID) > 128 || !utf8.ValidString(binding.OwnerID) ||
		strings.IndexByte(binding.OwnerID, 0) >= 0 || !completionName(binding.StagingName) ||
		!completionName(binding.FinalName) || strings.EqualFold(binding.StagingName, binding.FinalName) ||
		binding.DirectoryIdentity.Inode == 0 || binding.StagingIdentity.Inode == 0 {
		return completionError(CompletionInvalid, nil)
	}
	if d == nil || d.handle == nil || d.identity != binding.DirectoryIdentity || binding.StagingIdentity.Device != d.identity.Device {
		return completionError(CompletionIdentity, nil)
	}
	actual, err := CompletionIdentityOf(d.handle)
	if err != nil || actual != d.identity {
		return completionError(CompletionIdentity, err)
	}
	return nil
}

func mcpOutputFile(file *os.File, binding MCPOutputBinding) (os.FileInfo, error) {
	if file == nil {
		return nil, completionError(CompletionInvalid, nil)
	}
	if err := completionReadOnly(file); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, completionError(CompletionIdentity, err)
	}
	identity, err := CompletionIdentityOf(file)
	if err != nil || identity != binding.StagingIdentity {
		return nil, completionError(CompletionIdentity, err)
	}
	return info, nil
}

// OpenMCPOutput returns an owned O_RDONLY fd to the same anchored staging inode.
// It does not seal the file or authorize a resumed writer. After joining writers
// the caller can use this fd's Stat to freeze SealedSize before publication.
func (d *CompletionDirectory) OpenMCPOutput(binding MCPOutputBinding) (*os.File, error) {
	if err := d.checkMCPOutputBinding(binding); err != nil {
		return nil, err
	}
	file, err := d.openFile(binding.StagingName)
	if err != nil {
		return nil, err
	}
	if _, err = mcpOutputFile(file, binding); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (d *CompletionDirectory) checkMCPOutputSeal(file *os.File, seal MCPOutputSeal) (os.FileInfo, error) {
	if seal.SealedSize < 0 {
		return nil, completionError(CompletionInvalid, nil)
	}
	if err := d.checkMCPOutputBinding(seal.Binding); err != nil {
		return nil, err
	}
	info, err := mcpOutputFile(file, seal.Binding)
	if err != nil {
		return nil, err
	}
	if info.Size() != seal.SealedSize {
		return nil, completionError(CompletionIdentity, nil)
	}
	anchor, err := d.OpenMCPOutput(seal.Binding)
	if err != nil {
		return nil, err
	}
	defer anchor.Close()
	other, err := anchor.Stat()
	if err != nil || !os.SameFile(info, other) || other.Size() != seal.SealedSize || !info.ModTime().Equal(other.ModTime()) {
		return nil, completionError(CompletionIdentity, err)
	}
	return info, nil
}

// PublishMCPOutput borrows the sealed readonly fd; the caller closes it. The
// same lifecycle Fence must cover this call and the caller's later commit.
// File Sync precedes the anchored no-replace link; directory Sync follows.
// Nothing is removed on error, and an existing final is always a conflict.
// Even Confirmed cannot authorize a Task commit, notification or fresh effect.
func (d *CompletionDirectory) PublishMCPOutput(file *os.File, seal MCPOutputSeal) (MCPOutputPublishOutcome, error) {
	return d.publishMCPOutput(file, seal, func() error { return file.Sync() }, func() error { return d.handle.Sync() })
}

// Per-call Sync functions permit deterministic failure tests without a global
// hook. They do not change filesystem ownership or supply execution authority.
func (d *CompletionDirectory) publishMCPOutput(file *os.File, seal MCPOutputSeal, syncFile, syncDirectory func() error) (MCPOutputPublishOutcome, error) {
	before, err := d.checkMCPOutputSeal(file, seal)
	if err != nil {
		return MCPOutputNotAttempted, err
	}
	if err = syncFile(); err != nil {
		return MCPOutputNotAttempted, completionError(CompletionIO, err)
	}
	if err = d.link(seal.Binding.StagingName, seal.Binding.FinalName); err != nil {
		if errors.Is(err, os.ErrExist) {
			return MCPOutputConflict, err
		}
		return MCPOutputUncertain, err
	}
	after, err := d.checkMCPOutputSeal(file, seal)
	if err != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		return MCPOutputUncertain, completionError(CompletionIdentity, err)
	}
	final, err := d.openFile(seal.Binding.FinalName)
	if err != nil {
		return MCPOutputUncertain, err
	}
	finalInfo, err := final.Stat()
	_ = final.Close()
	if err != nil || !os.SameFile(after, finalInfo) || finalInfo.Size() != seal.SealedSize || !after.ModTime().Equal(finalInfo.ModTime()) {
		return MCPOutputUncertain, completionError(CompletionIdentity, err)
	}
	if err = syncDirectory(); err != nil {
		return MCPOutputUncertain, completionError(CompletionIO, err)
	}
	return MCPOutputConfirmed, nil
}
