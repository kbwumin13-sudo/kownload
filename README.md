<a id="readme-top"></a>

<div align="center">

<a href="docs/BRAND.md"><img src="docs/brand/app-icon.png" width="104" height="104" alt="kownload"></a>

# kownload

### 粘贴，回车，开始下载。

常驻菜单栏的 macOS 下载面板。按下 **⌃⌥D**，粘贴链接，文件在后台下完。

A keyboard-first download manager for macOS.

[**下载 macOS · Apple Silicon**](https://github.com/kbwumin13-sudo/kownload/releases/latest/download/kownload-build19-macos-arm64.dmg) · [更新说明](https://github.com/kbwumin13-sudo/kownload/releases/latest) · [安装与使用](#安装)

<img alt="macOS 12+" src="https://img.shields.io/badge/macOS-12%2B-171717?logo=apple&logoColor=white">
<img alt="Apple Silicon" src="https://img.shields.io/badge/Apple_Silicon-arm64-2f6f50">
<a href="LICENSE"><img alt="GPL-3.0" src="https://img.shields.io/badge/License-GPL--3.0-2f6f50"></a>

<br><br>
<img src="docs/images/kownload-panel.png" width="720" alt="build 19 的轻面板：链接输入、任务筛选和下载操作">

</div>

## 随手开始，安静下载

- **轻面板**：系统字体、紧凑控件和分组设置，跟随系统明暗。隐藏后保留输入与等待中的登录流程。
- **一个输入框**：普通链接、多行批量、磁力、种子拖放、ED2K 和网盘分享链接。
- **自动分段**：验证服务器的 Range 能力后启用多连接，支持断点恢复和连接复用。
- **网页媒体**：浏览器助手按播放器提供下载入口，支持 HLS、DASH、音轨和字幕选择；音视频用 FFmpeg 合并。
- **网盘下载**：在网页勾选文件后交给应用，也可粘贴分享链接；支持文件与目录任务。
- **首次引导**：第一次使用时设置下载目录与浏览器助手；完成后不再自动显示，可在通用设置重新打开。

<details>
<summary>查看新设置界面</summary>

<img src="docs/images/kownload-settings.png" width="720" alt="kownload 通用设置：菜单栏、快捷键、外观和使用引导">

</details>

## 安装

**macOS 12 或更新版本，Apple Silicon（M 系列芯片）。**

1. [下载安装包](https://github.com/kbwumin13-sudo/kownload/releases/latest/download/kownload-build19-macos-arm64.dmg)。
2. 打开 DMG，将 **kownload.app** 拖入 **Applications**。
3. 打开应用，按引导完成设置。升级时先退出正在运行的应用，再替换。

本版使用本机 ad-hoc 签名，未做 Apple 公证。首次打开如被系统拦截，请在「系统设置 → 隐私与安全性」中选择「仍要打开」。已有任务、设置和扩展继续沿用，无需清空数据。

### 浏览器助手

应用的首次引导提供助手目录入口。在 `chrome://extensions` 或 `edge://extensions` 开启「开发者模式」，选择「加载已解压的扩展程序」，加载该目录。启动应用后会注册 Native Messaging 主机。

也可以下载 Release 中的 **kownload-browser-helper-0.4.1.zip**，解压后加载。源码位于 [`browser-media-extension/`](browser-media-extension/)。

### 媒体依赖

```sh
brew install ffmpeg node pipx
pipx install yt-dlp
```

默认使用 `~/.local/bin/yt-dlp`、`/opt/homebrew/bin/ffmpeg` 和 `/opt/homebrew/bin/node`。安装在其他位置时，在下载设置中填写实际路径。直播和 DRM 保护内容不在支持范围内。

## 协议与网盘

| 类型 | 能力 |
| :-- | :-- |
| HTTP / HTTPS | 多连接分段、连接复用、断点续传、自定义请求参数 |
| BT / 磁力 | 种子文件选择、Tracker、做种策略 |
| ED2K | ed2k 协议、服务器与节点设置 |
| 网页媒体 | 视频、音频、图片；HLS（含 AES-128）、DASH |
| 网盘 | 百度、阿里、夸克、天翼、迅雷、UC、光鸭、移动、123 |

夸克与百度的文件、目录下载已有真实账号验收记录；其余平台的实现由模拟接口测试覆盖，真实账号矩阵仍待补齐。各平台的登录、分享、转存与恢复边界见 [NETDISK_PROVIDERS.md](NETDISK_PROVIDERS.md)。

登录凭据保留在内存中，任务保存文件身份。暂停或重启后，按提示打开对应网页并同步登录状态，再继续下载。

## 本次更新 · build 19

- 重做轻面板、任务列表、设置与首次使用引导。
- 改善 Range 识别、分段恢复、小文件连接复用与下载调度。
- 修复来源冷却延长、后入队任务及非 Range 暂停继续时的让槽遗漏。
- 修复限流终止错误被延期状态覆盖，以及嵌套转义日志中的 Cookie 脱敏。
- 改善本地通信恢复、浏览器资料读取与网盘目录任务。

本轮 Flutter **151 项**回归通过，相关 Go race 与 vet 通过。build 19 对 build 18 的三个常规场景耗时变化为 **−0.39%～+0.14%**；四个测速场景共 **28 次运行、6,706 个文件哈希全部正确**。令牌桶限流场景中位耗时增加 11.70%，两版波动均超过 40%，暂不能确认稳定无退化。连续 60 分钟稳定性本轮未重跑。

更完整的验证范围、构建身份与保全说明见 [build 19 验证记录](docs/BUILD19_VALIDATION.md)。

## 快捷键

| 快捷键 | 作用 |
| :-- | :-- |
| **⌃⌥D** | 唤起 / 收起轻面板，可重新设置 |
| **↩** | 开始下载 |
| **Esc** | 收起面板 |
| **⌘N** | 新建下载 |
| **⌘F** | 搜索任务 |
| **⌘,** | 打开设置 |
| **⌥⌘I** | 查看任务详情 |

## 开发

需要 Xcode、CocoaPods、Flutter 3.41.2 和 Go 1.24 或更新版本。

```sh
git clone https://github.com/kbwumin13-sudo/kownload.git
cd kownload
scripts/macos-frontend-build.sh
```

可通过 `FLUTTER_BIN`、`GO_BIN` 指定工具链；构建与安装细节见 [MACOS_FRONTEND.md](MACOS_FRONTEND.md)。

```sh
# Flutter 回归
cd ui/flutter
flutter test

# 浏览器助手回归（在仓库根目录执行）
node --test browser-media-extension/tests/*.test.js
```

## 文档

[设计约定](DESIGN.md) · [媒体引擎](MEDIA_SNIFFER.md) · [网盘接口](NETDISK.md) · [网盘验证](NETDISK_PROVIDERS.md) · [品牌资源](docs/BRAND.md)

本项目以 [GPL-3.0](LICENSE) 发布。[第三方许可与版权声明](THIRD_PARTY_NOTICES.md)。
