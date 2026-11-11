# 独立身份与本地数据迁移

身份唯一来源是 `config/identity.json`；`python3 scripts/generate-identity.py` 生成 Dart、Swift、Go 与 xcconfig，`--check` 校验已提交文件。Bundle ID/数据根为 `io.github.kbwumin13-sudo.kownload`，可执行文件为 kownload；Native host 名含下划线而 Bundle ID 含连字符，这是两种系统各自的命名规则。Go module 与 libgopeed.dylib 本阶段保留。

默认 IPC 位于新根的 `ipc/api.sock` 和 `ipc/host.sock`；只将已知旧默认地址映射到新默认，保留自定义端点与认证。`kownload:` 与过渡别名 `gopeed:` 进入同一创建/扩展路由。固定浏览器扩展 key/ID 保持。正式包同名 kownload.app 会覆盖原 App 包，但两个数据根独立；同时运行两版会争用下载文件与系统关联，因此安装和回退均先退出。

## 首启

Flutter 在 Hive.init、内核 Start、快捷键/浏览器初始化及自动继续任务之前调用 bootstrap。新版已有有效标记则正常启动；新根有数据无标记则停止，不能自动覆盖。旧版仍运行时停止导入并提示退出；Dart 持有 Hive 文件锁，导入工具持有旧 bbolt 只读锁；锁冲突直接停止。

导入工具使用新根旁的私密 staging，复制完整 Hive、扩展文件及其他持久文件，以 bbolt 只读事务导出 gopeed.db。数据库逐桶遍历全部 key/value 与 bucket sequence，其他文件校验相对位置和 SHA-256，包含 task/save/protocol_state/config/extension/extension_storage 及其未识别新字段。它不解释或改写任务/断点/路径/转存回执，不移动下载文件，不复制旧 host、socket、锁或运行日志。校验后 fsync 并原子 rename 为新根，再发布迁移标记。导入失败不启动内核；rename 后标记前中断须再次校验 pending 清单后才能完成。staging 先在临时名下写好归属令牌再改名到位，所以 staging 路径不会在没有令牌时出现；丢弃旧 staging 也先改名再删除。早期版本中断留下的无令牌（或令牌不符）staging，只在没有标记、没有 pending 清单、新根不存在或为空时才清理：导入工具只读源目录、持有迁移锁，staging 只是可重建的副本。

标记同时记录导入时旧根 `gopeed.db` 的大小和 SHA-256。旧版每次启动都会提交写事务（实测 build 25 空启动也会改变文件字节），因此以后启动时，若导入时确实复制过旧库（标记 `sourceDatabase.present` 为真）、旧版没有运行（能取得旧库的共享锁）而指纹不同，说明旧版在迁移后被使用过：新版停在错误页，不打开数据、不继续任务、不删除任何一侧，（若旧版此时正在运行——进程存在或旧库被锁——同样停在错误页并提示先退出旧版：两个版本同时运行会各自继续同一批任务、写同一个文件），页面给出两条改名命令——把新根改名后重新导入旧版现在的数据，或把旧根改名后继续使用迁移后的数据。旧根同时也是上游 Gopeed 的数据目录，所以导入时旧库不存在（全新安装）的，之后出现的旧根一律不检查；错误页还提供持久选项“保留新数据，今后忽略旧目录的变化”，它让迁移工具（`kownload-migrate --ignore-legacy-changes`）把 `ignoreLegacyChanges` 原子写入新根标记，此后不再检查旧根，也不再做旧版运行检查。两边若是同一批任务，需在其中一个应用里关闭“打开应用时继续未完成的下载”（旧版叫“自动开始任务”），避免两个应用同时写同一个文件。早于此字段的标记不比对。

原生 UserDefaults 只迁 `netdiskProfileFileBookmarks`，不覆盖已有新值。窗口、快捷键、历史与其他配置随 Hive 原样保留。原书签解析失败时继续走现有“选择资料文件”系统授权；不迁移 Keychain、浏览器 Cookie、密码库或整个偏好域。

### S0：启动与执行许可分开

开发版新增迁移标记的可选 `startupPolicy`（v1，`mode`/`reason`），不改变数据库格式。
新空根写 `normal/fresh`；导入数据写 `imported-review/imported_data`。旧标记没有执行
证明时按 `imported-review/legacy_marker`，不会因为打开一次 App 自动改成 normal。
未知政策版本/值拒绝，`ignoreLegacyChanges` 只决定是否检查旧根，不授予执行许可。

bootstrap 将政策作为非持久 `StartConfig.startupTaskPolicy` 传给 core；原用户
`autoStartTasks` 和普通 DownloaderConfig 均不改写。review 下 App 可以查看记录、创建
新任务；Flutter 不发启动自动继续，core 也不自动恢复上传、清理缺失文件记录或执行
无明确 ID 的 Continue-all。提示解释原因，关闭提示不解禁；明确选择任务继续仍需
通过当前 core 的状态版本/held 守卫。没有新增“修改全局自动开始以通过验收”的流程。

状态能解码、能执行和能安全自动续跑分别见[兼容清单](STATE_COMPATIBILITY.md)。
`confirmed_compatible` 是为可信兼容检查保留的 normal 原因；S0 没有自动写入或批量
确认接口，不用手填该值替代逐状态/输出文件核验。

## Native 清单切换

host 从当前数据根连接 `ipc/host.sock`，不回落连接旧库。新名
`io.github.kbwumin13_sudo.kownload` 是自有助手目标；兼容名 `com.gopeed.gopeed`
独立检查。启动流程不写正式浏览器清单，存在旧 alias 不表示必须覆盖或可以回退重放。

S1 的单浏览器服务 `inspectBrowser(browser)` 只读；`connectBrowser(browser,
legacyCompatibility:false)` 必须明确 chrome/edge，不猜默认浏览器、不循环两个目录。
默认只注册新名。旧无参 connectBrowsers 拒绝并要求选择；设置呈现由对应增量接此接口，
不能把旧双浏览器按钮当作新服务已完整接线。

归属检查包含 name/type、固定ID allowlist、绝对自有路径、无重定向symlink、当前UID/
权限及可执行文件与当前App资源的SHA256。路径相同但asset不符为unknown/mismatch；
不能仅凭旧Bundle ID、文件名或自报product认定属于本App。Gopeed/foreign/unknown
既有manifest均保留。新名正常时不要求旧alias同path；显式legacyCompatibility仅创建
缺失alias或更新已证明属于本App的记录，不能用确认框绕过归属。

每browser/name首次原字节或absent先私密保存到
`<新根>/native-manifest-backups/<chrome|edge>/`，失败不写该manifest；重复操作不覆盖
首份备份。每文件原子替换，多个文件只报告各自实际结果，不宣称跨文件事务。
恢复也明确browser/name，只在当前文件仍为本App的预期canonical字节且备份可信时恢复；
后来被第三方修改则拒绝。预览只在显式临时目录内生成清单，不写日常浏览器目录。

服务输出 `native=untested` 和 App/Core/pending 连接状态null，磁盘configured不等于
真实Native连接、Core能力或发布者认证。活动Port/助手握手另验，不能杀旧进程强制换载。
当前本地服务fixture/相关设置回归通过，不代表日常清单已切换；助手写前绑定接线仍需
完成且在独立候选中验证，见[Native契约](NATIVE_MESSAGING.md)。

## 预览隔离

`scripts/build-macos.py --preview` 使用独立预览 Bundle ID 和编译期临时数据根，包内完全移除 URL/document type 注册。host 预览构建禁止执行系统 URL 唤醒，只能连接正在运行的预览 IPC；不会唤起正式应用。预览不自动继续任务，也不注册浏览器清单。

需要测试导入时仅传 `--migration-source /private/tmp/.../legacy-clone`；该目录须是正式根的 APFS `cp -c` 克隆，权限 700，不含报告可见的原始记录。未传源时预览只初始化空实例。正式应用运行中取得的克隆属于崩溃一致快照，不等同于停写事务快照；预览导入时克隆自身已停写，随后使用锁和完整校验。正式首启仍要求退出旧版。

## 配对快照与离线回退准备（S0）

旧的“把新版活动库回灌 build25”的流程停止支持。prepared/aborted Handoff 及后续
operation/校验/副作用等新字段可能被旧 core 忽略；JSON 可读不等于可执行。
旧 `--new-root/--old-root/--backup-root --apply` 以及清单恢复参数在写任何文件前拒绝。
不传 apply 的旧命令只报告缺少配对快照，不复制或移动数据。

默认回退单位是可信冷备份采集的 **App＋同一时点数据＋偏好**。清单 v1 示例：

```json
{
  "version": 1,
  "sourceCommit": "<40位源码SHA>",
  "releaseVersion": "<与App内build-manifest一致>",
  "app": {"path": "kownload.app", "bundleId": "io.github.kbwumin13-sudo.kownload", "buildNumber": 39, "sha256": "<目录摘要>"},
  "data": {"path": "data", "sha256": "<目录摘要>"},
  "preferences": {"path": "preferences.plist", "domain": "io.github.kbwumin13-sudo.kownload", "sha256": "<文件摘要>"}
}
```

路径只能相对清单，组件互不包含；缺件、未知版本、App Info/包内构建清单与配对信息
不符、摘要不符均拒绝。数据目录拒绝 symlink/特殊文件；App 只允许包内相对 symlink。
目录摘要是相对路径排序的紧凑 UTF-8 JSON 数组的 SHA256，每行记录目录
`[path,"directory",permissions]`、文件 `[path,"file",permissions,fileSHA256]` 或包内
链接 `[path,"link",target]`；算法见脚本 `tree_digest`。清单必须在停写后生成，
脚本不能事后证明冷快照采集时点或来源真实性，不将匹配摘要当作执行授权。

```sh
python3 scripts/rollback-identity.py --snapshot-manifest '<冷备份>/snapshot.json' \
  --output '<不存在的离线副本目录>'
# 核验通过后，仅生成离线副本；不安装、不启动、不恢复运行偏好或浏览器清单：
python3 scripts/rollback-identity.py --snapshot-manifest '<冷备份>/snapshot.json' \
  --output '<不存在的离线副本目录>' --apply
```

apply 复制 App.app、data、preferences.plist 与可重验的 snapshot.json，完成后才写
restore-plan.json；原快照、新旧活动根和真实下载文件不动。输出目录独占创建、权限700，
中断只可能留下无完成计划的离线副本，重试使用新路径。工具不会修改已安装 App、
UserDefaults、系统关联或任何 Native 清单，也不充当安装器。

计划固定 `automaticResumeAllowed=false`、`outputFilesVerified=false`：任务引用的输出
文件可能在快照之后变化，恢复数据不恢复这些文件，不能据此启动旧版自动续跑。
实际切换前还需在单独授权下核对配套 App/helper、执行状态、路径/文件、偏好/系统权限
和当前清单归属；保留新版根/文件，不用旧备份覆盖后来用户改动，不重造已删除任务。
fixture 只接受 `/private/tmp` 内路径；系统 Python3.9 和默认 Python 的测试都仅处理假数据。
