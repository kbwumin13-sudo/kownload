package netdisk

import (
	"context"
	"encoding/json"
	model "github.com/GopeedLab/gopeed/pkg/protocol/netdisk"
	"strings"
)

func (p *tcloudProvider) form(ctx context.Context, c *APIClient, s *Session, src model.Source, path string, body map[string]string) (object, error) {
	r, e := apiForm(ctx, c, "https://cloud.189.cn/api/open"+path, values(body), requestHeaders(s, src))
	if e != nil {
		return nil, e
	}
	if code := first(r, "errorCode", "errorcode"); code != "" {
		return nil, responseError(code)
	}
	if e = checkResponse(r, "res_code", "0"); e != nil {
		return nil, e
	}
	return r, nil
}
func (p *tcloudProvider) task(ctx context.Context, c *APIClient, s *Session, src model.Source, kind, id string) (object, error) {
	if id == "" {
		return nil, Fail("invalid_transfer", "天翼接口没有返回批次任务 ID")
	}
	var last object
	for i := 0; i < 120; i++ {
		r, e := p.form(ctx, c, s, src, "/batch/checkBatchTask.action", map[string]string{"type": kind, "taskId": id})
		if e != nil {
			return last, e
		}
		last = r
		switch str(r["taskStatus"]) {
		case "4":
			if num(r["failedCount"]) > 0 {
				return r, Fail("transfer_failed", "天翼任务存在失败文件，专用目录已保留")
			}
			return r, nil
		case "1", "3":
		case "2":
			return r, Fail("transfer_conflict", "天翼转存发生文件冲突，未覆盖已有文件")
		default:
			return r, Fail("invalid_response", "天翼返回了未知批次任务状态")
		}
		if e = pollPause(ctx); e != nil {
			return last, e
		}
	}
	return last, Fail("transfer_timeout", "天翼批次任务超时，专用目录已保留")
}

func listOwnedFolder(ctx context.Context, p Provider, c *APIClient, s *Session, src model.Source, folder string) ([]model.File, error) {
	src.ShareID = ""
	src.ParentID = folder
	out := []model.File{}
	cur := ""
	seen := map[string]bool{}
	for {
		if seen[cur] {
			return nil, Fail("invalid_pagination", "专用目录分页重复")
		}
		seen[cur] = true
		l, e := p.List(ctx, c, s, src, cur)
		if e != nil {
			return nil, e
		}
		out = append(out, l.Files...)
		cur = l.NextPage
		if cur == "" {
			return out, nil
		}
	}
}
func knownFolderContents(files []model.File, r *model.TransferReceipt) error {
	ids := map[string]bool{}
	for _, id := range r.CreatedIDs {
		ids[id] = true
	}
	for _, f := range files {
		if f.IsDir || !ids[f.ID] {
			return Fail("ownership_error", "专用目录出现非本批次文件，转存已停止")
		}
	}
	return nil
}

// SHARE_SAVE returns a task ID rather than copy IDs. Only query the directory
// just created by this Transfer; isolate each source file into its own task and
// bind exactly one new remote ID to a successful task and matching file content.
// Existing directories or filename searches are never used to infer ownership.
func copyDelta(before, after []model.File, source model.File) (model.File, error) {
	ids := map[string]bool{}
	for _, f := range before {
		ids[f.ID] = true
	}
	delta := []model.File{}
	for _, f := range after {
		if !ids[f.ID] {
			delta = append(delta, f)
		}
	}
	if len(delta) == 0 {
		return model.File{}, Fail("copy_pending", "转存文件尚未出现在新建目录")
	}
	if len(delta) != 1 {
		return model.File{}, Fail("ownership_error", "转存任务后新增文件身份不唯一，专用目录已保留")
	}
	f := delta[0]
	if f.IsDir || f.ID == source.ID || f.Name != source.Name || source.Size > 0 && f.Size != source.Size || (source.Hash != "" && f.Hash != "" && !strings.EqualFold(source.Hash, f.Hash)) || source.Meta["gcid"] != "" && f.Meta["gcid"] != "" && !strings.EqualFold(source.Meta["gcid"], f.Meta["gcid"]) {
		return model.File{}, Fail("file_mismatch", "新建目录中的转存文件与所选源文件不一致，已保留目录")
	}
	return f, nil
}

func (p *tcloudProvider) Transfer(ctx context.Context, c *APIClient, s *Session, src model.Source, files []model.File, batch string) (result *model.TransferReceipt, err error) {
	if e := transferInput(s, src, files, batch); e != nil {
		return nil, e
	}
	info, e := p.shareInfo(ctx, c, s, src)
	if e != nil {
		return nil, e
	}
	share := str(info["shareId"])
	r, e := p.form(ctx, c, s, src, "/file/createFolder.action", map[string]string{"parentFolderId": "-11", "folderName": folderName(batch)})
	if e != nil {
		return nil, e
	}
	folder := str(r["id"])
	if folder == "" || folder == "-11" {
		return nil, Fail("invalid_transfer", "天翼没有返回新建目录 ID")
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
		b, _ := json.Marshal([]object{{"fileId": f.ID, "fileName": f.Name, "isFolder": 0, "srcParentId": f.Meta["parent_id"]}})
		r, e = p.form(ctx, c, s, src, "/batch/createBatchTask.action", map[string]string{"type": "SHARE_SAVE", "shareId": share, "targetFolderId": folder, "taskInfos": string(b)})
		if e != nil {
			return result, e
		}
		task := str(r["taskId"])
		_, taskErr := p.task(ctx, c, s, src, "SHARE_SAVE", task)
		if taskErr != nil {
			return result, taskErr
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
				return result, Fail("invalid_transfer", "天翼任务完成但未出现可核验的新副本 ID")
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
func (p *tcloudProvider) Cleanup(ctx context.Context, c *APIClient, s *Session, r *model.TransferReceipt) error {
	if e := ownedCleanup(ctx, p, c, s, r, "-11"); e != nil {
		return e
	}
	groups := [][]object{}
	fs := []object{}
	for _, f := range r.Files {
		name := f.Meta["own_name"]
		if name == "" {
			name = f.Name
		}
		fs = append(fs, object{"fileId": f.ID, "fileName": name, "isFolder": 0})
	}
	if len(fs) > 0 {
		groups = append(groups, fs)
	}
	groups = append(groups, []object{{"fileId": r.FolderID, "fileName": folderName(r.BatchID), "isFolder": 1}})
	for _, g := range groups {
		b, _ := json.Marshal(g)
		v, e := p.form(ctx, c, s, r.Source, "/batch/createBatchTask.action", map[string]string{"type": "DELETE", "targetFolderId": "", "taskInfos": string(b)})
		if e != nil {
			return e
		}
		if _, e = p.task(ctx, c, s, r.Source, "DELETE", str(v["taskId"])); e != nil {
			return e
		}
	}
	return nil
}
