package download

import (
	"context"
	"os"
	"path/filepath"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func (d *Downloader) prepareVerifiedOutput(ctx context.Context, dataset string, task *Task, lease *transfer.RecoveryLease) (*os.File, error) {
	store, ok := d.storage.(AtomicStorage)
	if !ok || lease == nil {
		return nil, transfer.ErrRecoveryHeld
	}
	store = verifiedAssociatedStore{store, dataset, task.ID}
	var output *os.File
	err := d.verifiedFence(ctx, dataset, task, lease.Token, func(owned *Task, record transfer.VerifiedCompletionRecord) error {
		if record.Phase != transfer.CompletionPrepared || owned.Meta.Res == nil || len(owned.Meta.Res.Files) != 1 || owned.Meta.Res.Name != "" {
			return transfer.ErrRecoveryHeld
		}
		target := owned.Meta.SingleFilepath()
		root, name := filepath.Dir(target), filepath.Base(target)
		if !filepath.IsAbs(root) || root != filepath.Clean(owned.Meta.Opts.Path) {
			return transfer.ErrRecoveryHeld
		}
		next := record
		next.StagingName = "." + record.ID + ".kownload-part"
		next.FinalName = name
		if err := saveTaskCompletionState(store, dataset, lease.Token, record, next); err != nil {
			return err
		}
		record = next
		// The root and basenames are already frozen/ACKed. No body write or
		// pathname-open of the final artifact is performed here.
		if err := os.MkdirAll(root, 0755); err != nil {
			return err
		}
		directory, err := transfer.OpenCompletionDirectory(root)
		if err != nil {
			return err
		}
		defer directory.Close()
		if record.DirectoryIdentity.Inode != 0 && record.DirectoryIdentity != directory.Identity() {
			return transfer.ErrRecoveryHeld
		}
		if record.DirectoryIdentity.Inode == 0 {
			next = record
			next.DirectoryIdentity = directory.Identity()
			if err = saveTaskCompletionState(store, dataset, lease.Token, record, next); err != nil {
				return err
			}
			record = next
		}
		if record.StagingIdentity.Inode != 0 {
			output, err = directory.OpenStagingWriter(record)
			return err
		}
		output, err = directory.CreateStaging(record.StagingName)
		if err != nil {
			return err
		}
		identity, err := transfer.CompletionIdentityOf(output)
		if err != nil {
			return err
		}
		next = record
		next.StagingIdentity = identity
		if err = saveTaskCompletionState(store, dataset, lease.Token, record, next); err != nil {
			return err
		}
		return nil
	})
	if err != nil && output != nil {
		_ = output.Close()
		output = nil
	}
	return output, err
}
