# 直接 HTTP 单文件校验完成 v1（planned，未激活）

能力为 `http-sha256-completion/v1`。本合同不属于 build40；transfer 模块通过不等于
Core/REST/UI 已闭合。首切片只接受新建普通单 URL HTTP(S)、一个成品与用户明确 SHA256；
无 checksum 沿原入口和原语义，多行不得广播同一摘要，rid/media/netdisk/多产物拒绝。

## 专用受理与同域操作

```text
GET  /api/v1/datasets/{datasetId}/verified-http/v1/capability
POST /api/v1/datasets/{datasetId}/verified-http/v1/operations
GET  /api/v1/datasets/{datasetId}/verified-http/v1/operations/{operationId}
POST /api/v1/datasets/{datasetId}/verified-http/v1/operations/{operationId}/cancel
```

沿原鉴权。datasetId 为32lowerhex，op沿现严格ID。capability只读、不创建目录/Task/账本；
成功 data 必须同时含 `scopeVersion:1,datasetId,capability:"http-sha256-completion/v1",
requestVersion:1,projectionVersion:1,supported:true,
methods:{create:"POST",query:"GET",cancel:"POST"}`。只有完整Core+适用发布adapter+原子存储
及同域实现齐备才可supported；compiled平台常量单独不够，亦不承诺目标FS/路径已获授权。
客户端恢复型journal仍要求本active API的persistent:true，不从可变设置猜测。

POST精确形状如下（版本/能力任一缺失或不匹配均在R/Resolve/IO前拒绝）：

```json
{"version":1,"requiredCapability":"http-sha256-completion/v1","operationId":"example-op","request":{"req":{"url":"https://example.invalid/file"},"opts":{}},"integrity":{"algorithm":"sha256","expectedSHA256":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}
```

receiver不把它降为旧create或可忽略的Extra。Core typed
`VerifiedHTTPIntegrityIntent{version,requiredCapability,algorithm,expectedSHA256}` 四字段
进入原R完整冻结意图/指纹；nil时旧请求canonical bytes/hash原样不变。completionId在R冻结，
同op旧请求/不同摘要冲突，不重用记录或补首次资格。S2永久namespace仍为
`task-batch.create/v1`，复用原op/R/M与journal，不造第二创建账本。R已有记录短路、真实M、
cancel及错误回读仍核same dataset；只有本次new M+已确认ACK沿现私有首次IO资格。

同域同op已存在时，先核相同原始意图/hash再重用原R冻结的有效false、目标和completionId；
不能先用后来全局true重新解析而丢旧回执、改目标或重造Task。首次R将server解析后的
执行选项保存在原operation/member/Task既有冻结选项中，raw永久指纹仍保持原始语义，
两者不可混淆。后续Start/恢复/C与成功出口消费已冻结的false，不重新继承后来全局值
而突然解压/加种。显式不同raw请求仍是同op冲突；不新增独立policy账本。

缺cap/method/version，404/HTML/未知包装或3xx，preflight零POST；3xx不跟随。调用POST后
同样响应属于ACK未知：保同route/dataset/ns/op引用，只GET、不回退旧POST/换op。
新动作查询默认走同verified前缀。旧journal不含kind，可用原scoped S2 GET安全读取
**创建历史**；created只说明Task曾建立，不能据此显示摘要通过或补发verified POST。

新GET/POST/cancel成功 data 包装为 `{scopeVersion:1,datasetId,
namespace:"task-batch.create/v1",capability:"http-sha256-completion/v1",operation:<原S2结果>}`。
同域unknown GET是HTTP200/code0，operation.state=unknown、空members/taskIds；
同域partial为HTTP503/code1000/operation_unavailable_query_same_id，只有权威同View取得的
原完整历史可带入，taskPresent未读成则省略。错域HTTP409/code1000/dataset_mismatch/
data:null；域/版本/一致视图未知也不返回另一域ID。创建回执从不代表verification。

### 最小响应样例

同域unknown GET，HTTP200：

```json
{"code":0,"msg":"","data":{"scopeVersion":1,"datasetId":"0123456789abcdef0123456789abcdef","namespace":"task-batch.create/v1","capability":"http-sha256-completion/v1","operation":{"version":1,"operationId":"example-op","state":"unknown","members":[],"taskIds":[],"createdCount":0,"failedCount":0,"pendingCount":0}}}
```

同域历史已created、Task存在/完成信息暂不可取，HTTP503。这里settled只指创建回执，
没有taskPresent/completionViews，不能据它显示SHA通过：

```json
{"code":1000,"msg":"operation_unavailable_query_same_id","data":{"scopeVersion":1,"datasetId":"0123456789abcdef0123456789abcdef","namespace":"task-batch.create/v1","capability":"http-sha256-completion/v1","operation":{"version":1,"operationId":"example-op","state":"settled","members":[{"index":0,"taskId":"example-task","state":"created"}],"taskIds":["example-task"],"createdCount":1,"failedCount":0,"pendingCount":0}}}
```

错域HTTP409，不采纳任何另一域ID：

```json
{"code":1000,"msg":"dataset_mismatch","data":null}
```

## 公开只读完成投影

沿现Task/组的读取周期，不新建poller。Task持久 `completion` 仅是required marker，不能
直接作UI phase。既有GET Task/list响应可选装饰 `completionView`；verified操作GET可以
附同形状 `completionViews` 列表。非enrolled Task省略；不得直接外发内部record。

```json
{"version":1,"scopeVersion":1,"datasetId":"0123456789abcdef0123456789abcdef","taskId":"example-task","completionId":"example-completion","available":true,"phase":"verified","recordedMatch":true,"errorCategory":null}
```

Task/ref/record/required、dataset和其投影必须由Core同一致快照核关联，再由REST薄包装；
不允许REST自行读DB拼当前内存Task，不允许为GET调用Q/restore/hash或写状态。UI核版本、
dataset/taskId和非空稳定completionId，换scope隔离旧投影。available=false时phase/
recordedMatch/completionId为null，errorCategory可为completion_unavailable；未知版/坏关联
保持不可用，不能猜false/通过。公开投影不持久回Task/Hive，不影响旧JSON/旧读端。

| phase白名单 | 展示含义 | recordedMatch |
| --- | --- | --- |
| prepared | 意图已保存，尚未具备校验前提 | null |
| waiting_verify | 同run写者已join，待校验 | null |
| verifying | 正在校验 | null |
| verified | 已记录匹配，待发布 | true |
| published_pending_commit | 已发布，待完成记录确认 | true |
| committed | 完成记录已确认并完成API状态线性化 | true |

recordedMatch只来自有效expected==actual的已确认record，不是此GET重新查文件。phase不能
把Task状态改done；Core未确认C/未线性化内存时不得公开committed为成功。错误只允许已确认
typed来源的completion_unavailable/completion_state_invalid/integrity_mismatch/
publish_conflict/cancelled/run_changed/io_error/unsupported_option；无可靠分类就null或
unavailable，不从原始error字符串猜测，不回显URL/headers/路径/设备inode/摘要进错误。

首v1只支持有效autoTorrent/autoExtract为false的单成品语义；null/缺省按既有继承规则解析，
显式true、继承true或无法冻结有效false须R前明确unsupported_option，不静默改成false、
不改全局配置。受理/恢复/成功出口消费同一冻结的有效选项；不能后来套新全局值扩副作用。
普通无SHA路径全部保持。Core若尚不能证明这一语义，应cap不支持，不先假装接受。

### 服务器文件时间（planned）

UseServerCtime保留原有效true/false语义，不永久静默降为false。首次R从实际HTTP有效config
捕获内部bool `verifiedUseServerCtime`，保存在原member.Options.Extra的server-frozen执行map，
位置同已冻结autoTorrent/autoExtract，不改caller raw或canonical/hash，不加Task字段或
第二policy ledger。caller raw同名key不构成授权。同R先核raw意图再复用原bool，不随随后
global切换；缺/坏须held，不从Task.Meta.Extra或当前config补。HTTP ParseOptExtra会丢未知key，
runtime须从同View的R owned map读取，并保持原UseNumber。

实际服务器Last-Modified已存为Task.Meta.Res.Files[0].Ctime（JSON名ctime），恢复复用该
持久快照，无需新增record字段；nil沿原无操作。锁定setft在macOS/Linux只改atime/mtime，
不直接设置creation；但本机APFS可随更早mtime自动下调birthtime，fd与legacy Chtimes真实
对照相同，不能承诺birthtime不变或因选项名把它称为独立设置macOS创建时间。writer join后/hash前按同run/
固定inode的fd adapter应用等价时间语义，禁止SHA模式downloadLoop对最终pathname的原
SetFileTime副作用或任何路径fallback。fd adapter/Run Fence/实际流水线仍待相应owner验证；
本段只冻结planned表达，不激活cap、不更改无SHA路径或build40产物。

## 三方持久状态与提交

Task.completion={version:1,completionId:string}；ID必须非空且等于原R冻结的completionId及
该Task对应record.CompletionID，不能仅凭bucket key或version认领错放record。
task_completion_v1按TaskID保存record；
task_completion_required_v1={version:1,capability:"http-sha256-completion/v1"}。Task/record/
required同一M事务确认后才有后续准备资格；见任一则三方缺坏/未知、completionId空或错配
均held，显式Continue亦然。此为未激活v1的planned精确定义，不增加v2/迁移或新ledger。
旧39/Core忽略新状态不安全，禁止裸活动库回退；配对快照也不证明输出/自动续跑安全。

record v1 JSON为version/completionId/phase/directoryIdentity{device,inode}/
stagingIdentity{device,inode}/stagingName/finalName/size/expectedSHA256/actualSHA256?。
prepared可空名、size=-1、anchor未绑定；不是执行许可。Resolve按原命名政策取得合法单basename
和固定根后先持久ACK；create空stage→pin真实目录/文件inode并ACK→首byte。无绑定期间崩溃
保守held。写者join后才固定size与waiting_verify，Complete拒prepared/负size/空名/无anchor。

verified保存确认后才同卷anchored no-replace发布，保stage锚点；C原子done+record+required，
只有已确认fresh winner才向原成功出口发一次私有许可。unknown ACK只同ID查询，恢复已C不补
脚本/Webhook等动作；没有FS+DB跨事务原子或exactly-once外部副作用承诺。未C重开须重hash；
同inode已final可续阶段，外来同hash不能认领。hash/Wait/join锁外，短发布/保存由同Task票据
Fence准入；HTTP结束应释放网络槽，但保同run token直到C/失败，不能新造generation。

分工：15拥有model字段slot/全部现有共享文件、Task/typedR/HTTP/Fence与API线性化；12拥有
transfer模块及新completion_state.go三方存储内核/测试；11拥有新published_read.go纯值locked
publish/clear/access及测试。12/11不写model/watch/HTTP/batch。14新REST薄文件/server注册及
既有GetTask/GetTasks仅响应装饰，16纯transient DTO/原轮询UI。三方借接点各自
明确后实施；本文件与S0行是planned契约，不是已部署能力。

## REST 首增量的验证边界

专用receiver和公开projection formatter可先在独立候选验证；真实BuildServer已薄接所绑定
Core的四个typed ForDataset方法，当前Core只读核dataset并返回supported:false，
create/query/cancel均明确unsupported，不回退旧
API、不会把无校验Task当校验任务。正例注入仅位于测试。真实Core typedR/三方guard/Fence/C/
published snapshot未接通时，不启用能力，也不装饰普通Task/list的phase；该部分须待同源
Core×API×UI/MCP交集验证。此增量不修改build40已交包。

adapter将outer version/requiredCapability填入typed Integrity，不塞Extra或重算raw/options；
未知Core能力版本/名称fail closed。nullable projection按字段保留版本/关联，空string为null，
RecordedMatch保nil且不共享指针；公开formatter再核shape，不给坏view补成可信数据。
typed invalid_verified_http_request与unsupported_option均HTTP400/code1000，完整能力未接为
501/verified_http_unsupported；partial/错域沿前述约定，不透传module error。
