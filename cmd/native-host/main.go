// Independently implemented from the Chrome protocol and docs/NATIVE_MESSAGING.md.
package main

import (
	"context"
	"github.com/GopeedLab/gopeed/internal/appidentity"
	"github.com/GopeedLab/gopeed/internal/nativehost"
	"os"
	"path/filepath"
)

var previewBuild string
var sourceCommit = "unknown"
var releaseVersion = "unknown"

func main() {
	callerOrigin := ""
	for _, arg := range os.Args[1:] {
		if nativehost.AllowedOrigin(arg) {
			callerOrigin = arg
			break
		}
	}
	if callerOrigin == "" {
		os.Stderr.WriteString("invalid_origin\n")
		os.Exit(1)
	}
	// The installed executable lives directly in the active identity's data root.
	exe, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	client := nativehost.UnixClient{Socket: filepath.Join(filepath.Dir(exe), appidentity.HostSocket)}
	wakeup := nativehost.AppWakeup{Client: client, Scheme: appidentity.Scheme}
	if previewBuild == "true" {
		wakeup.Open = func(context.Context, string) error { return os.ErrPermission }
	}
	dispatch := nativehost.Dispatcher{Client: client, Wakeup: wakeup, Identity: &nativehost.HostIdentity{
		Socket: client.Socket, CallerOrigin: callerOrigin,
		Build: nativehost.HostBuild{SourceCommit: sourceCommit, ReleaseVersion: releaseVersion, Preview: previewBuild == "true"},
	}}
	if nativehost.Serve(os.Stdin, os.Stdout, func(body []byte) nativehost.Response { return dispatch.Dispatch(context.Background(), body) }) != nil {
		os.Stderr.WriteString("native_protocol_error\n")
		os.Exit(1)
	}
}
