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
| `Task.recovery` ref，`version=1`（本地候选已实现，未发布/安装） | S3 独立候选 `e045641`；直接 HTTP enroll/IO 已接同事务许可 | 39/旧 core 可能忽略或保存丢 ref，不能当普通任务执行 | 新端必须核对 ref、ledger、required 哨兵三方；缺失/未知/错配时连显式 Continue 也拒绝 | 只执行受支持的 episode/generation/sourceRevision；迟到结果不能跨代生效，仍受 S0 启动政策 | 与以下两行同事务 enroll；配对 App/数据/偏好快照，禁止裸活动库回灌旧 core |
| bucket `task_recovery_v1`，record.version=1（本地候选已实现，未发布/安装） | S3 同候选，首能力仅 `http-transport-recovery/v1` 可冻结的 direct/manual/static/env 上下文 | 旧 core 不理解共享预算或 generation，不可据此执行/自动续跑 | 保存 episode/generation、sourceRevision/sourceHash、必需 executionDefaultsHash、initialAttempt、计数与有界 actions；缺坏默认摘要 held，不能补造 | 默认上下文合法变化只同 ledger CAS 提高 sourceRevision/generation，保 episode、issued 证据、余额/actions/notBefore；只读不 enroll、删除不重造 | 三方必须同事务；当前仅 transport action，总16且 transport6；refreshUsed/deferralUsed 仅0、notBefore 仅空值受支持，其他值拒绝而不清空 |
| bucket `task_recovery_required_v1`，`{version:1, capability:http-transport-recovery/v1}`（本地候选已实现，未发布/安装） | S3 同候选 required 哨兵；即使旧 writer 丢 Task ref 仍由新端识别 | 旧 core/App39 忽略新 bucket，不能安全执行或自动续跑 | 新端见哨兵即要求完整受支持三方；哨兵缺失而 ref/ledger 存在同样拒绝 | 不因 ref 被旧 writer 丢失降级 legacy；不自动修复、重新 enroll 或重造已删 Task | 备份须保三方状态；配对快照回退不证明输出字节或自动续跑安全，不把旧 JSON 可读当执行兼容 |
| 显式 error Retry 三方 v2 与 `lastExplicitRetry.version=1`（本地候选，未审查/合入/安装） | 本地 writer 在首次真正用户 Retry 同事务把 Task.recovery、task_recovery_v1 ledger、task_recovery_required_v1 三个既有 version 一起1→2；bucket/cap名称不变，不增 Task 字段 | 39忽略新状态仍不安全；冻结26/cdd76b11 v1 reader真实打开v2 Bolt后，Setup无fetcher且显式Continue held；既有输出保留 | 新 reader 仅接受完整v1/noLast或完整v2/Last；混版、缺坏Last、未知版本 held零IO，GET不升版 | Last是历史回执，不是首IO许可；仅新action已确认fresh ACK给本进程一次retryAuthorization，unknown/replay/restart不恢复 | 配对快照及禁止裸活动库回退；只明确用户新episode重置本轮计数/actions，Continue/Pause/Resume从不暗reset，保持同TaskID/path/checkpoint/source/D/notBefore |
| D4 净新覆盖三方 v3（本地成套已接线，待审/未安装） | 原 Task.recovery、task_recovery_v1 ledger、task_recovery_required_v1 三个 version 同事务升3；原bucket/cap不变，ledger必需 coverage.version=1；不增Task字段/第二ledger/迁移 | normal40/41/42均unsupported v3（政策）；40无held保证，41仅静态拒版，42仅按运行树字节关联下文ceff实际reader；不等于42 App冷启；39仍unsafe | 新reader仅接受完整v1无Last/Coverage、v2必Last无Coverage且Last.ToEpisode=current、v3必合法Coverage且Last可历史；混版/缺坏/未知held零IO | 首装只消费本进程普通新Task/本次new M已确认ACK、首byte前RAM一次资格；历史/unknown/replay/reload不补；checkpoint和完整record独立CAS，结算不直接授IO | 配对快照且禁止裸活动库回退；slot482本身仅schema/版本测试；本地runtime候选及验证分层见下节，normal42不含本候选 |
| HTTP共享恢复三方 v4（planned，未落库/未激活） | 原Task.recovery与task_recovery_v1、task_recovery_required_v1同事务version4，原key/字段不扩；required cap精确二选一 | af2版本谓词仅静态接受v1/v2/v3、拒4；真实新writer Bolt与逐入口拒执行待验，不能称已有实测 | http-shared-recovery/v1必须无Coverage；http-shared-coverage-recovery/v1必须合法Coverage v1；混版/未知cap/错配/缺坏held零IO | 仅已有完整v1-v3在安全非活动Fence与同D/token/fullRecord/required/current-checkpoint CAS升级；legacy不enroll，GET/Setup不迁移，升级无免费资格 | 总16/transport6/refresh3/deferral10守恒；绝对NotBefore硬门；unknown/reload/query不补RAM；保B/seq/Until/历史receipt，详下节planned约束 |
| `Task.completion={version:1,completionId:string}`（S4本地候选已接线，待审/未安装） | 普通single directHTTP(S)/显式SHA256专用receiver；ID等原R冻结值及record.CompletionID，无checksum原流程不变 | 旧39/Core可能忽略ref，不安全执行 | 新端见ref/record/required任一即核三方；缺坏/未知或CID空/错配held，bucket key不能代替CID绑定；Resolve/Start/staging前须确认受理 | 显式/批量/自动Continue不能绕过；prepared不授执行，删除不重造 | 与以下两行同M事务；精确v1，不增v2/迁移；旧Core不能消费新guard，禁止裸活动库回退，配对快照不证明输出/续跑安全 |
| `task_completion_v1`，record.version=1（S4本地候选已接线，待审/未安装） | 同TaskID，R冻结稳定completionId/typed integrity完整意图；Core/REST真实链已验，UI/App待成套验收 | 旧Core不理解校验/安全发布阶段 | prepared可空basename/size=-1/无anchor；首byte前完整name/root与真实inode保存ACK，join后才waiting_verify | 未C重开重hash；外来同hash不认领，未知阶段拒绝；不自动整理stage | phase仅prepared/waiting_verify/verifying/verified/published_pending_commit/committed；expected/actual与归属锚点不构成跨FS+DB原子保证 |
| `task_completion_required_v1={version:1,capability:http-sha256-completion/v1}`（S4本地候选已接线，待审/未安装） | 同M三方required；同C原子done+record+required，fresh ACK后API线性化与一次私有成功许可 | 旧39/Core忽略bucket仍unsafe；不能靠文档声称已拒 | 三方缺失/混坏/未知版，旧writer丢ref也不得降legacy；真实IO需同run/Fence | unknown ACK只读原ID，不补许可；恢复C仅重建状态不补脚本/Webhook | 备份保三方及实际输出另核；受理与公开DTO见VERIFIED_HTTP_COMPLETION.md，正常build40/41未含 |
| S4 `R.member.Options.Extra.verifiedUseServerCtime` bool（本地候选已接线，待审/未安装） | 首次R按实际HTTP有效config捕获server-frozen执行选项，位置同原冻结false选项，不改caller raw/canonical/hash、不增Task字段/第二ledger | 旧Core可能忽略或丢内部key；不能据此承诺安全执行，仍配对App/数据回退 | 同R先核raw意图再复用已存bool；仅从同view R的owned执行map读，缺失/非bool held；caller同key、Task.Meta.Extra或新global都不能补授权 | 保UseNumber；HTTP typed ParseOptExtra丢未知key不改变权威R；无SHA旧流程不动 | 时间值取已持久Task.Meta.Res.Files[0].Ctime；nil沿原无操作。macOS/Linux沿既有atime/mtime API语义，APFS可能随更早mtime下调birthtime，不承诺birthtime不变或独立设置creation；join后经固定fd应用，不触最终pathname，不新增record时间字段 |
| 路径 provenance / 规则版本 / 已冻结目标 | 后续计划，尚未写入 | 旧 core 可能忽略来源/重新分流 | 只用持久已冻结目标；未知来源政策不重算路径 | 未知拒绝，首次写入前先持久化 | App/数据快照不能回滚文件内容；须核真实输出位置/字节，不能删除新版文件来凑一致 |
| MCP 任务持久副作用策略 | S7 写端计划；当前五读桥不新增任务状态 | 旧 core 可能忽略禁用脚本/自动打开/Webhook 等限制 | 未知策略版本拒绝；模型参数不能授予权限 | 默认高风险副作用为零，显式可信配置另授权 | 旧端忽略策略会扩大副作用，因此不支持裸活动库回退；写端启用前补行为证据 |
| MCP 稳定 bin 的安装 xattr v1：`io.github.kbwumin13-sudo.kownload.mcp-install`，JSON 白名单 `recordVersion/product/sourceCommit/version/readApiVersion/architecture/sha256` | CLI 独立候选已在私有 HOME 首装/升级；recordVersion=1、product=kownload-mcp、40小写hex源码、release+同SHA、readApiVersion=1、arm64、64小写hex摘要；不属于 Task/数据库格式，不新增 receipt 库。旧离线副本已实证丢标记，本增量补固定标记保全 | 旧桥忽略标记；旧 CLI 不支持 install-helper，不得据此自动认领/覆盖 | App/桥需配对；标记仅同 UID 信任范围内的归属/冲突记录，不是安全认证 | 不涉及下载自动续跑；未知字段/版本、缺失或摘要不符拒绝升级，不自动认领 | CLI 同私有 staging inode 发布前写入并读回；不支持/失败保旧入口。离线保全仅对 data/bin/kownload-mcp 普通单链接文件验证 mode700、冻结 v1/七字段/≤2048B/二进制 SHA256 绑定；固定 marker 原字节复制后复读源与副本，不保全则不发布 restore-plan/pairVerified。目录摘要仍 content-only，标记单独核验，不推广为其他 attrs 或 quarantine 的备份证明 |

S4上述三方状态及时间选项只登记本地完整候选，正常build40/41不含，尚未发布/安装。
当前force=true选择中任一Task含completion ref/record/required时整次unsupported_option，
先于修改成员、队列、记录或文件；force=false可仅删Task并保留输出。owned产物安全unlink
仍是D5缺口。Core/REST真实fresh C、paired读取与Bolt重开零事件已有定向plain/race证据，
不替代UI/系统通知/实际App及其它产物验收。

S3 上述三行登记独立本地候选
`e04564142fb372fca98402d23a1707a36403f393`（tree
`dd0fccfd3ed79b6a0e4fbdae757e401c44fa29dc`）中已实现的直接 HTTP transport 纵向。
它尚未发布或安装，不属于当前生产 build39，也没有并入 a323 批次候选；
原 69b 局部内核不是可单摘的本增量。其余 S3/S4 能力仍未完成，不能从这三行推断已激活。

ledger 的 `initialAttempt.state` 零代次仅接受 `ungranted` 或 `uncertain`；
只有本进程普通新建或已成功 ACK 的 S2 M→Q 私有一次证据可给前者，无首次证明的历史/
legacy 纳入为 uncertain，begin 先持久扣一次 transport 才 issued。issued/clean-paused
须为与 record 相同的非零 generation；clean-paused 还须匹配合法 checkpointHash。
暂停、换源、默认变化或重启不补免费 initial/余额；未知扣费 ACK 不发 permit，
只能观察原结果。每 run 初始 worker slot 最多1024，各有一次初始资格；替换对象/slot
或 chunk 偏移不产生新免费资格，收费 wave 与其一次性成员许可保持原 episode 约束。

候选本地证据为 Core 55顶层+51子例 plain/race、HTTP整包168+225 plain及相关28+48 race、
proxy snapshot 与执行投影各2项 plain/race，全部 PASS/0skip；登记交付范围而非重新跑测。
包含真实 Core 扣费提交后进程退出再重开、未知 ACK 零下一 Range、缺坏三方/删除、
旧 waiter/迟到结果、默认摘要及 PAC 边界。直接 HTTP 新 watch 绑定独立 run ctx/channel
并核完整 token；非 HTTP 仍沿原全局 Wait，不能称其旧竞态也已修复。

首步只支持 transport action；refreshUsed/deferralUsed 固定0、notBefore仅空值，
429/503 仍沿旧 Budget/queue，跨重启冷却与共享 debit 尚未接；provider refresh、deferral
和其他恢复来源未闭合。纯读取不创建恢复记录。生产旧任务迁移、真实 App/浏览器、
非 HTTP 恢复、统一恢复 UI 及 S4 校验/发布成功门禁仍未验或未接，不宣称整 S3 完成。

S3 §8.7 的 `executionDefaultsHash`（D）固定为 64 位小写 hex，采用
`SHA256(JSON({version:1,inheritedUserAgent?:TrimSpace(实际默认UA),inheritedProxyPolicyId?:实际有效proxyPolicyId}))`。
只计入实际继承的项：显式 UA 或显式 none/custom 代理不受对应无关全局值变化影响；
无继承项也保存固定 v1 空集合摘要，不用缺字段代替。不保存默认值原文、代理凭据或
第二份认证，不向 raw Request 写回继承的 UA/Proxy。

`sourceHash = SHA256(JSON({version:1,request:HTTP已解释投影,executionDefaultsHash:D}))`。
投影复用 HTTP canonicalizer，覆盖初始 URL、默认 GET 方法、Body、规范 Header、实际
显式代理策略与 SkipVerify，排除 HTTP 不解释的 opaque Extra；不重排 URL query/escape，
不改 Body/认证值。此为源上下文绑定，不是资源身份或完整用户意图指纹；S2 的完整意图
指纹及大整数语义不变。先以旧 D 验证原 raw 绑定，才能 CAS 合法继承项变化；无法核证
旧绑定或默认摘要缺/坏即 context_mismatch held、零 IO。hash、wire 与 SourceKey 必须
消费同一不可变 UA/代理 handler 快照；proxyPolicyId 对相同有效策略可跨重启重算，不能
是进程随机 ID。规范化、换链、普通重定向或重启不重开 episode、不补免费 initial/retry。

首能力仅覆盖能实际冻结的 direct、manual/custom、static/env 代理上下文。PAC 动态
脚本可能执行即不 eligible：未纳入恢复状态的 legacy 任务保留原能力，不 enroll；已含
ref/ledger/required 任一状态的任务遇 PAC、缺能力或无法证明 snapshot 时 held、零 IO，
保留原 episode/预算/issued/notBefore，不降级 legacy 或清状态。PAC 纳入共享恢复仍为
后续计划，不能据当前能力宣称所有 HTTP 恢复已共享预算；不另建 PAC 抓取机制。

### 显式 Retry v2 本地候选的准确边界

此行登记已实现的独立本地候选；最终源码SHA及最后race结果待owner冻结，不表示已审查/合入/安装，也不表示首普通安装包40或恢复候选26已含。
v2 ledger 必需 `lastExplicitRetry:{version:1,actionId,fromEpisode,toEpisode,sourceRevision}`；
其 sourceRevision 是动作发生时的历史值，可小于等于后续当前 revision，默认/来源 CAS
不得改写它。完整 v1 不得含 Last，完整 v2 不得缺 Last；三个既有版本值必须同事务匹配，
capability 仍为 `http-transport-recovery/v1`。bucket名带v1不表示所有record版本可执行。

Retry仅由真实用户对error任务的新动作触发；同action不同payload拒绝，同action重放与
旧expectedEpisode仅查历史、不重新授IO。TaskID/path/checkpoint/sourceHash/
executionDefaultsHash/notBefore 保留，新episode仅清本轮计数/actions，initialAttempt仍为
issued；持久Last不能恢复私有一次许可。扣费/新episode ACK未知时保守，重开/GET/replay
均不补免费初始资格。冻结26/cdd76b11旧运行源码只以临时test overlay打开真实v2 Bolt；
TestFrozenV1ReaderRejectsExplicitRetryV2已实际RUN/PASS，覆盖Setup无fetcher、显式Continue
held与原输出保留。两次overlay未命中的no-tests不计证据。该拒执行证据不证明App39安全
执行新状态；39仍unsafe，禁止裸活动库回退。

### D4 净新覆盖三方 v3 本地候选与合同

此处登记15原合同§18.5、§18.8及本地slot
`4820268823a6f039a65c2f94e664f381e9f90036`，依赖纯值模块
`ffbd8534aa1401391d0502b0f91bc12fcdb0cef8`。slot只定义Coverage与严格版本变体，
没有writer/enroll/activation；后续本地runtime已接线，见以下证据分层。
本笔只有文档，不写数据库、不迁移、不进入normal40/41/42。


#### 旧普通版本的v3政策与证据分层

normal40/41/42均unsupported v3，回退必须同一停写时点App+data+prefs配对，不能裸库回退。
此为支持政策，不等于每个旧版都有v3 held守卫。17定点只读结论与本笔静态核对分层如下：

| 普通版本 | 当前可写入的结论 | 未证明的边界 |
| --- | --- | --- |
| 40 / 02c6c7e | 早于S3，没有Recovery/required守卫；普通无S2关联Task不能保证held | 未实测其误执行，不声称已发生误执行，也不把unsupported政策当已有guard |
| 41 / 4e5182b | reader版本谓词静态严格接受1或2、拒非1或2 | 这里只是静态结论，不是该App打开v3库/冷启动/逐入口拒执行实证 |
| 42 / ea27387 | pkg/download、pkg/rest、internal/protocol/http、internal/transfer运行子树逐字节同ceff | 可关联以下实际writer Bolt→ceff旧reader证据；不是42 App冷启 |

旧e13的planned范围、原负证与文件保留，不补造缺失状态。此次文档变更没有新运行测试或状态写入。

本地成套候选`d31a0b8696d4a6c88fd6c3db86ef7e15e28a2cda`（runtime提交
`99ef225918a85942674b6501f2da5728105030fb`）已接线，待审/未安装。
实际writer测试`TestCoverageRuntimeWriteCompatibilityDatabase`由真实新M与HTTP响应、
install3、join/Sync、earned seq1写出Bolt和实际前缀；`compat-writer.log`为1主RUN/PASS。
该writer使用dataset最终补点前WIP hash层，完整源文件SHA256见
`/tmp/k15-d4-runtime-evidence/source-union.json`的`runtimeWIP`，其中
`recovery_coverage_runtime.go`为`ed3c50965d687cceb0fd25364033ef929d340886e3bcac4b60ebb54f3f0c1ebb`；
不能把这次writer或此前合并race的22主25子改称最终runtime全量复跑。
最终dataset补点后，earned/unknown/repeat-prefix/nextbegin定向plain/race各4主6子PASS，
日志为`dataset-final-plain.log`、`dataset-final-race.log`，原`dataset-ack-old-red.log`保留。

冻结产品树`ceff9bb8abdd0a0e515259d5bf0d9628308799b5`未改源码，仅overlay私有harness，
读取上述同一writer Bolt；`TestFrozenCeffReaderRejectsActualCoverageV3` plain/race各1主实际
RUN/PASS，见`/tmp/k15-d4-runtime-evidence/ceff-reader-final.log`与`ceff-reader-race.log`。
Setup在Restore前held；Continue(single/all/multi-ID filter)、Patch、专用Retry均held，
RecoveryQuery为held/retryAllowed=false；QueryBatch历史仍可读。Restore=0、Build=0、
fetcher=nil，Close前后全bucket/key原JSON及输出字节保全。
专用`ContinueBatch`另以同ceff/同原writer Bolt直接调用补验，plain/race各1主RUN/PASS，
Restore=0/Build=0且全records/output不变；见`continuebatch-reader-supplement.json`与
`ceff-continuebatch-reader-plain.log`、`ceff-continuebatch-reader-race.log`。
原harness与writer DB未重造，不能把原multi-ID filter日志改称专用入口证据。
完整来源、日志摘要与未验证范围见`/tmp/k15-d4-runtime-evidence/results.json`及15原合同§18.8；
旧no-tests日志不计通过，normal40无guard/41仅静态/42源码关联的边界保持上表。

ledger新增唯一`coverage`对象，固定JSON字段为`version:1`、`identityHash`、`size`、
`status:tracking|unprovable`、`durableRanges`、`settledRanges`、`checkpointHash`、
`settlementSequence`；范围元素为半开区间`{begin,end}`。两组持久数组必须非nil，
空值编码为`[]`，缺失/null不补空基线；未知版本、坏摘要/区间/状态按held处理。
identity由实际完整HTTP最终URL（含query/escape）、strong ETag及已知正size绑定，
不持久URL/ETag原文。Durable是安全Sync封存的当前覆盖C，Settled是历史已奖励基线B；
Truncate只清当前C，不能清B或序号。1MiB阈值、小文件完整覆盖及正净新增仅产生数学候选，
真实EOF/Complete、writer join、同fd Sync与资源连续性必须由15的seal另行证明。
范围上限4096是证据边界，溢出/失证只停止自动结算，不截短实际正文或改变原WriteAt n/err。

首装独立于read/save：只有本进程可证明普通新Task或本次new M成功ACK的私有RAM一次槽，
才能在目标首byte（含prefetch向目标copy）之前、同生命周期Fence中Swap消费资格。
Resolve沿已有首请求资格取实际身份；持久ungranted、旧op、历史任务、unknown M ACK、
重载/replay均不能重建此槽；失败烧掉资格。无可信身份时保原v1/v2预算下载，不装空基线。
install只接受完整v1三方、已确认issued的非零generation、无Last/Coverage，以及
tracking/两组空非nil数组/seq0/空checkpointHash的精确initial；同事务核dataset、完整
expected record/token、当前checkpoint存在性和digest，从CURRENT owned Task仅改ref版3，
ledger改版3并存Coverage、required改版3，保原episode/issued/计数/余额/notBefore/source/D/
Meta/S2/completion。它不新开episode、不授首请求许可；未知ACK先撤IO并同域核权威三方，
已v3必须按v3守卫，不降级或补资格。

save只处理完整已有v3，同一UpdateAtomic比较整个expected record（含source/D/Last/
Coverage/seq/actions）与token，另核当前bucketSave的存在性/digest；不能拿旧
Coverage.CheckpointHash替代最新普通Store checkpoint。输入为owned Task和合法非null
checkpoint JSON，内部按原编码digest并重算完整Plan，不信caller净新增/阈值。
snapshotOnly保持episode/generation/initial/计数/actions/B/seq/Last/notBefore；earned
要求不同的新episode及重算正净新增Settlement，消费全部C、seq+1并清本轮计数/actions，
generation不变、initial仍issued，保source/D/Last/notBefore/TaskID/path/checkpoint。
仅事务ACK确认的earned fresh-winner可给15本进程一次settlementAuthorization；
返回bool不是网络许可，unknown ACK/reconcile/reload/replay不补此RAM资格。
S4 hash/publish/C仍同旧token，不在该期间旋转episode；IO/join/Sync不在DB或live锁内执行。

严格reader仅接受：v1无Last/Coverage；v2必Last、无Coverage且ToEpisode等current；
v3必合法Coverage且本代issued或clean-paused，Last可无或为合法历史（From!=To、
SourceRevision不大于current，ToEpisode可不等当前episode）。显式Retry在原v3上保持3、
B/两组Coverage/seq/notBefore，只有明确用户动作才按原合同开新episode，Continue不reset。
历史action的accepted/replayed回执NewEpisode取Last.ToEpisode，外层state.Episode仍为
实际current；不改历史Last、不因自动结算后的current补发许可。外部Recovery/Last wire
仍version1，内部triplet v3不扩REST wire。旧版可读JSON不等于安全执行，配对回退及禁止
裸活动库回灌约束不变；slot/纯模块测试不证明实际writer、旧reader或D4端到端已通过。

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

### HTTP共享恢复三方 v4 planned 合同（未落库/未激活）

仅登记15原合同§19.1 cofreeze，基准`af2c7d23456bb539dd2970a91f8953845bd984d9`。
三方沿原bucket/key，Task ref仍只Version，required仍Version/Capability；ledger原字段不增。
版本4只接受plain `http-shared-recovery/v1`（Coverage nil）或coverage
`http-shared-coverage-recovery/v1`（完整Coverage v1，含tracking/unprovable及原B/seq）。
plain带Coverage、coverage缺坏、混版/未知cap均held；旧v1-v3变体原义不变。

Actions仍原7字段`id/kind/generation/sourceRevision/members/finished/state`，kind闭集为
transport/refresh/deferral，ID连续，NextAction=len(Actions)=TotalUsed=三分类计数之和，
总上限16，分上限6/3/10；不因URL/source/连接变化reset。NotBefore仍原string，空或严格
非零UTC RFC3339Nano；deferralUsed>0须非空，过期不删字段，所有begin均须now>=Until。
延长Until先确认ACK，不截短等待、不新debit/action/wave；命令DTO的Until/ExtendActionID
不是新增持久action字段。同一逻辑动作只扣kind+total一次，重复/查询不恢复发送许可；
known fresh ACK至多一个受控nextwave，deferral只准排队、refresh只准一次dispatcher，
真正IO还受Until及同D/E/G/S/action/workset门禁。unknown/reload无RAM补发资格。

升级仅已有完整v1-v3在Core安全非活动Fence（旧worker锁外join）内调用，同事务首核D，
精确核token/全record/required与CURRENT checkpoint存在/hash，从CURRENT Task仅改
ref.version4、record.version4及required version/cap，其余状态逐值保留。v3包括unprovable
与B/seq必须进coverage variant，不能降plain；v1-v2进plain，不造空Coverage。
全无legacy不enroll，缺坏held，GET/Setup不迁移；升级本身不给initial/earned/continuation。
v4 plain可保原合法v1 G0：四计数/actions零、无Last/Coverage、initial.gen0、
InitialAttempt.CheckpointHash初始证明字段空、Until空；不抬generation/伪issued。
CURRENT bucketSave可存在，升级逐值保全并核Expected.CheckpointPresent/hash CAS，不能删除
或解释成免费；Token仅存储CAS坐标，不能借执行Token.Valid误拒G0，也不由此授IO。
首次免费仍仅本进程原newM确认的RAM证据，否则沿保守收费；
v2/v3及v4 coverage仍拒G0。Retry/save/install等须统一校验变体，未接Until门禁不得放开执行4。

公开Recovery/Last/receipt仍wire1，不公开cap/kind/Until、不添timer。仅合法managed v4且
Until未来时新增reason `cooldown`、retryAllowed=false；历史receipt.newEpisode仍Last.ToEpisode。
冻结af2的supportedRecoveryVersion仅静态拒4；未来须真实v4 writer Bolt分别覆盖plain与
coverage（含真实非零kind/Until），再验证Setup/Continue single/all/multi-ID/专用ContinueBatch/
Patch/Retry在Restore/Build/HTTP前held及全库/输出保全。当前没有这些执行证据，不沿用ceff
v3拒版日志称v4通过。40无guard政策与既有41静态/42源码关联边界保持；App+data+prefs
配对回退不证明裸活动库执行安全。此笔只有planned文档，不改53/54或正常43，不授writer激活。
