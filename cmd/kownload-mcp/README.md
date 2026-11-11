# Kownload MCP 五读薄桥（S7a）

独立 Go 模块，目标 macOS arm64。只注册五个读取工具，经 stdio 调用已有 App 的固定 UDS HTTP 服务；不初始化 Downloader、不监听端口、不启动 App、不读取数据库或日志，也没有写工具或任意 HTTP/shell 代理。

**当前源码已在实际 `BuildServer` 的 UDS 路由上接入读取适配器，沿用原 API 鉴权；已用合成数据、私有 UDS 和实际 mcporter 验证。仍是部分五读：`totalBytes` 始终 null，stats/errors 始终 unavailable。** 已安装 build 39 没有这些端点；默认受管 IPC 父目录的私有权限增量已并入源码，同源隔离 App 联调及部署仍待验证。桥不会降级到旧任务 API 或会恢复 fetcher 的 `Stats`。

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

`id` 为 1…64 个 `[A-Za-z0-9_-]`。cursor 为至多 128 个同字符集字符，空字符串表示末页；只能编码分页位置，不能携带私密 URL、路径、令牌。App 应提供稳定分页语义，单页任务不可重复；桥拒绝超过请求 limit 的结果。S7a stats 为单任务，不伪装全局统计。

Task：`{id,protocol,status,createdAt,updatedAt,progress}`。

- protocol：`http | https | bt | ed2k | media | netdisk | unknown`。
- status：现有 `ready | wait | running | pause | done | error`；不新增持久状态。
- createdAt/updatedAt：有效 RFC3339 时间，不含任意文本。get 返回 id 必须等于请求 id。
- 无 name、path、URL、header、req/res/opts、token、任意 Extra；文件名本身可能含私密信息，因此首版完全不返回。

Progress：`{downloadedBytes,speedBytesPerSecond,uploadedBytes,uploadSpeedBytesPerSecond,totalBytes,phase}`。

- 数值为非负且不大于 JS 安全整数上限 `9007199254740991`；totalBytes 未知时为 null，不能用零冒充未知。
- phase：`unknown | queued | resolving | downloading | merging | verifying | publishing | done`，仅映射已有内存快照；未观测到的 phase 用 unknown，不为读桥引入持久阶段。
- 读取缓存 Progress，不能触发 fetcher 恢复/调用网络。不以 downloaded==total 推断成功；最终状态依 Task.status。

Stats：`{available:false}`，或 `{available:true,activeStreams?:int,targetStreams?:int,peers?:int}`。字段采用相同非负安全整数范围。App 必须提供无恢复快照；未加载/没有快照时 unavailable，不能调用现有恢复式 `Downloader.Stats`。available=false 时不得同时带计数；available=true 的缺字段表示该协议未提供，不能补零冒充观测。

LastError：`{available:false}`，或 `{available:true,category,retryable,occurredAt}`。

- category：`network | authentication | integrity | storage | cancelled | unknown`；由 App 明确分类，不传错误原文。
- retryable 为 bool，occurredAt 为有效 RFC3339。
- App 可在原错误路径维护有界内存快照；缺历史返回 unavailable，不抓日志/私密数据库，不把 unavailable 等同于没有错误。

读取端实现必须不触发 restore/Start/Prepare/持久写入或网络，不在主锁内等待客户端/协议统计。桥端通过 schema 和类型重建 DTO，额外检查 id、重复记录、时间及 unavailable 语义。API 任意错误正文、路径、堆栈均丢弃。工具错误仅返回固定分类：`busy / app_unavailable / app_authentication_failed / cancelled / endpoint_or_task_unavailable / incompatible_read_api / invalid_app_response`。输入非法为 JSON-RPC -32602。只有以上五个注册工具；annotations 是提示，不承担权限控制。

## 传输与权限边界

默认固定 socket：`~/Library/Application Support/io.github.kbwumin13-sudo.kownload/ipc/api.sock`。`--socket <绝对路径>` 仅为可信宿主启动配置/隔离测试；工具参数不能改变 socket、host、路径、method 或请求头。

每次连接检查规范绝对路径及所有组件无符号链接，祖先目录 owner 为 root/当前 UID 且非共享可写（上层 sticky 临时目录例外）；socket 必须是文件系统 UDS、属当前 UID、无 group/world 写位，直接父目录必须属当前 UID 且无任何 group/world 访问位。私有父目录阻止其他 UID 到达 socket，因此 socket 自身的读/执行位（例如 0755）可以保留；父目录非私有时，即使 socket 为 0600 也拒绝。连接后检查 Darwin 内核 peer UID。

同 UID 的进程仍在信任范围内；UID/产品名不证明对端是可信 App，也不抵抗同 UID 恶意进程。桥不 chmod、不创建/删除 socket、不修复目录或生产权限。实际 `BuildServer` 的原 listener、自愈机制和进程 umask 均未改动。默认 App 启动分支已接入受管 IPC 目录检查：仅对确认归属的默认数据根下 ipc 建立/收窄为 0700，拒绝链接、异主和未知类型，不递归修改子项，不改变自定义地址。同源隔离 App 启动仍待联调；本模块不提供手动 chmod 全数据目录的补救流程。

### 可选显式 API 凭据

`--api-token-file /ABSOLUTE/PRIVATE/token` 仅为用户显式准备的可信启动配置，不是 MCP 工具输入。桥不读取 Hive、数据库或其他应用配置，不自动发现/生成/导出 token，不登录 Web、不使用 Bearer 替代鉴权。未配置文件时不发送 token；App 开启 API token 或仅开启 WebAuth 时，缺少适用凭据会返回固定 `app_authentication_failed`。API token 正确时按原中间件规则通过，包括同时开启 WebAuth 的情况。

文件在启动时只读一次：规范绝对路径，逐级无符号链接、可信且非共享可写祖先（sticky 临时祖先例外），当前 UID 的私有父目录；常规文件、当前 UID、无硬链接，权限严格 0400/0600。采用逐级目录 FD + no-follow 打开并核对文件身份，拒绝 FIFO/目录/链接，最多读取 4096 字节；内容是非空可打印 ASCII 单 token，可有一个结尾 LF，拒绝空白、CRLF、控制符和多行。文件不可用或不安全即启动失败，只输出固定结束信息，不输出文件路径/内容。轮换文件不会改变运行中桥的凭据，新进程读取新值。

内存 token 只加入固定五个 GET 的 `X-Api-Token`，不跟随重定向，不使用 HTTP 代理，不将请求头、错误正文或凭据返回到 MCP。原 API token 是全局 API 凭据，**没有被降权为只读 token**；五读限制属于桥的工具与固定请求集合，不能改变持有该凭据的其他进程权限。凭据文件私有检查不因 socket 权限组合调整而放宽。

客户端配置只可包含显式文件路径，不内嵌 token。mcporter `list --json` 的外层 `transport` 会展示其本地启动参数；它属于客户端配置元数据，与 MCP `tools` schema、工具结果和错误输出分开核验。桥工具 schema/结果不含 token 文件路径或内容。

stdio 单行最多 64 KiB，tool arguments 最多 4 KiB，输出单次写入最多 256 KiB，慢读端写入最多等待 5 秒后结束桥。stdout 仅协议帧（显式 `--version` 除外），SDK 日志丢弃，stderr 只给固定结束信息。消息/返回上限不等于下载总字节配额：不拒绝正常未知 Content-Length/chunked 资源，不承诺内核下载硬限额。

退出只结束桥；查询取消仅取消自身 UDS 请求，绝不暂停/删除 App 任务。没有 App 时返回 unavailable；无自动启动/重试创建路径。

## 二进制部署与配置方案（仅设计，未安装）

1. 后续候选包放 `Kownload.app/Contents/Helpers/kownload-mcp`；构建 manifest 固定 App source SHA、桥 version/source SHA、read-api=1、MCP 支持版本和二进制摘要。桥与主 App 使用一致的已批准签名策略，先签 Helper 再封主包；不能把开发 ad-hoc 验证当作发行身份验证。
2. 用户稳定入口为新数据根 `bin/kownload-mcp` **普通文件**；客户端 command 固定为其绝对路径（不依赖 cwd、PATH、App 移动位置或版本目录）。用户配置只需一次明确授权录入，更新不改配置、不触碰其他服务器条目。
3. 后续获准部署时：从可信 App Helpers 复制到同目录临时文件，核摘要、manifest 配对、签名与 `--version`，保持当前 UID/0700，fsync 后同卷 rename 原子替换稳定入口。只更新将来的新进程，旧 stdio 会话继续使用旧 inode；不擅自杀客户端。候选失败保留旧入口，版本不匹配明确失败而不回退旧任务 API。部署器不在本增量实现。
4. 回退使用匹配 App/桥组合。只读桥没有自己的任务存储/迁移字段；若读 API 不兼容则拒绝。新版活动数据库的回退仍遵守 S0，不能由桥可启动证明 core 能安全执行旧数据。
5. 本模块受控测试在 /tmp 模拟 Helpers→稳定 bin、签名检查、原子替换两版真实二进制，比较原客户端配置字节并通过同一路径复跑五读。它不证明候选 App 打包、真实部署器或生产路径升级已经通过。

隔离 mcporter 配置模板（不写入全局 MCP 配置；正式路径须由用户确认）：

```json
{"imports":[],"mcpServers":{"kownload":{"command":"/ABSOLUTE/NEW-DATA-ROOT/bin/kownload-mcp","args":[]}}}
```

## 后续三写的合同，当前均未实现/启用

必须先复用 D3 持久 operationId+规范化请求指纹和回执，不得 fallback 旧 create；首版仅 HTTP/HTTPS。magnet/ED2K 新建另审网络与权限政策。网络目标策略必须覆盖重定向与最终地址，路径策略覆盖最终输出，不以桥只验首 URL 宣称完整 SSRF 防护。

新 MCP 任务默认禁用完成脚本、自动打开、外发 Webhook、自动执行后处理等高风险副作用；只有可信用户配置独立授权才可继承。策略必须绑定 Task 并持久化；模型参数和桥隐藏参数都不能替代 core enforcement。旧 core 忽略该字段的回退风险先进入 S0 清单；GUI 既有行为保持。当前模块不添加这些字段/策略或任何三写注册。

## 验证范围

`go test`/`go test -race` 使用隔离 UDS 和官方 SDK；包含五工具 schema、读路径、非法输入/未知写工具、私密字段/异常响应、限额、超时/取消、并发与 socket 边界。`tests/mcporter_smoke.py` 使用本机已有 mcporter CLI 和真实构建的桥，执行 tools/list、五读、写调用拒绝及两版本稳定路径模拟；不是自制客户端替代测试。

新增 `pkg/rest/mcp_read_integration_test.go` 使用实际 `BuildServer`、原认证中间件、私有合成 Bolt 数据与原 UDS listener；覆盖鉴权矩阵、TCP 不注册、Web 兜底顺序、S2 三条 operation 路由共存、405、五读及 socket 自愈，不手动 chmod socket。设置 `KOWNLOAD_MCP_TEST_BINARY=/ABSOLUTE/TEST-BRIDGE` 后运行 `TestMCPReadBuildServerCLI`，通过已有 mcporter 与真实 stdio 二进制验证五读、错误/缺失 token、文件启动拒绝、凭据路径稳定/轮换和自愈后五读。测试配置与客户端 HOME 均为临时目录；未设置该变量时此 CLI 集成用例显式跳过，不能将其计为已验证。

远端工作流新增本独立模块的 `go mod verify` 与 `go test -race ./...`；根模块 `./...` 本身不覆盖此模块。真实 mcporter 集成仍须显式设置测试二进制，不能从 CI 单测通过推断已完成 App 联调。

尚待：S3 安全统计/错误发布快照与有效总量、候选 App 五读完整联调、真实签名/包内集成、获准生产稳定路径部署与客户端配置迁移。当前不接触生产 App、日常浏览器、用户任务或凭据。
