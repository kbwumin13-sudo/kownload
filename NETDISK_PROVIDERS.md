# 内置网盘 Provider

2026-10-14。代码位于 `internal/netdisk/providers.go` 和 `provider_*.go`，使用 Main 提供的 `APIClient`、`Session` 和模型。这里记录实现范围与 mock 证据；没有访问真实网盘、读取用户 settings/凭据或安装软件。

`BuiltinProviders()` 注册九个 ID，顺序为 `baidu aliyun mcloud tcloud xunlei quark uc 123pan guangya`。所有 provider 按 **Source.ShareID 是否为空** 判断个人/分享，清空 ShareID 后即使 Source.URL 仍是原分享链接，也请求个人接口。目录保持远端 ID；下载保留所选文件身份、相对 Path 和 Size。

| ID | 个人 List | 分享 List | 单文件 Resolve | 自动 Transfer / Cleanup |
| --- | --- | --- | --- | --- |
| baidu | OAuth xpan/file start/limit；Cookie api/list page/num，目录 fs_id 转 path | wxlist，root/dir/page/has_more | OAuth filemetas 按 fs_id 匹配；Cookie disk/home 的 RC4 签名 + api/download 的 dlink[]；分享 tplconfig + sharedownload 按 fs_id 匹配 | OAuth xpan create / Cookie api/create 返回实际 fs_id；share/transfer 或 taskquery 的 to_fs_id 单文件映射；按已核验新目录 ID/路径删除，并等待异步任务 |
| aliyun | v2/file/list，parent_file_id/marker | adrive/v3/file/list，share_token/marker | 逐文件调用下载接口，核验响应 file_id，保留 Referer | createWithFolders 新建批次目录，单文件 v3/batch copy 映射新 file_id；只清理创建副本及专用目录 |
| mcloud | 新个人盘 hcy/file/list，pageCursor | getOutLinkInfoV6 返回当前完整目录；该上游接口无分页游标 | 个人 Mcloud-Sign；分享 AES-CBC 请求 dlFromOutLinkV3，取原文件 CDN 下载地址 | typed unsupported；不覆盖家庭盘、群组盘、旧个人盘 |
| tcloud | listFiles.action，folderId/pageNum | getShareInfoByCode + listShareDir.action，fileId/shareDirFileId/pageNum | accessToken 的 MD5 签名下载；无 token 时使用浏览器 Cookie 的 web API | createFolder 返回实际 id；逐文件 SHARE_SAVE 并等待 taskStatus=4；在该新目录按前后 ID 差集、名字、大小及可用 hash 核对唯一副本；DELETE 等待任务成功 |
| xunlei | files，parent_id/page_token | share / share/detail，pass_code_token/page_token | files/{id} usage=CONSUME；分享 file_info；缺少分享直链时 transfer_required | create files 专用目录、逐文件 share/restore、trace_file_ids / restore_file_ids 及任务轮询；实际 own ID 清理 |
| quark | file/sort，pdir_fid/_page | sharepage/token + detail，stoken/_page | 完整 Quark PC UA / ve=6.9.7.761；acquire_dl_token 内存令牌 + speedup_session；file/download 单 fid 匹配；分享带 fids_token，允许游客无社交令牌 | 仅实际 typed transfer_required 才转存；新建批次目录、逐文件 sharepage/save、task.save_as_top_fids 实际 ID；清理等待 task 完成 |
| uc | 同上，独立 pc-api.uc.cn / UCBrowser 参数 | 同上 | 完整 UC PC UA / Sec-Ch-Ua / ve=1.8.8；个人按 fid 匹配；分享先直接 file/download + fids_token | 仅实际 typed transfer_required 才转存，其余同上 |
| 123pan | file/list/new，parentFileId/Page/Next | share/get，shareKey/SharePwd/Page | v2 download_info / share/download/info；Etag / S3KeyFlag、dispatchList+downloadPath 和 params 解码 | typed unsupported |
| guangya | get_file_list，parentId/page；orderBy=3、sortType=1 | 官方 web JS 的 get_share_page_files_list，accessToken/parentId/cursor/hasMore；支持目录分页 | 个人 get_res_download_url；分享 accessToken + get_share_download_url，captcha/device headers | 官方 create_dir（failIfNameExist）、restore_share 单文件任务、get_task_status；核验新目录中唯一新 ID 后映射；delete_file 等待任务成功 |

九盘个人/分享目录及单文件解析均有具体 API 实现；必要自动转存已覆盖百度、天翼、阿里、迅雷、夸克/UC，光鸭也补齐转存。移动和123的分享文件直接解析，因此尚无自动转存实现，显式 Transfer/Cleanup 返回 typed unsupported。这些是源码适配与 mock 验证的覆盖范围，不能当作九盘真实账号的端到端验收。百度分享目录 ID 转 path 在新 Session clone 中通过 wxlist 目录遍历恢复；个人 OAuth 用 filemetas，Cookie 用 api/list 遍历恢复，最多 10000 个请求。目录游标解析错误和重复分页返回 typed error。

光鸭分享 List 已有官方 endpoint 证据，不是 unsupported；当前契约不含独立 Capabilities 字段，以上矩阵就是能力边界。Main 如需处理验证码或官方接口暂时拒绝，可扩展独立 Capture.listing，传回浏览器可见目录及分页状态，不能混入 selected Files；这属于运行时降级路径，不是本次缺失分享 List 的替代声明。

## 内存认证与下载

认证来自 `Session.Capture`，不读本机网盘扩展 settings。常见字段：

- `Tokens.access_token` / `accessToken` / `Headers.Authorization`；阿里目标个人盘 `default_drive_id`，已列出的每个文件 `Meta.drive_id`。
- 夸克/UC `Share.stoken`；文件令牌放 `Capture.Share["share_fid_token.<fid>"]`，也可从 **Capture.Files** 的临时 Meta 中读取。provider 返回 Listing/File/Receipt 时不携带 share-fid token。
- 夸克社交下载令牌由 Go 请求 `drive-social-api.quark.cn/1/clouddrive/chat/conv/file/acquire_dl_token`，该域名已加入 ProviderInfo.Hosts/APIHosts。令牌及毫秒 `expired_timestamp` 仅存 Capture.Share 的 `social_token` / `social_token_expired_timestamp`；有效期预留 60 秒后才复用，未知有效期则下次重新获取。个人解析保留 typed login_required / expired；分享在社交令牌接口返回这两种认证错误时可提交空 token，网络、验证、配额及无效响应等其他错误原样返回。UC 不调用夸克社交接口，分享直解失败只在明确 typed transfer_required 时交由 Main 转存。下载 Cookie 继续使用 Download.Cookies 的域隔离，不放入 Header。
- 迅雷 `X-Captcha-Token` / `captcha_token`、`X-Device-Id` / `device_id`、`Share.pass_code_token`；光鸭 `share_access_token`、captcha/device。
- 移动 `Authorization`（Basic），或 authorization Cookie；`personalCloudHost` 可指定当前 API 路由，但最终仍受 APIClient 域名白名单约束。
- 百度 OAuth access_token 或 Cookiejar 的 BDUSS / BDUSS_BFESS；仅浏览器 Cookie 的个人 List/Resolve 和自动 Transfer/Cleanup 已编码并通过 mock，无需 OAuth，尚未做真实账号验证。bdstoken、分享 uk/shareid/seckey 保留 Capture.Share；远端路径是非认证 Meta.remote_path。个人下载签名每次从个人 disk/home 获取，不能使用历史分享页的签名。

List 可重新获取分享 token。Service 使用 Session clone 时，这些变更只对本次 clone 可见；Resolve/Transfer 会在需要时重新获取，不能依赖前一 REST 请求的 List 缓存写回。

Download.File 的 Meta 仅保留 drive_id、parent_id、source_id、remote_path、space、etag、s3key_flag。光鸭 GCID 仅作远端身份 Meta，不将其冒充标准 SHA1/MD5 File.Hash。原始 URL、Authorization、Cookie、share-fid token 不进入 File.Meta 或 receipt。百度 OAuth 参数只附加到百度下载域名，下载 URL 留在 Download 内存中。下载认证 Cookie 从 Capture 和 **APIClient cookiejar 在下载 URL 上适用的 Cookie** 合并，jar 新增 Cookie 补当前下载主机 Domain；Header 不携带原始 Cookie 或 API Authorization，不向无关 CDN 导出认证。

接口失败通过 `*netdisk.Error` 返回 login_required、expired、transfer_required、unsupported、invalid_response、file_mismatch 等原因；不会把错误文案作为 URL，也不会在缺少文件列表、下载地址或新建身份时返回空假成功。

## 转存与清理所有权

Transfer 先验证已同步 AccountID，创建 `__gopeed_<batchID>` 专用目录；FolderID 必须来自 create 响应，绝不通过名称查已有目录替代新建 ID。每个复制/转存请求只处理一个源文件，receipt.Files 换为接口确认的实际 own ID，保留原 Name/Path/Size。CreatedIDs 是新文件 ID，FolderID 单独记录；Files.Meta.source_id 和 parent_id 保留非认证映射证据。阿里目标盘来自账号 endpoint 或明确的 default_drive_id，不能拿分享者的 drive_id 当转存目标。

创建目录后的后续错误返回 **receipt + error**。已确认新 ID 逐项积累；轮询失败若响应已提供新 ID，仍记录副本。百度同步及异步结果的身份若发生变化，保留响应提供的全部新 ID 并返回 invalid_transfer。没有可验证 ID 的未知创建不会猜测，已知目录和副本保留供 Main 持久化 transfer_failed。百度 create 使用 rtype=1 防止覆盖，响应路径必须等于本批专用路径，否则保留 create 返回的 FolderID 并报错；同名文件冲突不会覆盖已有副本。

天翼 SHARE_SAVE 和光鸭 restore_share 的公开接口只返回 taskId，未提供复制 ID 映射。实现不查找已有同名目录，而是在本次 create 响应确认的全新目录中，逐文件任务前核对已有 ID，任务成功后读取该目录全部分页；只接受唯一新增文件，并核对名字、大小及可用校验值。额外文件、缺少新 ID、属性不一致或任务失败均保留 receipt 并报错。这是“创建响应确定目录 + 单文件任务 + 远端 ID 差集”的所有权证据，区别于百度/阿里/夸克/迅雷的直接响应映射；真实平台仍需验证该流程及目录可见延迟。

Cleanup 核对 provider、同账号、批次、实际创建身份，远端根目录中的同 FolderID 必须仍叫 `__gopeed_<batchID>`；同时检查目录全部分页。目录含外来文件、身份重复、缺少 source_id / parent_id 或仍有 Remaining 下载时拒绝整个清理。删除请求只包含 CreatedIDs，随后删除专用 FolderID；不发送分享原 ID，不按名字清理其他目录。空专用目录的失败 receipt 可在核验后清理。Main 决定何时满足全批次完成条件并调用 Cleanup。

## 来源与归属

- 固定上游 [hmjz100/LinkSwift](https://github.com/hmjz100/LinkSwift)，源码 `../artifacts/linkswift-2026-10-14/upstream/LinkSwift-{stable,dev}.user.js`，作者 hmjz100 / 油小猴，上游 AGPL-3.0 归属及完整 LICENSE 保存在同目录。本实现保留该参考归属；未照搬开发版阿里 getFilesUrl 的 batch.forEach 覆盖错误。
- Primary：[OpenListTeam/OpenList](https://github.com/OpenListTeam/OpenList/tree/482464c2affc5eeba5d4a25bb272aeb352ec5918/drivers)，2026-10-14 通过 `gh api` 读取 main，核对时 SHA `482464c2affc5eeba5d4a25bb272aeb352ec5918`；189 / 189pc / aliyundrive / aliyundrive_share / quark_uc / guangyapan / 123_share / thunder_browser。139 / 123 使用同目录 `upstream/openlist/*` 缓存。
- 天翼分享目录补充参考 [bookstairs/bookhunter telecom/share.go](https://github.com/bookstairs/bookhunter/blob/master/internal/driver/telecom/share.go)，通过 `gh api` 获取源文件。
- 天翼 SHARE_SAVE、创建/删除/批次轮询补充参考 [1307super/cloud189-auto-save](https://github.com/1307super/cloud189-auto-save)，`src/services/cloud189.js` / `src/services/task.js`；不采用其中默认根目录转存与覆盖策略。
- 百度转存及 Cookie 个人接口补充参考 [qjfoidnh/BaiduPCS-Go transfer.go](https://github.com/qjfoidnh/BaiduPCS-Go/blob/main/baidupcs/transfer.go)、[prepare.go](https://github.com/qjfoidnh/BaiduPCS-Go/blob/main/baidupcs/prepare.go)、[panhome/sign.go](https://github.com/qjfoidnh/BaiduPCS-Go/blob/main/baidupcs/internal/panhome/sign.go)、[panhome/parse.go](https://github.com/qjfoidnh/BaiduPCS-Go/blob/main/baidupcs/internal/panhome/parse.go)、[GDownload baidu_api.js](https://github.com/cool2528/GDownload/blob/main/plugins/baidu-netdisk/lib/baidu_api.js)、[BaiduPCS-Rust client.rs](https://github.com/komorebiCarry/BaiduPCS-Rust/blob/main/backend/src/netdisk/client.rs)。采用 to_fs_id，不采用 GDownload 把 from_fs_id 当成转存文件 ID 的逻辑。
- 光鸭官方网页 [index.b1e7c5bb.js](https://www.guangyapan.com/static/js/index.b1e7c5bb.js)，读取日期 2026-10-14，SHA256 `f5eeb594b6c622c87d87e91dfc22110076d3351e137b46f577cb4630e06c2347`。hB/hV/hU/f6/hI/f8 及 CY/wp/wd 的调用提供 token、分享父目录/cursor、转存、任务状态和创建/删除参数证据。
- 本机扩展公开源码：`monkeyWie@gopeed-extension-baiduwp/src/api/{share,open}.js`、`foxxorcat@gopeed-extention-quarkuc/src/index.js` 及其打包 SDK 请求代码、`cybdarlnig-png@xunlei-pan/index.js`。只阅读代码，不加载配置和存储。

## Mock 验证

`adapters_test.go` 和 `adapter_*_test.go` 使用 http.RoundTripper 拦截，未知 endpoint 立即失败，不访问真实网盘。覆盖九 ID、个人/分享目录参数与分页、超过 float64 精度的百度 ID、单文件响应身份和阿里并发错配回归、移动/123固定签名向量、天翼签名、分享令牌不落 File.Meta、expired、Cookiejar 新 Cookie 和域隔离、域名白名单、实际新建 ID、部分转存 receipt 与错误、同账号/目录外来文件/源文件不删除等边界。新增百度 Cookie-only 个人/转存/清理、RC4 固定向量、dlink[] 身份匹配、异步失败与身份变化追溯、天翼/光鸭单文件 task 与新目录差集及第二任务失败 receipt、光鸭官方分享 cursor 回归。Main 的 service/REST/Fetcher 集成测试由其所属文件维护。

本轮最终 `go test -race` 的 internal/netdisk 和 internal/protocol/netdisk 均通过；pkg/protocol/netdisk 无测试文件。四个相关 package 的 `go vet` 通过。全量 pkg/rest race 包含既有非网盘测试，未等待其完成，主动停止，不将其列为通过；定向 TestNetdiskBrowserRESTContract 的 race 测试通过。真实账号下载、转存和删除均未由本 worker 执行，Main 接管真实验收及重新打包。


## Main 实机增补（2026-10-14）

已通过现有 Chrome 助手同步夸克网页登录态，以内置 netdisk 任务下载用户提供的分享原文件「巨构案例.zip」。实际大小 439278761 字节，78 个 ZIP 条目的 CRC 全通过；暂停、应用退出、重启、重新同步后继续同一任务和断点通过。文件 SHA256 为 48d64b4ca0bcea80a709cc1f67739c8498252ee92f1493ed733714ce44603653。没有转存该文件或清理任何用户云端原文件。

原始个人/分享文件以来源身份和文件接口的权限核验恢复。转存副本仍以账号和实际创建 ID 核验；缺少稳定 UID 的平台会拒绝重认证后的清理，不通过 Cookie 指纹变化推断账号一致。账号接口字段和真实转存流程仍需补验，不能将该分享下载结果外推为所有盘、目录或副本清理已实测。
