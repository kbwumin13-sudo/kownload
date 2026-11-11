# 校验完成通知的实时载体 v1（本地候选，待成套验收）

仅用于解决通知服务以状态轮询补发历史成功的旁路；正常build40/41不含此能力。本地完整
Core/REST候选已接线，待审/未发布或安装。普通任务仍沿原
通知语义。Task/ref/完成record和completionView不加Fresh、时间、nonce或事件持久字段。
形状合法不等于具备fresh许可，只有Core真实C ACK及API done线性化后的唯一winner能发布。

## Core与接收端

`GET /api/v1/datasets/{D}/verified-http/v1/completion-events` 沿actual API原router/鉴权，
绑定本server的Core。同域校验由Core的
`SubscribeVerifiedHTTPDone(ctx,D)(<-chan VerifiedHTTPDoneEvent,release func(),error)`负责。
完整Core接点缺失时返回unsupported JSON，不打开SSE头，不因broker可用而开放capability。
旧单Listener不被替换。查询、恢复、重放和unknown C ACK均不得调用publish。

REST adapter先调用同Core的`VerifiedHTTPCapabilityForDataset(D)`，严格核version=1、
capability=http-sha256-completion/v1及supported=true；否则501 JSON且零订阅。
通过后才以原请求ctx/原D调用真实Subscribe，后者再次守域；订阅错误仍在SSE头之前处理。
本地完整候选已按完整runner条件报告能力；缺接点/未知能力仍保持unsupported。
真实BuildServer/UDS已通过Core fresh C ACK→API线性化→四字段事件及Bolt重开零补发的
同源plain/race测试；注入事件HTTP测试仍只证明传输。Dart消费者/系统通知/实际App待成套
验证，不修改正常build40/41或5e6实际预览证据。

事件只有四字段：`{version:1,datasetId:<32lowerhex>,taskId:<ID>,completionId:<R冻结CID>}`。
新事件的Task/CID为非空有效UTF-8，各不超过128 UTF-8字节；这只是传输边界，不改Task格式。
没有fresh bool、SHA、路径、凭据、时间或eventId。

14纯RAM接点固定为`newVerifiedHTTPDoneBroker()`，其`subscribe(ctx,D)`返回上述channel/
release/error，`publish(event)`无返回许可，`close()`幂等。15独占Downloader字段、创建/
关闭、订阅wrapper及同域校验、fresh C唯一分支的锁外publish。纯broker不检查或创造C证明。

每Core最多4订阅，每订阅16条缓冲；无订阅丢弃，publish非阻塞，满缓冲只移除/清空并关闭
该连接。没有历史缓冲或重订阅补发。release、ctx取消和Core.Close清理同一订阅一次，
不阻下载，不持主/Task/owner锁写网络。客户端已收的数据无法撤回，仍必须核当前API epoch。

## SSE线格式与错误

HTTP200、`Content-Type: text/event-stream`、`Cache-Control: no-store`。仅发以下UTF-8帧，
换行为LF；JSON单行，网络chunk可任意分割，整帧（含末尾空行）最多4096字节：

```text
event: accepted-done
data: {"version":1,"datasetId":"0123456789abcdef0123456789abcdef","taskId":"example-task","completionId":"example-completion"}

```

无id/retry/comment/heartbeat/hello/历史drain。任何query参数（含since/cursor）或任何
Last-Event-ID头（包括空值）都在订阅前拒绝，不忽略恢复输入。不能发多行data或其它事件名。
该GET无正文；Content-Length非零/未知或chunked正文在订阅前400并关闭连接，不等待drain，
避免HTTP/1的首个Flush因未读请求正文而超出局部写期限。

| HTTP/结束 | 含义与客户端行为 |
| --- | --- |
| 200正确SSE | 只消费当前连接之后的实时事件，EOF不采纳未闭合帧 |
| 401/403 | 原鉴权拒绝，本API epoch停止 |
| 404/501或JSON unsupported | 不支持，本epoch停止，零legacy fallback |
| 400坏域/恢复输入，409 dataset_mismatch，错误版本/媒体类型/帧 | 协议/scope错误，本epoch停止 |
| 503 completion_events_capacity | 4订阅已满；仅原tick未来可再订阅，不drain/重放 |
| 503 completion_events_unavailable | broker已关闭/当前服务不可用；原tick可有界重订阅 |
| 200后溢出/关闭/网络写失败 | 直接EOF/断流，无错误事件，无重放；原tick只订阅以后新事件 |

标准JSON错误仍code1000/data:null，无raw error。每次Write/Flush有5秒局部deadline及
ctx取消打断；成功Flush后清除deadline以允许无heartbeat空闲，退出前界定最终写入，
不改全局HTTP超时。单连接至多一个有界取消watcher，必须join后再返回，禁止无界写goroutine。

## 消费与验证范围

16单owner新client/model及notification_service，原2秒tick负责legacy状态/失败通知及
失联后的重订阅；最多一个inflight/connection，每tick最多一次尝试，无新timer。enrolled
任务不能因done/phase/match触成功通知。实时事件核version、当前active API epoch、D/Task/
CID及committed关联；本stream RAM去重有界，满可断流而不重放。API换代/禁通知/dispose
立即断流并丢晚到；401/403/unsupported/坏协议本epoch停。缺事件只少通知，无exactly-once承诺。

14拥有两个新broker文件、REST SSE文件/测试和薄route；15拥有现有Core接线；16拥有消费。
pure broker与test backend成功仅证明局部，仍须真实Core fresh C→API→UI同源验证。恢复/
query/unknownACK必须0事件；普通Task行为、MCP只读和首包产物保持既有边界。
