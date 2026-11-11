# 独立身份与本地数据迁移

身份唯一来源是 `config/identity.json`；`python3 scripts/generate-identity.py` 生成 Dart、Swift、Go 与 xcconfig，`--check` 校验已提交文件。Bundle ID/数据根为 `io.github.kbwumin13-sudo.kownload`，可执行文件为 kownload；Native host 名含下划线而 Bundle ID 含连字符，这是两种系统各自的命名规则。Go module 与 libgopeed.dylib 本阶段保留。

默认 IPC 位于新根的 `ipc/api.sock` 和 `ipc/host.sock`；只将已知旧默认地址映射到新默认，保留自定义端点与认证。`kownload:` 与过渡别名 `gopeed:` 进入同一创建/扩展路由。固定浏览器扩展 key/ID 保持。正式包同名 kownload.app 会覆盖原 App 包，但两个数据根独立；同时运行两版会争用下载文件与系统关联，因此安装和回退均先退出。

## 首启

Flutter 在 Hive.init、内核 Start、快捷键/浏览器初始化及自动继续任务之前调用 bootstrap。新版已有有效标记则正常启动；新根有数据无标记则停止，不能自动覆盖。旧版仍运行时停止导入并提示退出；Dart 持有 Hive 文件锁，导入工具持有旧 bbolt 只读锁；锁冲突直接停止。

导入工具使用新根旁的私密 staging，复制完整 Hive、扩展文件及其他持久文件，以 bbolt 只读事务导出 gopeed.db。数据库逐桶遍历全部 key/value 与 bucket sequence，其他文件校验相对位置和 SHA-256，包含 task/save/protocol_state/config/extension/extension_storage 及其未识别新字段。它不解释或改写任务/断点/路径/转存回执，不移动下载文件，不复制旧 host、socket、锁或运行日志。校验后 fsync 并原子 rename 为新根，再发布迁移标记。导入失败不启动内核；rename 后标记前中断须再次校验 pending 清单后才能完成。staging 先在临时名下写好归属令牌再改名到位，所以 staging 路径不会在没有令牌时出现；丢弃旧 staging 也先改名再删除。早期版本中断留下的无令牌（或令牌不符）staging，只在没有标记、没有 pending 清单、新根不存在或为空时才清理：导入工具只读源目录、持有迁移锁，staging 只是可重建的副本。

标记同时记录导入时旧根 `gopeed.db` 的大小和 SHA-256。旧版每次启动都会提交写事务（实测 build 25 空启动也会改变文件字节），因此以后启动时，若导入时确实复制过旧库（标记 `sourceDatabase.present` 为真）、旧版没有运行（能取得旧库的共享锁）而指纹不同，说明旧版在迁移后被使用过：新版停在错误页，不打开数据、不继续任务、不删除任何一侧，（若旧版此时正在运行——进程存在或旧库被锁——同样停在错误页并提示先退出旧版：两个版本同时运行会各自继续同一批任务、写同一个文件），页面给出两条改名命令——把新根改名后重新导入旧版现在的数据，或把旧根改名后继续使用迁移后的数据。旧根同时也是上游 Gopeed 的数据目录，所以导入时旧库不存在（全新安装）的，之后出现的旧根一律不检查；错误页还提供持久选项“保留新数据，今后忽略旧目录的变化”，它让迁移工具（`kownload-migrate --ignore-legacy-changes`）把 `ignoreLegacyChanges` 原子写入新根标记，此后不再检查旧根，也不再做旧版运行检查。两边若是同一批任务，需在其中一个应用里关闭“打开应用时继续未完成的下载”（旧版叫“自动开始任务”），避免两个应用同时写同一个文件。早于此字段的标记不比对。

原生 UserDefaults 只迁 `netdiskProfileFileBookmarks`，不覆盖已有新值。窗口、快捷键、历史与其他配置随 Hive 原样保留。原书签解析失败时继续走现有“选择资料文件”系统授权；不迁移 Keychain、浏览器 Cookie、密码库或整个偏好域。

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

## 简单回退

保持数据库格式，按“退出新版 → 备份旧根 → 把新根复制回旧根 → 恢复原清单 → 新根改名保留 → 装回 build 25 → 换回旧版助手”回退。换回旧版助手是必做步骤：新助手连到旧 host 时，旧版应用会建好任务，但助手只能显示 `legacy_host` 提示，用户再点会再建一个。新根最后改名为 `<新根>.rolled-back-<时间戳>`（脚本输出 `newRootKeptAt`），旧根备份保留，以便处理复制/安装失败。新根不留在原名下，是为了让以后再次升级时重新从旧根导入回退后旧版产生的数据，而不是静默打开回退那一刻的副本；万一改名前中断，首启的旧库指纹比对会停在错误页。没有兼容启动器或第三运行目录，也不做数据库 schema 转换。脚本兼容 macOS 自带的 Python 3.9。

```sh
python3 scripts/rollback-identity.py --new-root '<新根>' --old-root '<旧根>' \
  --backup-root '<尚不存在的私密备份目录>'
# 核对参数后显式执行；--restore-browser-manifests 按新根里的备份恢复 Chrome/Edge，
# 其他目录可用 --manifest-pair '<原清单备份目录>' '<NativeMessagingHosts 目录>' 逐个指定：
python3 scripts/rollback-identity.py --new-root '<新根>' --old-root '<旧根>' \
  --backup-root '<尚不存在的私密备份目录>' --restore-browser-manifests --apply
```

脚本默认不复制；执行时先检查进程/数据库与 Hive 锁，复制并校验到短期 staging，再把旧根原子移至私密备份并发布副本。staging 只是同格式复制的临时位置，不是兼容运行根。原 host 从旧根保留，别名清单按原文件或 absent 恢复；脚本不安装或启动 App。安装旧包后首次下载目录权限可能需要重新授权。若曾显式更改本机后端设置，请在旧版重新检查默认 IPC；远程/自定义端点保留其原有语义。

验证只对临时克隆使用 `--fixture --apply`，该模式拒绝临时根之外的路径。build 25 内核打开新版写出的 bbolt 与旧解码忽略新字段，必须单独验收；仅比较文件哈希或新版可启动不能代替旧内核实测。对外报告只写数量、哈希一致性和通过/失败，不打印记录、凭据或真实路径。
