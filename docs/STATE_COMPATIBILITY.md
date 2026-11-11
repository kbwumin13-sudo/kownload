# 状态兼容与离线回退

S0 基线：现用 build39 / `5babaf7b2a9e90a7a58e9ff690228c2664f430ea`；开发基线
`446b474cdffd70c887c300fb01ea3b4e6fab5277`。本清单区分数据读取、执行与自动续跑。
JSON 解码成功、数据库格式没变、App 能启动，都不能证明旧版可以执行新版任务。

## 状态清单

每次增加持久字段、bucket 或执行阶段，先补齐本表，再合入该状态的写入端。
`未知` 的执行/自动续跑能力按拒绝处理；不能通过删除字段或重造任务修复未知状态。

| 状态与版本 | 读取与写入者 | 旧版可读 | 可执行 | 可安全自动续跑 | 回退要求与证据 |
| --- | --- | --- | --- | --- | --- |
| 原 Task 六种 status、task/save/protocol_state/config/extension/extension_storage、Hive database | 39 及开发基线 | 原字段按旧契约；具体更早版本仍需匹配 | 当前支持协议沿原行为 | 原用户偏好、netdisk 重新授权与协议恢复约束仍生效 | 配套 App/数据/偏好快照；输出文件另核对，不能只比数据库字节 |
| `Task.handoff.version=1`，`prepared/committed/aborted` | 39；`browser_handoff.go` | 不识别 Handoff 的旧 core 可能忽略字段；不等于兼容 | 39 只执行 committed；prepared/aborted/未知版本保持 held | 只有受支持 committed 才可能继续；旧 core 未证明安全 | 禁止活动库裸回灌旧 core；39 故障/重启历史证据见身份审查报告，S0 不冒称重测 |
| 助手 profile `nativeBridgeBindingV1` 与 capture pending 的 `bridgeBinding.version=1`：hostName/routeScope | S1 本地接线，首写 Native 请求前持久化 | 旧助手可能忽略绑定或仍选择旧 host，不能判安全 | 实际宿主转发前校验 expected scope；不匹配零转发；遗留未绑定 pending 仅保持未知 | worker 重启只原 binding 同 op 查询；无 ACK 不换 host 重放；存储失败零写请求 | App 数据快照不等浏览器 pending 已回退；先收口或保留未知，不能清空后重造。本地写前失败/不同宿主进程/worker 重启有 fixture；日常浏览器与配套安装仍待验 |
| 媒体既有 session `operations[mark]` v1：operationId/bridgeBinding | S1 本地接线，首次 create 前整项保存 | 旧助手可能把对象误当旧字符串，不能安全恢复 | 当前端只同绑定、同 op 查询 submission；旧无绑定字符串/未知版本保留未知 | ACK 未知不重放 create；跨 worker 先查询；profile 绑定不自动清除或换 host | profile 保存不代表 job 保存成功；两者完成前零 wake/prepare/create。旧助手回退前先收口或保留未知，App 快照不覆盖浏览器 journal |
| `Task.creationOperation` v1，namespace=`task-batch.create/v1`，operationId/index | S2 本地 `3c69231`；原 Task 六状态不变 | 39 可能忽略或保存丢字段，不等可执行 | 新端须与 manifest/member/逆索引一致；无 ref 且无逆索引的旧 Task 兼容 | 受启动 policy 和原任务守卫；未识别/不一致拒绝 | 活动库裸回退不支持；配对快照。以下三个 S2 行的实证来自其独立 plain/race/子进程断点交付，本分支只登记合同，不冒称已合入 |
| bucket `task_operations_v1`，record.version=1 | S2 同提交；固定 namespace、稳定 members，pending 无 Task | 旧 core 不理解持久 operation | pending 只保留意图清单，不启动、不自动重造；created 保留删除历史 | 不 TTL 淘汰；重放不得自动继续暂停任务；调用者须显式提供相同完整意图 | 旧版不维护操作幂等，不能裸回灌；凭据/意图变化不得冒充同 op 或重建 created 前缀 |
| bucket `operation_tasks_v1`，TaskID→ref | S2 同提交；与 Task/member 同一 M 事务持久写入 | 旧 core 不维护逆索引 | 启动核对；丢 ref/错配/未来版本隔离，不能把有逆索引的任务当 legacy | 索引/任务/manifest 不一致拒绝，不靠自动修复重造 | 新 reader 丢字段/未知 manifest 和索引已有隔离测试；不证明旧 core 执行安全，默认配对快照 |
| `config` bucket 单 key `dataset_identity_v1`，`{version:1,datasetId:32位小写hex}`（S5b v1，隔离验证通过） | REST 启动在 Downloader.Setup 前单事务首次生成；GET 不生成 | 旧 core 忽略该附加 key，不能回答数据域协议；不改变原 Task 可执行范围 | 逻辑 ID 不授予任务执行或认证能力；坏/未知记录保留并拒绝新端启动 | ID 不证明数据未回退，原 Task/S0/S2 守卫仍必需 | 新库首次启用生成一次，重启保留；复制/配对快照保逻辑 ID，不证明内容新鲜。缺 ID 的旧快照再次启用会获新 ID，旧 journal 不得换域重造请求；真实 Bolt/Mem、重开/复制/换库、坏值/写失败、REST 初始化与只读零写定向/race 已验，生产尚未启用 |
| Hive 单 key `task_batch_journal_v1`，JSON String `{journalVersion:1,refs:[{route:{product,identityProtocol,routeScope},datasetId,namespace,operationId,state:query_required}]}`（S5b 手动 HTTP(S) 成套候选，未生产启用） | 16 d4b2bba journal/store 经成套手动多行 HTTP(S) 与只读组入口接线；绑定实际 boot API 的不可变 snapshot，正式 App 隔离验收仍待；route 固定 kownload/1/既有64hex，dataset 必须来自 persistent:true 的 Core 身份 | 旧 App 可忽略此 Hive 键，不等于旧 core 可以安全执行 S2 状态；仍沿 S2 的不兼容执行边界 | 只有新用户意图、当前 UI 代次、仍匹配的活动 API/ref/scope 与新 ref 真实 put ACK 可消费一次首发；preflight unknown 只验能力；重开/重复永远 queryOnly，错误或超时不可发送；不保存 req/opts/auth/hash/回执副本 | 未决不淘汰，无自动删除/GC；坏/未知 journal 整体隔离且保原值，不换 route/dataset 认领；Mem 域拒恢复型预写 | 多 store 同 Box 串行 fence 保持至真实 put settle，超时后迟到成功不升级原返回或覆盖后续 refs；引用不是认证或数据未回退证明，配对快照不允许重造未知 op |
| 共享恢复 budget / execution generation | S3 计划，尚未写入 | 未知 | 未知，拒绝；迟到执行不得跨代生效 | 未知，拒绝；换连接/URL/队列不能清零预算 | 先冻结暂停/重试/重启政策及故障用例，补版本与旧端兼容证据 |
| 校验/发布 SHA phase 与完成门禁 | S4 计划，尚未写入 | 旧 status 能读也可能跳过校验 | 未知版本不得执行或写 done | 未决/校验失败/发布未持久不得当完成续跑 | 旧 core 无完整完成门禁，禁止裸活动库回退；校验/发布/副作用故障测试必需 |
| 路径 provenance / 规则版本 / 已冻结目标 | 后续计划，尚未写入 | 旧 core 可能忽略来源/重新分流 | 只用持久已冻结目标；未知来源政策不重算路径 | 未知拒绝，首次写入前先持久化 | App/数据快照不能回滚文件内容；须核真实输出位置/字节，不能删除新版文件来凑一致 |
| MCP 任务持久副作用策略 | S7 写端计划；当前五读桥不新增任务状态 | 旧 core 可能忽略禁用脚本/自动打开/Webhook 等限制 | 未知策略版本拒绝；模型参数不能授予权限 | 默认高风险副作用为零，显式可信配置另授权 | 旧端忽略策略会扩大副作用，因此不支持裸活动库回退；写端启用前补行为证据 |
| MCP 稳定 bin 的安装 xattr v1：`io.github.kbwumin13-sudo.kownload.mcp-install`，JSON 白名单 `recordVersion/product/sourceCommit/version/readApiVersion/architecture/sha256` | CLI 独立候选已在私有 HOME 首装/升级；recordVersion=1、product=kownload-mcp、40小写hex源码、release+同SHA、readApiVersion=1、arm64、64小写hex摘要；不属于 Task/数据库格式，不新增 receipt 库。旧离线副本已实证丢标记，本增量补固定标记保全 | 旧桥忽略标记；旧 CLI 不支持 install-helper，不得据此自动认领/覆盖 | App/桥需配对；标记仅同 UID 信任范围内的归属/冲突记录，不是安全认证 | 不涉及下载自动续跑；未知字段/版本、缺失或摘要不符拒绝升级，不自动认领 | CLI 同私有 staging inode 发布前写入并读回；不支持/失败保旧入口。离线保全仅对 data/bin/kownload-mcp 普通单链接文件验证 mode700、冻结 v1/七字段/≤2048B/二进制 SHA256 绑定；固定 marker 原字节复制后复读源与副本，不保全则不发布 restore-plan/pairVerified。目录摘要仍 content-only，标记单独核验，不推广为其他 attrs 或 quarantine 的备份证明 |

## 回退单位

默认回退单位是**同一停写时点的 App、数据目录、应用偏好快照**。
快照应记录版本/构建与源码归属、各输入的字节摘要、原数据根和偏好域；未知、缺件或摘要
不符不能执行恢复。记录版本名或手填 `compatible=true` 不能替代匹配证据。

`scripts/rollback-identity.py --apply` 不应再把新版活动数据库默认复制给旧 core。
配对校验必须在任何复制、建目录、改名或浏览器清单恢复之前结束。恢复工具不启动或安装
App，不向运行中的 UserDefaults 导入偏好，不修改用户 `autoStartTasks`。

快照保存的是当时的任务引用，输出文件后来可能已经增加、移动、被替换或完成。
恢复数据/偏好不代表输出文件已经恢复，更不代表可以自动续跑。同一旧任务不得重造，
新版活动根及输出保留；持久幂等关联与用户明确删除的事实不能通过回退自动抹掉再重建。

## 启动与执行许可

App 独立启动供查看数据，与导入任务获准自动续跑是两个决定。导入来源/执行政策未知时，
可以完成只读导入并打开 App，但不得因原 `autoStartTasks=true` 自动执行这些任务。
用户全局偏好不被改写，已识别的非导入任务仍沿原契约。

S0 的局部启动政策合同已由 13 核准，14/15 分别接线：临时 `startupTaskPolicy` 在 Flutter
启动自动继续及 Core Setup 的上传恢复等隐式执行之前生效；不能只在界面跳过一次批量
继续。默认/显式继续的边界与状态版本守卫要由 core 回归证明。

## 验证记录

S0 验证仅在临时 fixture 执行，不安装或运行生产 App，也不启动旧任务：

- `python3 scripts/test-rollback-identity.py`：旧裸 apply 的拒绝用例在旧码失败，
  新工具验证缺件/版本/摘要/路径冲突与配对副本，取快照而非活动库，偏好仅离线保存；
  系统 Python 和内部 framework 相对链接也覆盖。
- MCP 受控旧负例：二进制和 mode700 保留，但原工具 exit0/pairVerified 后副本丢安装
  xattr。修复只显式复制固定产品 marker；真实 macOS roundtrip、缺失/未知/坏/超长拒绝、
  写入/复读失败及源变化不发布计划均由 fixture 覆盖。属性读写绑定到逐级无链接打开并
  持有的已验证 inode；末级文件/父目录换成链接的回归验证外部 marker 与源字节不变，
  不把事后拒绝计划当成阻止外部写入的替代。旧负例不改写为“从未支持”，也不
  把丢标记称为 Task 损坏；生产恢复和旧 App 标准根升级仍未验。
- `go test ./internal/identitymigration` 与相关 `-race`：新空根 normal、导入 review、
  旧 marker 保持 review、未知政策写前拒绝，旧数据和目标 marker 不被重写为兼容。
- Flutter 定向测试验证导入后的政策读取、旧 Hive 字节保留、暂态 StartConfig 序列化、
  启动 resume callback 不调用、原自动开始选择不变、提示可见且关闭不解禁/不阻止新操作。
- Core `TestStartupPolicy*` 及 REST 定向测试由 15 交付：Setup 上传、缺失文件自动
  清理、无明确 IDs 的 continue-all 有屏障，显式创建/继续仍受 held；未知政策开库前拒绝，
  base Config 不保存启动政策。对应 Go 接线提交为 `772e67a057b0e8149b8ce75484b93f14bf356a8c`。

这些测试不证明真实旧版回退、生产 GUI 提示或输出文件仍匹配；后续新状态仍须各自实现
版本支持守卫与故障测试，不能把 S0 的启动政策误当作通用未知字段执行证明。
