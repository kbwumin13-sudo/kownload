# kownload Native Messaging 契约（v1）

本契约是浏览器助手、独立 Native host、Flutter RPC 和 REST 的共同边界。帧格式依据 [Chrome Native Messaging 公开协议](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging)：UTF-8 JSON，前置 4 字节 native byte order 无符号长度；host 输出最多 1 MiB，输入最多 64 MiB；stdout 只写协议帧。每个输入帧对应一个输出帧，EOF 正常退出。

新清单名 `io.github.kbwumin13_sudo.kownload` 与兼容名 `com.gopeed.gopeed` 指向同一个独立 host。manifest 的名称不改变消息语义。浏览器来源须在 manifest/host 的固定扩展 ID 白名单内。预览只在临时目录生成清单，不注册正式浏览器或 URL scheme。

## 请求

```json
{"method":"create","operationId":"550e8400-e29b-41d4-a716-446655440000","meta":{"silent":false},"params":{"req":{"url":"https://example.org/file"}}}
```

| 字段 | 约束 |
| --- | --- |
| `method` | `ping`、`wakeup`、`create`、`forward`，区分大小写 |
| `operationId` | 顶层字符串；新客户端的 create/forward 必须提供。1–128 个 ASCII 字符，首字符字母或数字，随后允许字母、数字、`.`、`_`、`:`、`-`。旧客户端可省略，host 不生成 ID |
| `meta` | 可省略的对象；`silent` 可省略，默认 false；其余产品元数据透传给 Flutter |
| `params` | ping/wakeup 可省略；create/forward 必须是对象，保持既有产品参数结构 |

同一次用户提交及其查询/重试必须沿用 operationId，新的显式提交使用新 ID。ID 不从 URL、文件名或 Cookie 推导。去重与持久化由 Flutter/REST 权威实现；host 不做去重，不保存请求，不在超时或应答丢失后重发 POST。没有 ID 的旧请求不能承诺跨请求幂等。

| 方法 | 参数与行为 | 成功条件 |
| --- | --- | --- |
| ping | 不唤醒；只探测当前身份的 Flutter IPC | `data` 为是否可连接的布尔值；未运行返回 code 0、data false |
| wakeup | `meta.silent` 控制隐藏启动；先探测，再唤醒一次并有界等待 IPC | 可连接后 code 0；不创建任务；旧客户端可忽略 data |
| create | 参数是既有创建请求，POST Flutter `/create`；可先唤醒一次 | Flutter 明确返回 accepted 或 created；不能仅凭 HTTP 2xx 推断成功 |
| forward | `params:{"method":"POST","path":"/api/v1/tasks","data":{...}}`；POST Flutter `/forward`，保持原有 REST 参数和 data 结果形状 | Flutter 返回真实 REST HTTP 状态及业务 envelope；任务创建另需 created 与非空 taskIds |

forward 的 path 必须为本机 REST `/api/v1/` 下的路径，不接受 URL、scheme、`//` 或 `..` 段；method 为大写 HTTP 方法。host 不直接连接下载内核。调用端在 forward 前执行 wakeup；host 不替 forward 重发或隐式恢复任务。

## IPC 与业务应答

host 将 operationId 放入 `X-Kownload-Operation-Id` 请求头，**不塞进 params**；meta 以 JSON 放入 `X-Kownload-Host-Meta`，过渡期同时提供 `X-Gopeed-Host-Meta`。Flutter `/forward` 把 ID 传给 REST 同名请求头。应答顶层回显同一 `operationId`（旧请求可省略），不允许另造 ID。HTTP 客户端禁用重定向和 POST 重试。

Flutter `/create` 返回标准业务 envelope；`/forward` 保留 REST envelope 与 `data` 的既有形状，并把 REST 的 HTTP 状态保留到 Flutter HTTP 应答。业务 envelope 的 `code` 是整数（0 成功、非 0 失败），`msg` 是脱敏消息，`data` 是既有业务结果。新增 `operationId` 与 `submission` 都在 envelope 顶层：

```json
{"code":0,"msg":"","data":null,"operationId":"op-1","submission":{"state":"accepted","taskIds":[]}}
```

```json
{"code":0,"msg":"","data":"task-1","operationId":"op-2","submission":{"state":"created","taskIds":["task-1"]}}
```

- accepted：仅表示用户操作已受理、确认界面已打开；推荐 HTTP 202。taskIds 必须为空，不显示“已下载/已创建”。用户取消不等于失败重试。
- created：任务已实际持久化创建；HTTP 200/201，taskIds 必须为非空字符串数组。单任务旧 data 仍为任务 ID 字符串；批量仍为 ID 数组，不能为了新语义改变旧 data 的层级。
- create 必须有 submission；forward 的 POST `/api/v1/tasks`、`/api/v1/tasks/batch` 必须有 created（任务集合以实际 REST 路由为准）。非创建型 forward 结果无需 submission。
- 业务失败不带成功 submission。HTTP 500 即使 body 中 code 为 0 仍是失败；HTTP 200、code 1000 同样是失败。空 body、非法 JSON、缺少整数 code 均为无效应答，不能当成成功。

## Native 应答与兼容

```json
{"code":0,"data":{"code":0,"msg":"","data":"task-1","operationId":"op-2","submission":{"state":"created","taskIds":["task-1"]}},"message":"","operationId":"op-2","httpStatus":200,"businessCode":0,"submission":{"state":"created","taskIds":["task-1"]}}
```

外层 code 只描述桥接结果：0 成功、1 失败。`data` 保留原 Flutter 业务 envelope，不能再套一层；ping 的 data 仍为布尔值，wakeup 可为 null。`message` 是可展示的脱敏错误。`httpStatus` 为收到的 HTTP 状态；未发起/未收到 HTTP 应答时为 null。`businessCode` 为有效业务 code，无法解析或没有业务应答时为 null。`operationId` 回显请求值；`submission` 复制已验证的业务提交结果，非提交结果不带此字段。

因此旧助手的成功判断 `outer.code === 0` 及 `outer.data.code === 0`、原 data 解析保持有效；旧 create 客户端忽略新增数据仍可打开确认页。兼容不包含继续把 HTTP/业务错误伪装为成功。新助手必须检查 submission，不能把旧无结构成功应答当作 created。

```json
{"code":1,"data":{"code":1000,"msg":"请求未完成","data":null},"message":"请求未完成","operationId":"op-3","httpStatus":500,"businessCode":1000,"error":"http_error"}
```

稳定错误类别：`invalid_request`、`unknown_method`、`wakeup_failed`、`ipc_unavailable`、`request_failed`、`invalid_response`、`http_error`、`business_error`。超时/连接断开属于 request_failed；创建结果未知，应由客户端用同一 operationId 查询权威层状态，host 不自动补交。错误优先级为非 2xx HTTP → 业务非 0 → 无效 envelope/提交结果；无效 JSON 的非 2xx 应答仍保留真实 httpStatus。

错误应答与日志不能回显完整资源 URL、查询参数、路径、Cookie、Authorization、Token、原始请求或服务端堆栈。host 对失败的业务 data 置 null，msg/message/error 采用固定脱敏分类；保留业务 code、httpStatus、operationId 供诊断。成功 data 仍按业务协议交给原发起扩展，不作为日志。stderr 诊断只记录错误类别，不输出请求正文。

## 预算与最低回归

唤醒最多 10 秒，create HTTP 最多 10 秒，forward HTTP 最多 60 秒，与旧 host 的实际 forward 预算一致。外部取消应传给 IPC。等待 IPC 的探测可重试连接，已提交 POST 不能重试。超过 1 MiB 的应答返回固定 invalid_response，不把部分帧交给浏览器。

Flutter 直接 API 的 `/api/v1/netdisk/prepare` 等待 310 秒；当前网盘助手经 Native forward 只调用 browser/claim、browser/capture，prepare 不经过此链路，因此本契约不为它引入预算条目。直接 API 的既有预算保持。

当前既有助手的完整 forward 调用清单（worker A 依据助手全部文件及后端调用路径复核，接线前版本）：

| 调用 | 现有处理与证据 |
| --- | --- |
| POST `/api/v1/media/inspect` | `background.js` 的媒体检查；REST InspectMedia 与 `internal/protocol/media/inspect.go` 的 workCtx 均为 45 秒 |
| POST `/api/v1/tasks` | `background.js` 创建；CreateDirect → doCreate 先持久化任务，startTask 的 handler 在 goroutine 内执行，媒体 Resolve/yt-dlp 不在 POST 中同步等待；`pkg/download/downloader.go` 的相应函数 |
| GET `/api/v1/tasks`（重复 status 查询参数） | `background.js` 查询暂停/错误任务，本地状态读取 |
| PATCH `/api/v1/tasks/{id}` | `background.js` 更新任务来源，本地保存 |
| PUT `/api/v1/tasks/{id}/continue` | `background.js` 调度继续任务，解析在任务执行流程内 |
| GET `/api/v1/config` | `background.js` 本地配置读取 |
| POST `/api/v1/netdisk/browser/claim` | `netdisk.js`；Service.Claim 为本地认领 |
| POST `/api/v1/netdisk/browser/capture` | `netdisk.js`；CaptureContext 的 quark 账号探测 authCtx 为 8 秒，其余为本地验证 |

旧 host 的 `forward` 明确使用 `60*time.Second`，`create` 使用 `10*time.Second`（`git show 7ccfe41:cmd/host/main.go`）。未发现上述链路有合法超过 60 秒的既有同步预算。`tasks/batch` 不在既有助手直接调用清单；网盘 list（45 秒）和 prepare（300 秒，Flutter 等待 310 秒）走 Flutter 直接 API。本轮将任务提交迁到 Native create 后，旧 POST tasks 的 forward 能力继续兼容旧助手，预算保持。

最低夹具包括拆帧/多帧/截断/长度上限/坏 JSON、来源白名单、未知方法、IPC 不可达、唤醒失败、B02（**create 的 Flutter HTTP 500 必须外层 code 1/httpStatus 500**）、HTTP 200 业务错误、空 body、accepted/created、旧成功 data、同一 ID 多次提交的 host 不去重不重试，以及新旧清单名解析到同一可执行文件。Flutter/REST/助手的幂等与交互回归由对应模块测试覆盖。
