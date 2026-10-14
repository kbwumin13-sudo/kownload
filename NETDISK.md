# 内置网盘下载接口

2026-10-14。正在实现用户确认的 LinkSwift 整合。Main负责模型/service/client/netdisk Fetcher/REST。Provider worker只写 provider实现和测试；Browser worker只写 browser-media-extension；Flutter worker只写 Flutter界面相关文件。保留工作树原改动。

Provider IDs: `baidu aliyun mcloud tcloud xunlei quark uc 123pan guangya`。

公开模型：`pkg/protocol/netdisk/model.go`；provider契约：`internal/netdisk/provider_contract.go`；带cookiejar的API客户端：`internal/netdisk/client.go`。

## REST（助手forward path不带/api/v1）

- GET `/netdisk/providers` -> ProviderInfo[]。
- POST `/netdisk/browser/request` `{source,instanceId?}` -> BrowserRequest。
- GET `/netdisk/browser/requests/{id}` -> BrowserRequest；DELETE同一路径取消。
- POST `/netdisk/browser/claim` `{requestId?,sourcePage,instanceId,browser,contextId}` -> null 或 `{request:BrowserRequest,nonce}`。
- POST `/netdisk/browser/capture` `{requestId?,nonce?,capture:Capture}` -> `{sessionId,source,files}`。无requestId仅用于明确网页用户点击；等待App请求需要nonce及同一instance/context/share绑定。
- POST `/netdisk/list` `{source,sessionId,cursor?,proxy?}` -> Listing。
- POST `/netdisk/prepare` `{source,sessionId,files,opts,proxy?}` -> `{batchId,reqs:CreateTaskBatchItem[]}`。只能在用户确认所选文件后调用，会在必要时临时转存。
- GET `/netdisk/transfers` -> TransferReceipt[]；POST `/netdisk/cleanup` `{batchId}` 重试完成批次清理。
- POST `/tasks/batch` `{reqs}` 创建prepare返回的任务。

REST标准返回 `{code:0,msg:"",data}`。网盘错误返回 `{code:1,msg:中文信息,data:{reasonCode:login_required|transfer_required|...}}`。不打印原始Cookie/token。Capture鉴权仅内存，公开file及task extra过滤临时认证信息。

App打开选定profile时只在来源URL添加公开相关标识`gopeed_request=<requestId>`，它不是认证或capture权限；nonce仍只在Native桥内返回。未配对请求只接受该标识对应的页面认领，之后绑定instance/context，同instance换document时旋转nonce。任务/来源URL去掉此标识，避免其他profile现有标签抢先认领。

## 网页选择进入App

捕获session后调用Native `create`（非silent），params：
`{req:{url:sourcePage,protocol:"netdisk",extra:{provider,source,sessionId,files:选中文件}},opts:{}}`。
Flutter以此显示网盘确认及保存位置，调用prepare及tasks/batch。

## Provider实现

`BuiltinProviders() []Provider` 提供全部九类。List支持parentId与cursor；Resolve只映射当前file，Download包括URL/Header/Cookies/File。Transfer返回实际创建ID、专用目录及映射后的Files。Cleanup只能删receipt创建的副本。无可用能力时返回明确错误，不返回假成功。临时转存全批成功才清理，暂停/失败保留；receipt持久化，认证不持久化。

上游固定源码保存在项目根 `artifacts/linkswift-2026-10-14/upstream/`，保留AGPL归属。开发版阿里getFilesUrl批量覆盖错配不可照搬。核心下载复用HTTP Fetcher，以netdisk协议保存原文件身份和断点状态，恢复后需要浏览器重新同步同一账号。


## 原文件恢复与副本账号

原始个人文件与分享原文件恢复时，以原来源、文件 ID、大小和可用 hash 重新核验，平台下载接口核对当前会话的文件权限。浏览器账号 ID 不可用时，瞬时 Cookie 指纹变化不会使原文件断点失效；只选用用户重新打开该来源同步的会话。转存副本和清理继续要求回执中的原账号，禁止用这一恢复分支处理副本。

本机实际夸克账号接口未直接返回 UID，仅返回 nickname、avatarUri、mobilekps、config；不把昵称或 mobilekps 当成账号 ID。未知稳定 UID 的副本在重新认证后可能需要额外的账号适配，清理保持拒绝状态。真实副本恢复与清理需按 NETDISK_PROVIDERS.md 补验。
