# 独立身份与本地数据迁移

身份唯一来源是 `config/identity.json`；`python3 scripts/generate-identity.py` 生成 Dart、Swift、Go 与 xcconfig，`--check` 校验已提交文件。Bundle ID/数据根为 `io.github.kbwumin13-sudo.kownload`，可执行文件为 kownload；Native host 名含下划线而 Bundle ID 含连字符，这是两种系统各自的命名规则。Go module 与 libgopeed.dylib 本阶段保留。

默认 IPC 位于新根的 `ipc/api.sock` 和 `ipc/host.sock`；只将已知旧默认地址映射到新默认，保留自定义端点与认证。`kownload:` 与过渡别名 `gopeed:` 进入同一创建/扩展路由。固定浏览器扩展 key/ID 保持。正式包同名 kownload.app 会覆盖原 App 包，但两个数据根独立；同时运行两版会争用下载文件与系统关联，因此安装和回退均先退出。

## 首启

Flutter 在 Hive.init、内核 Start、快捷键/浏览器初始化及自动继续任务之前调用 bootstrap。新版已有有效标记则正常启动；新根有数据无标记则停止，不能自动覆盖。旧版仍运行时停止导入并提示退出；Dart 持有 Hive 文件锁，导入工具持有旧 bbolt 只读锁；锁冲突直接停止。

导入工具使用新根旁的私密 staging，复制完整 Hive、扩展文件及其他持久文件，以 bbolt 只读事务导出 gopeed.db。数据库逐桶遍历全部 key/value 与 bucket sequence，其他文件校验相对位置和 SHA-256，包含 task/save/protocol_state/config/extension/extension_storage 及其未识别新字段。它不解释或改写任务/断点/路径/转存回执，不移动下载文件，不复制旧 host、socket、锁或运行日志。校验后 fsync 并原子 rename 为新根，再发布迁移标记。导入失败不启动内核；中断只重建本工具拥有的 staging；rename 后标记前中断须再次校验 pending 清单后才能完成。

原生 UserDefaults 只迁 `netdiskProfileFileBookmarks`，不覆盖已有新值。窗口、快捷键、历史与其他配置随 Hive 原样保留。原书签解析失败时继续走现有“选择资料文件”系统授权；不迁移 Keychain、浏览器 Cookie、密码库或整个偏好域。

## Native 清单切换

host 从当前数据根连接 `ipc/host.sock`，不回落连接旧库。新名 `io.github.kbwumin13_sudo.kownload` 与旧名 `com.gopeed.gopeed` 两份 manifest 指向同一个 `kownload-native-host`。清单生成器保留原文件的私密逐字节备份；原本不存在的清单记为 absent，供回退恢复。启动流程不写正式浏览器清单。

本轮只在临时目录生成/验证清单。正式切换须在用户确认安装时执行显式安装动作，原清单备份按浏览器分目录保存到新根。旧版重新启动可能重装旧清单；安装后应显示当前连接归属，不能静默反复争抢。此处不声称已切换用户的正式浏览器。

## 预览隔离

`scripts/build-macos.py --preview` 使用独立预览 Bundle ID 和编译期临时数据根，包内完全移除 URL/document type 注册。host 预览构建禁止执行系统 URL 唤醒，只能连接正在运行的预览 IPC；不会唤起正式应用。预览不自动继续任务，也不注册浏览器清单。

需要测试导入时仅传 `--migration-source /private/tmp/.../legacy-clone`；该目录须是正式根的 APFS `cp -c` 克隆，权限 700，不含报告可见的原始记录。未传源时预览只初始化空实例。正式应用运行中取得的克隆属于崩溃一致快照，不等同于停写事务快照；预览导入时克隆自身已停写，随后使用锁和完整校验。正式首启仍要求退出旧版。

## 简单回退

保持数据库格式，按“退出新版 → 备份旧根 → 把新根复制回旧根 → 恢复原清单 → 装回 build 25”回退。保留新根和旧根备份，以便处理复制/安装失败。没有兼容启动器或第三运行目录，也不做数据库 schema 转换。

```sh
python3 scripts/rollback-identity.py --new-root '<新根>' --old-root '<旧根>' \
  --backup-root '<尚不存在的私密备份目录>'
# 核对参数后显式执行；每个浏览器可附加一个 --manifest-pair：
python3 scripts/rollback-identity.py --new-root '<新根>' --old-root '<旧根>' \
  --backup-root '<尚不存在的私密备份目录>' \
  --manifest-pair '<该浏览器原清单备份目录>' '<该浏览器 NativeMessagingHosts 目录>' --apply
```

脚本默认不复制；执行时先检查进程/数据库与 Hive 锁，复制并校验到短期 staging，再把旧根原子移至私密备份并发布副本。staging 只是同格式复制的临时位置，不是兼容运行根。原 host 从旧根保留，别名清单按原文件或 absent 恢复；脚本不安装或启动 App。安装旧包后首次下载目录权限可能需要重新授权。若曾显式更改本机后端设置，请在旧版重新检查默认 IPC；远程/自定义端点保留其原有语义。

验证只对临时克隆使用 `--fixture --apply`，该模式拒绝临时根之外的路径。build 25 内核打开新版写出的 bbolt 与旧解码忽略新字段，必须单独验收；仅比较文件哈希或新版可启动不能代替旧内核实测。对外报告只写数量、哈希一致性和通过/失败，不打印记录、凭据或真实路径。
