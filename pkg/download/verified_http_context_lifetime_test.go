package download

import (
	"context"
	"errors"
	"testing"

	"github.com/GopeedLab/gopeed/internal/transfer"
)

func TestVerifiedHTTPLeaseSnapshotsDoNotReplaceRunCancellation(t *testing.T) {
	_, _, owner, _ := verifiedFenceFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initial, err := owner.currentLease(ctx)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := owner.currentLease(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if err = snapshot.Check(); !errors.Is(err, transfer.ErrRecoveryStale) {
		t.Fatal("Background snapshot revived canceled run", err)
	}
	if _, err = owner.verificationContext(initial.Token); !errors.Is(err, transfer.ErrRecoveryStale) {
		t.Fatal("hash context lost run cancellation", err)
	}
	if _, err = owner.currentLease(context.Background()); !errors.Is(err, transfer.ErrRecoveryStale) {
		t.Fatal("snapshot rebound canceled run", err)
	}
	if _, err = owner.begin(initial.Token.Generation+1, ""); err != nil {
		t.Fatal(err)
	}
	nextContext, nextCancel := context.WithCancel(context.Background())
	defer nextCancel()
	next, err := owner.currentLease(nextContext)
	if err != nil || next.Check() != nil {
		t.Fatal("next generation could not bind its context", err)
	}
	if initial.Check() == nil {
		t.Fatal("old context/token touched next generation")
	}
	nextCancel()
	if next.Check() == nil {
		t.Fatal("new generation lost its cancellation")
	}
}
