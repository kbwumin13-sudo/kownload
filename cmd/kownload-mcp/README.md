# Kownload MCP Helper

独立 Go 模块，目标 macOS arm64。默认只注册五个读取工具，经 stdio 调用已有 App 的固定 UDS HTTP 服务。显式可信 `--write-policy-file` 可额外注册创建、暂停、继续三个工具，仍须 Core 的实际能力与逐次授权；不初始化 Downloader、不监听端口、不启动 App、不读取数据库或日志，也不提供任意 HTTP/shell 代理。

**当前源码已在实际 `BuildServer` 的 UDS 路由上接入读取适配器，沿用原 API 鉴权；已用合成数据、私有 UDS 和实际 mcporter 验证。仍是部分五读：`totalBytes` 仅在合法 done 快照中提供记录的最终 Downloaded 总量，其余状态为 null；直接 HTTP 的 activeStreams/targetStreams 与已接受的分类错误已由既有采样和当前 run/source 门禁发布；暂停、自动冷却让槽、新 run、换源、成功和删除均清理旧缓存。其它协议未提供的 peers 等字段仍为 unavailable，未用零补齐。真实 BuildServer/HTTP/stdio/mcporter 专项通过，正式 App 中这条新 publisher 链尚未验收。** 已安装 build 39 没有这些端点；默认受管 IPC 父目录的私有权限增量已并入源码；f1a54d8 同源隔离 App、包内 Helper、实际 mcporter 十次 GET 与 socket 自愈已验证。该 preview 无合法非空 token 入口，401 矩阵仅有实际 BuildServer 夹具证据。桥不会降级到旧任务 API 或会恢复 fetcher 的 `Stats`。

## 固定依赖与构建

- Go SDK：`github.com/modelcontextprotocol/go-sdk v1.7.0`，来源 SHA `bc72835f62eb94d0fb484439f886b6885b075f36`。
- MCP 主规范：2026-07-28；保留 SDK 的 2025-11-25 初始化兼容。协议协商交给 SDK，App 读 API 版本独立固定为 1。
- 独立 `go.mod`/`go.sum`，不改变根模块。使用项目唯一恢复的 Go 1.25.4；`GOTOOLCHAIN=local`，不自动恢复第二份 SDK。

```sh
cd cmd/kownload-mcp
GOTOOLCHAIN=local "$GO_BIN" test -race ./...
GOTOOLCHAIN=local "$GO_BIN" build -trimpath -o /tmp/kownload-mcp .
/tmp/kownload-mcp --version
```

构建时可用 `-ldflags '-X main.version=<固定发行版本+提交>'` 注入桥版本。开发默认 `0.1.0-dev`。本模块代码沿仓库主 LICENSE；依赖原始许可与哈希保存在 `licenses/`。SDK 固定 LICENSE 包含 MIT→Apache-2.0 迁移说明，不应改标为纯 MIT。`go.sum` 固定模块内容，后续正式源码收集需包含本模块的模块源码 ZIP 和许可，交由构建负责人接线；本增量不修改共享打包脚本/third_party。

## 五读接口 v1（当前部分数据能力）

所有请求只能是 GET，携带 `X-Kownload-Read-Version: 1`；响应为 `{"version":1,"data":<DTO>}`。未知 envelope/DTO 字段均拒绝，不向客户端转发原始响应。无端点或任务返回 404；不把 404 当“空任务”，不探测旧 API。版本不兼容失败关闭。正文最大 128 KiB，HTTP 响应头最大 8 KiB；不跟随重定向。单次超时 3 秒，最多 4 个同时读取，第 5 个返回 busy。

| MCP 工具 | 输入 | 固定 App GET | DTO |
| --- | --- | --- | --- |
| `kownload_list_tasks` | `limit` 必填 1…100，`cursor` 可选 | `/api/v1/mcp/read/v1/tasks?limit=…&cursor=…` | `{tasks:Task[],nextCursor:string}` |
| `kownload_get_task` | `id` | `/api/v1/mcp/read/v1/tasks/{id}` | Task |
| `kownload_get_progress` | `id` | `/api/v1/mcp/read/v1/tasks/{id}/progress` | Progress |
| `kownload_get_stats` | `id` | `/api/v1/mcp/read/v1/tasks/{id}/stats` | Stats |
| `kownload_get_errors` | `id` | `/api/v1/mcp/read/v1/tasks/{id}/errors` | LastError |

`id` 为 1…64 个 `[A-Za-z0-9_-]`。cursor 为至多 128 个同字符集字符，空字符串表示末页；只能编码分页位置，不能携带私密 URL、路径、令牌。App 应提供稳定分页语义，单页任务不可重复；桥拒绝超过请求 limit 的结果。S7a stats 为单任务，不伪装全局统计。 `available:true` 必须至少包含一个真实已知的统计字段；已知 0 保留为数字 0，未知字段省略。`available:false` 不携带统计或错误字段，不能据此判断为零连接或无错误。纯 DTO 测试不代表 publisher；当前直接 HTTP 的真实发布证据来自 BuildServer/HTTP/stdio/mcporter 专项，正式 App 验收仍单列。

Task：`{id,protocol,status,createdAt,updatedAt,progress}`。

- protocol：`http | https | bt | ed2k | media | netdisk | unknown`。
- status：现有 `ready | wait | running | pause | done | error`；不新增持久状态。
- createdAt/updatedAt：有效 RFC3339 时间，不含任意文本。get 返回 id 必须等于请求 id。
- 无 name、path、URL、header、req/res/opts、token、任意 Extra；文件名本身可能含私密信息，因此首版完全不返回。

Progress：`{downloadedBytes,speedBytesPerSecond,uploadedBytes,uploadSpeedBytesPerSecond,totalBytes,phase}`。

- 数值为非负且不大于 JS 安全整数上限 `9007199254740991`；totalBytes 未知时为 null，不能用零冒充未知。
- `totalBytes` 仅当同一次 `dataLock` 快照的状态为 done、已有 Progress 且所有标量合法时，取记录的最终 `Progress.Downloaded`，合法0保留为0。活动、暂停、错误等非done状态保持null；缺Progress或非法标量沿用 snapshot unavailable，不生成补值DTO。该数值不代表磁盘、资源大小或摘要复验，不读取Meta.Res/文件/配置/日志，也不调用fetcher。
- phase：`unknown | queued | resolving | downloading | merging | verifying | publishing | done`，仅映射已有内存快照；未观测到的 phase 用 unknown，不为读桥引入持久阶段。
- 读取缓存 Progress，不能触发 fetcher 恢复/调用网络。不以 downloaded==total 推断成功；最终状态依 Task.status。

Stats：`{available:false}`，或 `{available:true,activeStreams?:int,targetStreams?:int,peers?:int}`。字段采用相同非负安全整数范围。App 必须提供无恢复快照；未加载/没有快照时 unavailable，不能调用现有恢复式 `Downloader.Stats`。available=false 时不得同时带计数；available=true 的缺字段表示该协议未提供，不能补零冒充观测。

读取适配器只在 `Task.dataLock` 下复制可选 `mcpReadPublishedSnapshotLocked()` 的纯值结果，锁外验证并构造 DTO。没有接口实现或没有观测时返回 `available:false`，不查询 fetcher、存储或日志。每个统计字段独立记录是否已知；合法 0 不丢失，任一已知计数越界返回 snapshot unavailable，未知字段不补值。

LastError：`{available:false}`，或 `{available:true,category,retryable,occurredAt}`。

- category：`network | authentication | integrity | storage | cancelled | unknown`；由 App 明确分类，不传错误原文。
- retryable 原样复制生产侧接受的 typed 策略，不按任务状态推测。occurredAt 由真实事件的 Unix 毫秒转换为 UTC RFC3339；已知事件的毫秒范围为 1…253402300799999，越界或未知分类返回 snapshot unavailable。
- producer 负责当前 run/sourceRevision 守卫，以及新 run、换 source、暂停失效和成功时清空；迟到旧事件不得发布。读取侧不映射原始错误、不发明重试策略、不重建历史。生产薄点尚未接入，纯值/锁行为测试不算真实传输、错误或恢复的端到端验收。

读取端实现必须不触发 restore/Start/Prepare/持久写入或网络，不在主锁内等待客户端/协议统计。桥端通过 schema 和类型重建 DTO，额外检查 id、重复记录、时间及 unavailable 语义。API 任意错误正文、路径、堆栈均丢弃。工具错误仅返回固定分类：`busy / app_unavailable / app_authentication_failed / cancelled / endpoint_or_task_unavailable / incompatible_read_api / invalid_app_response`。输入非法为 JSON-RPC -32602。默认只有以上五个注册工具；annotations 是提示，不承担权限控制。

## 传输与权限边界

默认固定 socket：`~/Library/Application Support/io.github.kbwumin13-sudo.kownload/ipc/api.sock`。`--socket <绝对路径>` 仅为可信宿主启动配置/隔离测试；工具参数不能改变 socket、host、路径、method 或请求头。

每次连接检查规范绝对路径及所有组件无符号链接，祖先目录 owner 为 root/当前 UID 且非共享可写（上层 sticky 临时目录例外）；socket 必须是文件系统 UDS、属当前 UID、无 group/world 写位，直接父目录必须属当前 UID 且无任何 group/world 访问位。私有父目录阻止其他 UID 到达 socket，因此 socket 自身的读/执行位（例如 0755）可以保留；父目录非私有时，即使 socket 为 0600 也拒绝。连接后检查 Darwin 内核 peer UID。

同 UID 的进程仍在信任范围内；UID/产品名不证明对端是可信 App，也不抵抗同 UID 恶意进程。桥不 chmod、不创建/删除 socket、不修复目录或生产权限。实际 `BuildServer` 的原 listener、自愈机制和进程 umask 均未改动。默认 App 启动分支已接入受管 IPC 目录检查：仅对确认归属的默认数据根下 ipc 建立/收窄为 0700，拒绝链接、异主和未知类型，不递归修改子项，不改变自定义地址。同源隔离 App 启动仍待联调；本模块不提供手动 chmod 全数据目录的补救流程。

### 可选显式 API 凭据

`--api-token-file /ABSOLUTE/PRIVATE/token` 仅为用户显式准备的可信启动配置，不是 MCP 工具输入。桥不读取 Hive、数据库或其他应用配置，不自动发现/生成/导出 token，不登录 Web、不使用 Bearer 替代鉴权。未配置文件时不发送 token；App 开启 API token 或仅开启 WebAuth 时，缺少适用凭据会返回固定 `app_authentication_failed`。API token 正确时按原中间件规则通过，包括同时开启 WebAuth 的情况。

文件在启动时只读一次：规范绝对路径，逐级无符号链接、可信且非共享可写祖先（sticky 临时祖先例外），当前 UID 的私有父目录；常规文件、当前 UID、无硬链接，权限严格 0400/0600。采用逐级目录 FD + no-follow 打开并核对文件身份，拒绝 FIFO/目录/链接，最多读取 4096 字节；内容是非空可打印 ASCII 单 token，可有一个结尾 LF，拒绝空白、CRLF、控制符和多行。文件不可用或不安全即启动失败，只输出固定结束信息，不输出文件路径/内容。轮换文件不会改变运行中桥的凭据，新进程读取新值。

内存 token 只加入上述固定读端点与显式启用后的专用写端点的 `X-Api-Token`，不跟随重定向，不使用 HTTP 代理，不将请求头、错误正文或凭据返回到 MCP。原 API token 是全局 API 凭据，**没有被降权为只读 token**；权限限制属于桥的可信 policy、Core 授权和固定请求集合，不能改变持有该凭据的其他进程权限。凭据文件私有检查不因 socket 权限组合调整而放宽。

客户端配置只可包含显式文件路径，不内嵌 token。mcporter `list --json` 的外层 `transport` 会展示其本地启动参数；它属于客户端配置元数据，与 MCP `tools` schema、工具结果和错误输出分开核验。桥工具 schema/结果不含 token 文件路径或内容。

stdio 单行最多 64 KiB，tool arguments 最多 4 KiB，输出单次写入最多 256 KiB，慢读端写入最多等待 5 秒后结束桥。stdout 仅协议帧（显式 `--version` / `install-helper` CLI 除外），SDK 日志丢弃，stderr 只给固定结束信息。消息/返回上限不等于下载总字节配额：不拒绝正常未知 Content-Length/chunked 资源，不承诺内核下载硬限额。

退出只结束桥；查询取消仅取消自身 UDS 请求，绝不暂停/删除 App 任务。没有 App 时返回 unavailable；无自动启动/重试创建路径。

## 显式安装与升级 Helper（macOS arm64）

独立 CLI 子命令由用户选择可信 App；它不是 MCP 工具，也不会启动 App、监听端口、设置自启或读写客户端全局配置：

```sh
/ABSOLUTE/TRUSTED/Kownload.app/Contents/Helpers/kownload-mcp install-helper \
  --bundle /ABSOLUTE/TRUSTED/Kownload.app \
  --data-root "$HOME/Library/Application Support/io.github.kbwumin13-sudo.kownload"
```

所有参数是可信启动配置，不能由模型工具参数传入。`--data-root` 必须严格等于当前 HOME 的默认受管根，而且已存在、属当前 UID、没有 group/world 写权限；不接受任意绝对目录。路径逐层拒绝符号链接；既有 `bin` 必须属于当前 UID 且为 0700，缺失时只在已验证根中创建这个目录，不修复既有权限。普通入口固定为 `bin/kownload-mcp`、当前 UID、0700、无硬链接。

只接受固定 `Contents/Resources/build-manifest.json` 和 `Contents/Helpers/kownload-mcp`：
验证 App/Helper strict 签名、arm64 Mach-O、40 位源码 SHA、顶层与 MCP 版本配对、read-api=1、SDK v1.7.0、最终 Helper SHA256，
再复制到同目录私有 staging。manifest 最大 64 KiB、Helper 最大 128 MiB；这些是安装文件限额，不是下载任务配额。
临时 inode 写入安装 xattr 并读回、fsync 后，核暂存 Helper 的签名与精确 `--version`，再复核摘要/身份并原子 rename。
不执行未知旧入口来判断身份。发布前任何校验或写入失败均保留旧入口；rename 后目录 fsync 失败会明确报告“已发布、持久性未确认”，不会谎称回滚。

目录锁串行化合作的安装进程，首次发布用排他 rename，升级前重新核旧 inode 与内容。
新进程使用新版本，活动 stdio 进程继续持有旧 inode，不被终止。
客户端 command 固定为稳定入口绝对路径，args 和配置字节无需随升级变化；本 CLI 不读取或重写它们。
回退也须显式选择与 App 配对的旧版本；桥安装成功不证明 Core 数据可以回退。

安装记录 xattr 固定为 `io.github.kbwumin13-sudo.kownload.mcp-install`，最多 2048 字节，严格拒绝未知字段、重复字段与未知版本：

```json
{"recordVersion":1,"product":"kownload-mcp","sourceCommit":"<40位小写hex>","version":"<releaseVersion>+<同SHA>","readApiVersion":1,"architecture":"arm64","sha256":"<已安装字节的64位小写hex>"}
```

升级要求旧入口的有效记录与实际摘要一致；未知文件、外来 UID、链接、缺失/不符记录均拒绝，不自动认领。xattr 不可用或写失败时不发布无标记入口。
**xattr 是意外冲突和版本归属记录，不是安全认证；同 UID 进程在信任范围内，可伪造记录。** 包签名和摘要证明选择的包内一致性，ad-hoc 签名或自报产品名不证明发布者身份；所选 bundle 的信任由用户明确决定。
源 Helper 或 bundle 的 `com.apple.quarantine` 会保留，旧入口的标记不会被清除；不兼容的隔离标记拒绝升级，不改 Gatekeeper。受系统信任策略阻止执行时失败关闭，不绕过。

S0：旧桥忽略该 xattr，旧 CLI 不支持此命令；App 与桥须按 manifest 配对。备份/恢复应同时保留字节和 xattr，丢标记后升级拒绝。当前备份工具是否保留此新属性须独立验证；不能仅凭普通文件还原宣称兼容。

本模块 `tests/install_helper_smoke.py /ABSOLUTE/GO` 在私有 HOME 下构建两版真实桥、签名已知合成 bundle，再通过真实 CLI 首装/升级；实际 mcporter 执行五读，另保留旧 stdio 进程跨升级继续读取。夹具的两个 source ID 只是测试标签，不冒充发行源码；签名测试不是发布者认证。正式同源 App/桥配对部署须另与构建负责人协调，生产部署未执行。

隔离 mcporter 配置模板（不写入全局 MCP 配置；正式路径须由用户确认）：

```json
{"imports":[],"mcpServers":{"kownload":{"command":"/ABSOLUTE/NEW-DATA-ROOT/bin/kownload-mcp","args":[]}}}
```

## 显式三写 v1

默认启动仍仅五读。只有本机可信启动参数 `--write-policy-file /ABSOLUTE/PRIVATE/policy.json` 才注册 `kownload_create_task`、`kownload_pause_task`、`kownload_resume_task`。文件沿共享安全 loader 在启动时只读一次，当前 UID、0400/0600、单硬链接常规文件、私有父目录及逐级 no-follow，最多 4096 字节。文件错误即启动失败；模型不能传 policy、文件路径、token、`confirmed` 或 `allowedEffects` 授权。配置更新须显式重新启动；桥不写入客户端全局配置。

三个工具各有三种 `mode`，没有通用 REST 或独立第九个查询工具：

| mode | 必需字段 | 固定 backend 行为 |
| --- | --- | --- |
| `inspect` | `datasetId`；create 加 `outputRootId`，action 加 `taskId` | 只 GET `/api/v1/mcp-write/v1/capabilities`，取得当前 P 及实际可信任务 tuple |
| `submit` | `datasetId,policyId,operationId`；create 加 `create:{url,outputRootId,fileName}`，action 加 `action:{taskId,expected:{episode,generation,sourceRevision}}` | 唯一一次 POST `/api/v1/mcp-write/v1/operations` |
| `query` | 原 `datasetId,policyId,operationId` | 只 GET `/api/v1/mcp-write/v1/operations/{operationId}`；不提交、不恢复、不换 ID |

参数精确区分大小写，拒绝未知/重复键、两个互斥 payload、错误类型和越界整数；operation ID 为 1…128 个字母数字及 `._:-`，首位须字母数字。初版创建仅 HTTP(S)、无 URL userinfo/fragment，文件名必须单一组件；不接任意 headers、Options、目标路径或模型指定副作用。网络、实际重定向/IP、输出根身份和 noReplace 仍由 Core 执行边界负责，首 URL 的词法检查不代替它们。

回执状态为 `unknown/pending/settled/not_recorded`：unknown/pending 要求继续同 op 查询；not_recorded 只表示同 D 权威读确认没有 R，kind/result 为 null，不能据此授权新执行。created/applied 必须有真实 task ID，rejected 可以没有尚未创建的 task；未配置的 cap P/network ID 为 null 且全部操作 false。

响应仍为 `{version:1,data:...}`，工具结果为 `{mode,capability,receipt}`，未使用的一项显式 null。capability 的 P 仅标识当前可信 authority，不是凭据。三写结果只含 dataset/policy/operation/task ID、固定状态/错误码及 tuple；不回传 URL、路径、原请求、ExecutionBinding、headers、错误原文或摘要。

提交超时、断连、非鉴权错误响应或无法验证的回执均按 ACK 未知处理：保原 operation ID、`state:unknown,queryRequired:true`。后续只查询原操作；桥无 POST 重试、旧 create fallback、自动换 ID 或后台轮询。Core 在同 `(D,mcp-write/v1,op)` 下对 kind 与 caller raw 做幂等裁决，首次 ExecutionBinding 独立持久冻结；同 op 不能换 kind 或刷新 expected tuple 伪装新动作。HTTP 401 仅输出固定鉴权失败，不回显正文。

暂停/继续仍须当前可信 policy 显式 task ID 范围及真实 D/E/G/sourceRevision。新创建的回执不自动扩 scope。P 更新后可用原 P 查询旧回执；新提交必须核当前 P。继续不是 Retry，不重置恢复预算。新 MCP 任务的完成脚本、自动打开、webhook、自动后处理默认抑制，Core 持久绑定其创建上限；后续授权不能扩张历史上限。GUI 任务行为与专用 MCP 创建标签分开。

本节描述 Helper 的已实现请求边界；Core/REST/state 与最终同源 App 三写验收须在成套集成后核证，不能用受控 UDS 的协议测试代替真实创建、暂停和继续。

## 验证范围

`go test`/`go test -race` 使用隔离 UDS 和官方 SDK；包含五工具 schema、读路径、非法输入/未知写工具、私密字段/异常响应、限额、超时/取消、并发与 socket 边界。`tests/mcporter_smoke.py` 使用本机已有 mcporter CLI 和真实构建的桥，执行 tools/list、五读、写调用拒绝及两版本稳定路径模拟；不是自制客户端替代测试。

新增 `pkg/rest/mcp_read_integration_test.go` 使用实际 `BuildServer`、原认证中间件、私有合成 Bolt 数据与原 UDS listener；覆盖鉴权矩阵、TCP 不注册、Web 兜底顺序、S2 三条 operation 路由共存、405、五读及 socket 自愈，不手动 chmod socket。设置 `KOWNLOAD_MCP_TEST_BINARY=/ABSOLUTE/TEST-BRIDGE` 后运行 `TestMCPReadBuildServerCLI`，通过已有 mcporter 与真实 stdio 二进制验证五读、错误/缺失 token、文件启动拒绝、凭据路径稳定/轮换和自愈后五读。测试配置与客户端 HOME 均为临时目录；未设置该变量时此 CLI 集成用例显式跳过，不能将其计为已验证。

远端工作流新增本独立模块的 `go mod verify` 与 `go test -race ./...`；根模块 `./...` 本身不覆盖此模块。真实 mcporter 集成仍须显式设置测试二进制，不能从 CI 单测通过推断已完成 App 联调。

尚待：S3 安全统计/错误发布快照与有效总量、普通 App 非空 token 联调、包含本安装 CLI 的正式同源包配对部署、获准生产稳定路径部署与客户端配置迁移。当前不接触生产 App、日常浏览器、用户任务或凭据。
