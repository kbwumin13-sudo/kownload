# 按逻辑数据域受理批次 v1

批次目录与客户端 journal 只保存、查询引用。客户端预查 identity 后，服务端仍须在实际
R/M/cancel 事务中核对预期数据域，避免同一路径换库后写入另一库。此增量复用原
`task-batch.create/v1` namespace、operation ID、请求指纹和 R/M/Q 记录；不增加 Task
字段、bucket、回执库或下载流水线。当前为独立候选源码，生产 App39 未启用。

## 专用入口

沿原 API 鉴权注册以下路径，datasetId 必须为 32 位小写 hex：

```text
POST /api/v1/datasets/{datasetId}/task-batches/v1/operations
GET  /api/v1/datasets/{datasetId}/task-batches/v1/operations/{operationId}
POST /api/v1/datasets/{datasetId}/task-batches/v1/operations/{operationId}/cancel
```

POST body 沿原 `BatchOperationRequest` v1；dataset 是调用条件，不进入原 canonical
hash 或 Task。旧三入口及返回形状保留，新客户端不得回退旧 POST/cancel。
新路由绑定本次 server 的 Core，避免后来创建的 server 改变全局变量而串用数据域。

成功 `Result.data` 包装为
`{scopeVersion:1,datasetId,namespace:"task-batch.create/v1",operation:<原结果>}`。
同域 unknown GET 返回 HTTP200/code0，operation 为：

```json
{"version":1,"operationId":"example-op","state":"unknown","members":[],"taskIds":[],"createdCount":0,"failedCount":0,"pendingCount":0}
```

只有来自同域权威读取视图的结果可放入包装。HTTP503/code1000/
`operation_unavailable_query_same_id` 可以携带同包装的完整创建记录和未知的任务存在状态；
`taskPresent` 未读成就省略，不猜为 false，不截断成员或改计数。错域返回
HTTP409/code1000/`dataset_mismatch`/data:null。缺失、损坏或无法取得一致视图时
data:null，不返回另一域的 ID；错误正文不回显底层异常、URL、路径或凭据。

## Core 的读取和写入条件

`datasetReader.Get` 在一个真实 Bolt.View / Mem RLock 中解码自有副本，沿原
UseNumber 保留精度，回调中不开放 Put、不嵌套 storage.Get/view，不返回事务页切片。
离开回调后的 reader 拒绝使用。未知 Storage、缺失/损坏域均失败关闭。

R 的已有 operation 短路也先核域；R/M/cancel 的实际 UpdateAtomic 首部再次核域，
拒绝时该事务零 Task/op 写。M 之间若发现域变化，保留此前 R 的历史，不删除重造。
单 op 查询及错误回读将 dataset、operation/members、Task 存在和反向关联放同一
读取视图。GET 不执行 Q/resolve/create/restore；Q 只采用已核域的 owned 快照。
只有本次 M 确认新建且 ACK 成功的 createdNow 沿原策略取得首次 HTTP 执行资格；
旧 operation、重开、GET 或 M ACK 未知均不补发该资格。

## 客户端首次发送与恢复

从实际活动 API client 获取 identity；恢复型 journal 要求 persistent:true，并与已有
App route 三字段匹配。preflight unknown GET 仅验证协议与域，不产生首次发送资格。
只有新的用户意图、当前 UI 代次及本次 journal 写入真正确认，才可消费一次 POST。
重复、关闭、重开和恢复引用均 query-only，不以 unknown 当作未曾创建。

preflight 收到 404/HTML/无 scopeVersion 时禁止 POST。POST 一经调用后遇到同样响应，
属于无权威回执：保留 journal，仅在原 scope 查询，不删引用、不回退旧 API 或重造请求。
三个动作不跟随 3xx；未知版本、异域包装不采纳其中 ID。同域503保留原 HTTP/code，
可以展示可信 partial 历史但仍需查询。

routeScope 仍是 App 路由引用，datasetId 仍是逻辑域；两者都不是认证、物理 DB 证明
或内容未回退证明。明确复制/配对快照保留逻辑 ID，无法证明未知 operation 没有存在过。
旧 Core 不理解新路径，必须按不支持处理；旧端能读旧 API 不等于具备域受理能力。

## 验证范围

Core 相关行为由 scoped pipeline 回归覆盖；REST 接口夹具验证三种动作分派、精度及
unknown/partial/mismatch 包装。真实 BuildServer 的 Bolt/Mem HTTP 回归覆盖原鉴权、
三动作错域零写、查询不改 DB/不访问协议、一次唯一任务与文件 SHA256、同 ID 重放
不重启下载，以及旧单 op GET 形状保留。新 App GUI/journal 激活和生产升级另行验收。
