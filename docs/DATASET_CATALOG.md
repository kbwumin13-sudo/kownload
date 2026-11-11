# S5b 逻辑数据域与只读批次目录 v1

v1 已实现并通过隔离定向/race；尚未安装或操作生产数据。存储仅增加既有 `config` bucket 中的单条
`dataset_identity_v1`：`{"version":1,"datasetId":"<随机16字节的小写32hex>"}`。
REST 启动在 Downloader.Setup/上传恢复之前原子首次初始化；已有值只校验不重写。
坏值、未知版本或写失败保留原数据并停止本次启动，仅关闭本次尚未发布的 storage。
不改变 Storage 接口、Task、恢复/调度或全局自动开始偏好。

`GET /api/v1/dataset/identity` 沿原 auth 返回
`{version:1,datasetId,persistent}`，其中 MemStorage 的 persistent 为 false。
GET 不建 bucket、不生成 ID。逻辑 ID 与现有 route binding、operation 指纹/成员核对组合；
不是身份认证、物理数据库证明或“内容未回退”证明。同路径新库获新 ID；明确复制和配对
快照保持原 ID，即使内容已经回退。未知 operation 仍只查询/人工处理，不重造请求。

## 固定 namespace 的目录

`GET /api/v1/task-batches/operations?filter=all|pending&limit=1..100&cursor=...`，
默认 all/50，固定 `task-batch.create/v1`。响应沿原 Result envelope：

```text
{version:1,namespace,datasetId,persistent,consistency:"live-keyset",filter,
 items:[{keyHash,operationId?,state,requestHash?,counts?,error?}],
 nextCursor?,hasMore,scanned}
```

有效 entry 的 state 只为 pending/settled，counts 为 members/created/failed/pending；
settled 只说明创建记录没有待创建成员，不代表下载完成或记录可删除。
不检查 Task 是否仍存在；展开仍用原单 operation GET。
不返回 URL、header、options 或原始坏记录。坏/未知/错配记录以 keyHash 和固定 error
标识，state=unknown、无猜测计数；pending filter 也保留这些占位。

cursor 为有界、不透明的版本/dataset/filter/固定 namespace/原始最后 key；严格校验，
不能读取其他 bucket 或 namespace。每页仅一个只读存储快照，按原 key 字节顺序向前。
最多扫描 1000 条，预算耗尽即使 items 为空也用最后扫描 key 推进 nextCursor/hasMore。
无跨页全局快照：并发插入较小 key、删除或状态变化可能影响后续页；游标仅表示已扫描
位置。hasMore=false 只是本页读事务走到当时范围末尾，不证明任一 operation 不存在。
跨 dataset 或 filter 的 cursor 拒绝；同逻辑 ID 的旧快照无法由该 cursor 检出回退。

GET 不调用 Q、resolve、restore、create 或协议，不自动修复坏记录；存储不支持此能力时
明确拒绝，不降级为写入/全表解码后静默跳过。后续 UI journal 另行实现，不属于本合同。


## 校验与范围

cursor 上限65536字节，内部原始key上限为bbolt.MaxKeySize；严格校验版本、namespace、
filter、dataset和范围，不能用cursor选其他bucket。错误响应只含固定分类：400
invalid_catalog_request、409 dataset_cursor_mismatch、501 dataset_identity_storage_unsupported、
503 dataset_unavailable，不回显cursor、原记录或存储错误正文。有效条目counts仅为创建记录
摘要；unknown条目省略counts/requestHash，error区分record_key_invalid、record_unreadable、
record_invalid、record_unsupported、record_key_mismatch。

13个顶层定向用例及相同race通过，覆盖真实Bolt/Mem初始化与并发、重开/复制/同路径换库、
坏/未知值保留、熵源/只读库拒绝；旧Setup不清该key。GET前后实际数据库字节不变，目录保留
坏记录且不泄漏options/认证。包括1000条全settled后的空pending页推进、二进制坏key、
跨域/namespace/cursor拒绝及live插入删除；真实REST使用原auth，S2原单op读取不变、协议
请求和任务创建计数均为零。失败启动不进入Setup/logger、不替换或关闭已有运行实例。
新GET绑定本次server的Core，避免后一次BuildServer改全局指针使旧server标签串用。
这些结果不等于生产迁移/UI journal或快照内容未回退证明；Hive journal仍为独立planned状态。
