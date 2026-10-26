package netdisk

import (
	"context"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
)

func (p *guangyaProvider) task(ctx context.Context, c *APIClient, s *Session, src model.Source, id string) error {
	if id == "" {
		return Fail("invalid_transfer", "光鸭没有返回转存任务 ID")
	}
	for i := 0; i < 120; i++ {
		r, e := p.call(ctx, c, s, src, "/nd.bizuserres.s/v1/get_task_status", object{"taskId": id})
		if e != nil {
			return e
		}
		d := obj(r["data"])
		switch str(d["status"]) {
		case "2":
			if code := str(at(d, "detail", "code")); code != "" && code != "0" {
				return responseError(code)
			}
			return nil
		case "0", "1":
		case "3", "-1":
			return Fail("transfer_failed", "光鸭异步任务失败，专用目录已保留")
		default:
			return Fail("invalid_response", "光鸭返回了未知异步任务状态")
		}
		if e = pollPause(ctx); e != nil {
			return e
		}
	}
	return Fail("transfer_timeout", "光鸭异步任务超时，专用目录已保留")
}
func (p *guangyaProvider) Transfer(ctx context.Context, c *APIClient, s *Session, src model.Source, files []model.File, batch string) (result *model.TransferReceipt, err error) {
	if e := transferInput(s, src, files, batch); e != nil {
		return nil, e
	}
	if bearer(s) == "" {
		return nil, LoginRequired(p.Info().Name)
	}
	token, e := p.shareToken(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	r, e := p.call(ctx, c, s, src, "/nd.bizuserres.s/v1/file/create_dir", object{"parentId": "", "dirName": folderName(batch), "failIfNameExist": true})
	if e != nil {
		return nil, e
	}
	folder := str(at(r, "data", "fileId"))
	if folder == "" {
		return nil, Fail("invalid_transfer", "光鸭没有返回新建目录 ID")
	}
	result = receipt(p, s, src, batch, folder, []model.File{})
	for _, f := range files {
		before, e := listOwnedFolder(ctx, p, c, s, src, folder)
		if e != nil {
			return result, e
		}
		if e = knownFolderContents(before, result); e != nil {
			return result, e
		}
		r, e = p.call(ctx, c, s, src, "/userres/v1/restore_share", object{"accessToken": token, "fileIds": []string{f.ID}, "parentId": folder})
		if e != nil {
			return result, e
		}
		task := str(at(r, "data", "taskId"))
		if e = p.task(ctx, c, s, src, task); e != nil {
			return result, e
		}
		var copied model.File
		for i := 0; i < 40; i++ {
			after, e := listOwnedFolder(ctx, p, c, s, src, folder)
			if e != nil {
				return result, e
			}
			copied, e = copyDelta(before, after, f)
			if e == nil {
				break
			}
			if typed, ok := e.(*Error); !ok || typed.Code != "copy_pending" {
				return result, e
			}
			if i == 39 {
				return result, Fail("invalid_transfer", "光鸭任务完成但未返回唯一的新副本身份")
			}
			if e = pollPause(ctx); e != nil {
				return result, e
			}
		}
		if e = recordCreated(result, f, copied.ID); e != nil {
			return result, e
		}
		nf := &result.Files[len(result.Files)-1]
		nf.Size = copied.Size
		nf.Meta["own_name"] = copied.Name
		nf.Meta["task_id"] = task
		if nf.Hash == "" {
			nf.Hash = copied.Hash
		}
	}
	return result, nil
}
func (p *guangyaProvider) Cleanup(ctx context.Context, c *APIClient, s *Session, r *model.TransferReceipt) error {
	if e := ownedCleanup(ctx, p, c, s, r, ""); e != nil {
		return e
	}
	for _, ids := range [][]string{r.CreatedIDs, {r.FolderID}} {
		if len(ids) == 0 {
			continue
		}
		v, e := p.call(ctx, c, s, r.Source, "/nd.bizuserres.s/v1/file/delete_file", object{"fileIds": ids})
		if e != nil {
			return e
		}
		if e = p.task(ctx, c, s, r.Source, str(at(v, "data", "taskId"))); e != nil {
			return e
		}
	}
	return nil
}
