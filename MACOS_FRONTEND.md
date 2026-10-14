# kownload macOS 前端

此分支以 v1.9.3（`a5cd53f94c18ac65add684b1113fa5f0b47cc4da`）为基线，包含 macOS 前端和媒体下载协议。发布内核版本为 `1.9.3-kownload.1`；正式应用身份为 `com.gopeed.gopeed`，沿用已有数据与扩展。

## 构建

本机构建使用项目上一级的 `toolchains/flutter`，版本为 Flutter 3.41.2。其他环境可用 `FLUTTER_BIN`、`GO_BIN` 指定工具，或使用 PATH 中的 Flutter/Go。macOS 构建需要完整 Xcode 与 CocoaPods。

正式产物使用完整 Xcode 构建：

```sh
scripts/macos-frontend-build.sh
```

脚本编译下载内核及 Native 主机，再构建 Flutter 前端。成品在 `ui/flutter/build/macos/Build/Products/Release/Gopeed.app`。封装后按 `../artifacts/build/media-core` 校验新内核和主机，存在本地基线时也校验 updater，并检查应用签名；没有基线时直接从源码编译 updater。

本机 Xcode 27 已不接受上游旧的部署目标。脚本仅为本次构建设置 `MACOSX_DEPLOYMENT_TARGET=12.0` 与 `ARCHS=arm64`，不改仓库的跨平台部署配置。

以下 Command Line Tools 备用脚本用于前端基线；媒体版本使用上面的完整 Xcode 构建脚本：

```sh
DEVELOPER_DIR=/Library/Developer/CommandLineTools python3 scripts/macos-native-clt.py
```

这条路径实际编译新的 Swift 宿主及 Flutter AOT 前端，使用固定版本源码生成插件接口模块，并链接备份应用中的原插件二进制；不重新编译插件实现或 Go 后端。产物为 `../artifacts/release/Gopeed.app`，签名和后端校验随构建执行。

## 窗口与视觉

kownload 使用单个 Flutter 引擎与 AppKit 窗口。默认菜单栏常驻、`⌃⌥D` 唤起640pt原生玻璃卡片，Esc/失焦收起；内容直接放入 `NSGlassEffectView.contentView`。旧系统使用 `NSVisualEffectView`，减少透明度时使用系统实色。设置在同一窗口恢复常规尺寸，任务/通用/下载/连接与能力/高级五类入口保留原能力。

输入普通链接、多行链接、磁链或拖放种子；更多参数按需展开。未完成任务最多显示三条。成功确认后0.8秒收起，新输入会取消收起。切换设置、隐藏和网盘登录等待保留当前工作流。扩展默认只读取已装配置，商店和升级检查在高级维护中按需打开。

原生行为及输入/拖放回归：

```sh
python3 ui/flutter/macos/tests/run_panel_host_tests.py
bash scripts/test-macos-pointer-routing.sh
```

## 隔离实测

设置 `GOPEED_PREVIEW_STORAGE` 为绝对目录，再直接运行构建后的可执行文件。预览会使用独立数据库和本地通信地址，并跳过浏览器注册、浏览器 RPC 服务、开机启动及应用更新检查。

从 Finder 或自动化宿主启动时，也可传 `--preview-storage=/absolute/path/to/preview-store`。

```sh
GOPEED_PREVIEW_STORAGE="/absolute/path/to/preview-store" \
ui/flutter/build/macos/Build/Products/Release/Gopeed.app/Contents/MacOS/Gopeed
```

本机下载样本：

```sh
python3 scripts/macos-fixture-server.py
```

新建下载链接 `http://127.0.0.1:18793/fixture.bin`，选择测试目录，检查暂停、继续、完成及输出校验值。该服务支持 HTTP Range，主动限速以便观察状态变化。

## 键盘

`⌃⌥D` 全局切换轻面板，可在通用设置重新录制；冲突时保留原组合。`⌘N` 新建下载，`⌘F` 搜索完整任务，`⌘,` 设置，`⌥⌘I` 任务详情，列表聚焦时 `⌘A` 全选。`⌘W` 隐藏窗口。删除任务仍通过确认框选择是否同时删除文件。

## 安装与回退

验收后，先正常退出原 Gopeed，再复制旧应用和整个 `~/Library/Application Support/com.gopeed.gopeed` 目录到本项目的备份位置；随后安装 `/Applications/kownload.app`，将旧 `/Applications/Gopeed.app` 保留为兼容入口。避免两个引擎同时打开同一数据库。

回退时正常退出新版，把备份应用恢复到 `/Applications/Gopeed.app` 并恢复原启动入口。应用数据格式保持不变；若需恢复安装时的数据快照，再恢复对应支持目录。

本轮验证说明见 [原生面板验收记录](docs/NATIVE_PANEL_VALIDATION.md)。本机详细日志、截图和备份位于项目上一级 artifacts，独立于公开源码。

## 媒体嗅探

浏览器配套插件及媒体协议说明见 [MEDIA_SNIFFER.md](MEDIA_SNIFFER.md)。macOS 可执行辅助程序通过同目录临时文件原子替换，避免覆盖 Mach-O 后沿用旧的内核签名缓存。
