package netdisk

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GopeedLab/gopeed/internal/controller"
	"github.com/GopeedLab/gopeed/pkg/base"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

type folderQueueProvider struct {
	testProvider
	files      []model.File
	blockAfter int
	resolves   int
}

func (p *folderQueueProvider) List(context.Context, *APIClient, *Session, model.Source, string) (*model.Listing, error) {
	return &model.Listing{Files: p.files}, nil
}

func (p *folderQueueProvider) Resolve(ctx context.Context, _ *APIClient, _ *Session, _ model.Source, f model.File) (*Download, error) {
	p.resolves++
	if p.resolves > p.blockAfter {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return &Download{URL: "https://cdn.quark.cn/" + f.ID, File: f}, nil
}

func TestFolderQueuesBeforeResolvingEveryDownloadLink(t *testing.T) {
	for _, share := range []bool{false, true} {
		t.Run(fmt.Sprint("share=", share), func(t *testing.T) {
			p := &folderQueueProvider{blockAfter: 1}
			for i := 0; i < 906; i++ {
				p.files = append(p.files, model.File{ID: fmt.Sprint(i), Name: fmt.Sprintf("%d.bin", i), Size: 1, Meta: map[string]string{"parent_id": "folder"}})
			}
			s := NewService(p)
			cap := testCapture(t, s)
			src := cap.Source
			if !share {
				src.ShareID = ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			prepared, err := s.Prepare(ctx, controller.NewController(), &base.Request{URL: src.URL}, src, cap.SessionID, []model.File{{ID: "folder", Name: "Folder", IsDir: true}}, nil)
			if err != nil {
				t.Fatalf("metadata preparation waited for queued-file links: %v (resolved %d)", err, p.resolves)
			}
			if len(prepared.Reqs) != 906 {
				t.Fatalf("lost directory files: %d", len(prepared.Reqs))
			}
			for _, item := range prepared.Reqs {
				e := item.Req.Extra.(*model.ReqExtra)
				if e.File.Path != "Folder" || e.File.Meta["parent_id"] != "folder" {
					t.Fatal("lost directory identity/path")
				}
			}
		})
	}
}

func TestSingleLargeFilePreparationDoesNotReadFileContents(t *testing.T) {
	p := &folderQueueProvider{blockAfter: 1}
	s := NewService(p)
	cap := testCapture(t, s)
	f := model.File{ID: "large", Name: "large.bin", Size: 1 << 40}
	prepared, err := s.Prepare(context.Background(), controller.NewController(), &base.Request{URL: cap.Source.URL}, cap.Source, cap.SessionID, []model.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Reqs) != 1 || prepared.Reqs[0].Req.Extra.(*model.ReqExtra).File.Size != f.Size {
		t.Fatal("large file metadata lost")
	}
}
