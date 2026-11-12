package download

import (
	"context"
	"errors"
	"net/url"

	"github.com/GopeedLab/gopeed/internal/fetcher"
	"github.com/GopeedLab/gopeed/internal/pathpolicy"
	"github.com/GopeedLab/gopeed/pkg/base"
)

// A preview owns only an ephemeral metadata object. It is never appended as a
// Task, enrolled, persisted or passed to Bind/Activate. Require closes native
// storage before AddTorrentSpec can see even a zero-length member. The object
// and its torrent reference are released before returning the value preview.
func (d *Downloader) previewNativePath(ctx context.Context, request HTTPPathCreateRequest, snapshot PathPolicySnapshot, options *base.Options) (result pathpolicy.Preparation, resultErr error) {
	if ctx == nil || options == nil || request.Request == nil {
		return result, ErrPathFreezeInvalid
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	variant, ok := pathVariantForCaps(request.RequiredCapabilities)
	if !ok || !variant.Native {
		return result, ErrPathFreezeInvalid
	}
	req, err := cloneOperationJSON(request.Request.Req)
	if err != nil {
		return result, ErrPathFreezeInvalid
	}
	opts, err := cloneOperationJSON(options)
	if err != nil {
		return result, ErrPathFreezeInvalid
	}
	manager, err := d.parseRequestFm(req)
	if err != nil || manager.Name() != variant.Protocol {
		return result, ErrPathFreezeInvalid
	}
	probe := manager.Build()
	if probe == nil {
		return result, ErrPathFreezeHeld
	}
	d.setupFetcher(manager, probe)
	defer func() { resultErr = errors.Join(resultErr, probe.Close()) }()
	required, ok := probe.(pathFreezeRequirement)
	if !ok {
		return result, ErrPathFreezeHeld
	}
	metadataOwner := NewTask()
	if err = required.RequirePathFreeze(metadataOwner.ID); err != nil {
		return result, err
	}
	if resolver, ok := probe.(fetcher.TaskResolver); ok {
		err = resolver.ResolveTask(ctx, req, opts)
	} else if resolver, ok := probe.(fetcher.ContextResolver); ok {
		err = resolver.ResolveContext(ctx, req, opts)
	} else {
		return result, ErrPathFreezeHeld
	}
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	meta, err := cloneOperationJSON(probe.Meta())
	if err != nil || meta == nil || meta.Res == nil || meta.Opts == nil || len(meta.Res.Files) == 0 {
		return result, ErrPathFreezeHeld
	}
	name := meta.Res.Name
	if name == "" {
		name = meta.Opts.Name
		if name == "" {
			name = meta.Res.Files[0].Name
		}
	}
	input := pathpolicy.Input{Path: snapshot.InputRoot, Origin: request.PathIntent.Origin, Name: name}
	if origin, err := url.Parse(req.URL); err == nil {
		input.Domain = origin.Hostname()
	}
	decision, err := pathpolicy.Evaluate(input, snapshot.Config)
	if err != nil {
		return result, err
	}
	meta.Opts.Path = decision.Root
	layout, err := pathResourceLayout(meta)
	if err != nil {
		return result, err
	}
	hash, err := pathpolicy.LayoutFingerprint(layout)
	if err != nil {
		return result, err
	}
	return pathpolicy.Preparation{Decision: decision, Name: name, Layout: layout, LayoutHash: hash}, nil
}
