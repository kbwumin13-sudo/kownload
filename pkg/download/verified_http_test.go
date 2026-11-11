package download

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestVerifiedHTTPInactiveCoreNeverFallsBack(t *testing.T) {
	d, store, _ := scopedOperationFixture(t, false)
	snapshot := func() string {
		mem := store.(*MemStorage)
		mem.lock.RLock()
		defer mem.lock.RUnlock()
		raw, err := json.Marshal(mem.data)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	before := snapshot()
	cap, err := d.VerifiedHTTPCapabilityForDataset(scopedDatasetA)
	if err != nil || cap.Version != 1 || cap.Supported || cap.Capability != verifiedHTTPCapability {
		t.Fatal("claimed inactive writer", cap, err)
	}
	for _, action := range []func() (VerifiedHTTPOperationResult, error){
		func() (VerifiedHTTPOperationResult, error) {
			return d.CreateVerifiedHTTPOperationForDataset(context.Background(), scopedDatasetA, VerifiedHTTPRequest{})
		},
		func() (VerifiedHTTPOperationResult, error) {
			return d.QueryVerifiedHTTPOperationForDataset(scopedDatasetA, "same-op")
		},
		func() (VerifiedHTTPOperationResult, error) {
			return d.CancelVerifiedHTTPOperationForDataset(scopedDatasetA, "same-op")
		},
	} {
		got, err := action()
		if !errors.Is(err, ErrVerifiedHTTPUnsupported) || got.Operation.State != "" || len(got.CompletionViews) != 0 {
			t.Fatal("inactive action returned a receipt", got, err)
		}
	}
	if before != snapshot() {
		t.Fatal("unsupported core wrote state")
	}
	if _, err = d.VerifiedHTTPCapabilityForDataset(scopedDatasetB); !errors.Is(err, ErrDatasetMismatch) {
		t.Fatal("wrong dataset accepted", err)
	}
}
