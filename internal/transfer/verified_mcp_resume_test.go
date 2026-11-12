//go:build darwin && cgo

package transfer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifiedMCPResumeWriterPreservesStage(t *testing.T) {
	path := verifiedRootFixture(t)
	root, lease := verifiedRootOpenFixture(t, path)
	defer root.Close()
	defer lease.Close()
	body := []byte("same acknowledged stage prefix")
	binding := verifiedMCPTestStage(t, lease, root.Binding().Identity, body)
	writer, err := OpenVerifiedMCPOutputWriter(lease, binding)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := writer.WriteAt([]byte("tail"), int64(len(body))); err != nil || n != 4 {
		writer.Close()
		t.Fatal("independent same-inode writer failed", err)
	}
	if err := writer.Sync(); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(path, binding.StagingName))
	if err != nil || !bytes.Equal(got, append(body, []byte("tail")...)) {
		t.Fatal("resume truncated or changed the prefix", err)
	}
	if _, err := os.Stat(filepath.Join(path, binding.FinalName)); !os.IsNotExist(err) {
		t.Fatal("resume created a final", err)
	}
}

func TestVerifiedMCPResumeWriterRejectsForeignAndFinal(t *testing.T) {
	for _, mode := range []string{"foreign-stage", "foreign-final", "same-inode-final"} {
		t.Run(mode, func(t *testing.T) {
			path := verifiedRootFixture(t)
			root, lease := verifiedRootOpenFixture(t, path)
			defer root.Close()
			defer lease.Close()
			body := []byte("preserve all existing bytes")
			binding := verifiedMCPTestStage(t, lease, root.Binding().Identity, body)
			stage, final := filepath.Join(path, binding.StagingName), filepath.Join(path, binding.FinalName)
			switch mode {
			case "foreign-stage":
				if err := os.Rename(stage, stage+".original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(stage, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-final":
				if err := os.WriteFile(final, body, 0600); err != nil {
					t.Fatal(err)
				}
			case "same-inode-final":
				if err := os.Link(stage, final); err != nil {
					t.Fatal(err)
				}
			}
			writer, err := OpenVerifiedMCPOutputWriter(lease, binding)
			if writer != nil {
				writer.Close()
				t.Fatal("returned a writer for rejected identity")
			}
			if err == nil {
				t.Fatal("invalid resume was accepted")
			}
			got, err := os.ReadFile(stage)
			if err != nil || !bytes.Equal(got, body) {
				t.Fatal("failed resume changed stage", err)
			}
		})
	}
}
