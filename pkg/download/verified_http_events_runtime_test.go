package download

import (
	"context"
	"errors"
	"testing"
)

func TestVerifiedHTTPSubscriptionRequiresActiveDataset(t *testing.T) {
	d, store, _ := scopedOperationFixture(t, false)
	if _, _, err := d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetB); !errors.Is(err, ErrDatasetMismatch) {
		t.Fatal("wrong dataset subscribed", err)
	}
	events, release, err := d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetA)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// There is deliberately no runtime producer in this adapter slice.
	select {
	case event := <-events:
		t.Fatal("subscription replayed history", event)
	default:
	}
	var before datasetRecord
	if _, err := store.Get(bucketConfig, datasetIdentityKey, &before); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("Close retained an event")
		}
	default:
		t.Fatal("Close did not close subscriber")
	}
	if _, _, err := d.SubscribeVerifiedHTTPDone(context.Background(), scopedDatasetA); !errors.Is(err, ErrVerifiedHTTPEventsClosed) {
		t.Fatal("closed instance accepted subscriber", err)
	}
	release()
}
