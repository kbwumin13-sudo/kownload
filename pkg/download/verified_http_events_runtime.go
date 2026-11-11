package download

import "context"

// SubscribeVerifiedHTTPDone observes only future, live accepted completions.
// Reading the dataset, connecting, or reconnecting never reconstructs a permit
// from Task.done, completion records, timestamps or a previous subscriber.
func (d *Downloader) SubscribeVerifiedHTTPDone(ctx context.Context, dataset string) (<-chan VerifiedHTTPDoneEvent, func(), error) {
	if d.closed.Load() {
		return nil, nil, ErrVerifiedHTTPEventsClosed
	}
	if err := withDatasetReadView(d.storage, dataset, func(datasetReader) error { return nil }); err != nil {
		return nil, nil, err
	}
	return d.verifiedDone.subscribe(ctx, dataset)
}
