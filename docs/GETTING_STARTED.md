# kownload 使用指南

[← 返回主页](../README.md) · [下载发行版](https://github.com/kbwumin13-sudo/kownload/releases/tag/v1.9.3-kownload.32)

本指南适用于正式 **build 45 · 1.9.3-kownload.32**，涵盖安装、浏览器助手、媒体工具与可选下载能力。配套[完整源码包](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/kownload-v1.9.3-kownload.32-source.tar.gz)和[SHA-256 校验文件](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/SHA256SUMS.txt)与安装包在同一 Release 提供。

## 安装与升级

需要 macOS 12 或更新系统、Apple Silicon（M 系列芯片）。

1. 从 [Release](https://github.com/kbwumin13-sudo/kownload/releases/tag/v1.9.3-kownload.32) 下载 DMG，打开后将 `kownload.app` 拖入 `Applications`。
2. 打开应用，跟随使用引导选择下载目录。
3. 按 **⌃⌥D** 唤起面板，粘贴链接并回车。

应用默认常驻菜单栏。若喜欢常规应用的切换方式，可在「设置 → 通用」打开「在程序坞中显示 kownload」。首次引导也可在「设置 → 通用 → 使用引导」重新打开。

安装包采用 ad-hoc 签名，尚未经过 Apple 公证。首次打开被系统拦截时，在「系统设置 → 隐私与安全性」中选择「仍要打开」。若系统询问访问「下载」文件夹，请允许后再继续任务。

升级前正常退出旧版，再替换 App。应用会提示新版本并打开 Release 页面，不自动下载安装；请选用同一 Release 配套的 App 与浏览器助手。回退前阅读对应版本的发行说明，先保留当前应用和数据。

### 从 build 25 或更早版本升级

首次打开新版会把旧数据根 `~/Library/Application Support/com.gopeed.gopeed` 只读导入 `~/Library/Application Support/io.github.kbwumin13-sudo.kownload`。任务、设置和扩展继续沿用，旧目录保留。导入前退出旧版，之后重新连接浏览器助手。详细说明见 [身份迁移](IDENTITY_MIGRATION.md)。

## 浏览器助手

支持 Chrome 与 Edge，负责普通下载交接以及网页媒体、网盘文件选择。

1. 在首次引导中打开助手目录，或下载本版[浏览器助手 ZIP](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/kownload-browser-helper-0.6.0-1.9.3-kownload.32.zip) 并解压。
2. 打开 `chrome://extensions` 或 `edge://extensions`，开启「开发者模式」，点「加载已解压的扩展程序」，选择助手目录。
3. 在 App「设置 → 通用 → 浏览器助手」中点「连接…」，完成连接。助手更新后在扩展页点「重新加载」。

普通下载接管、是否先确认及媒体偏好在 App 中按浏览器管理。首次启用普通下载接管时，浏览器会请求下载权限。为避免重复接管，请关闭其他扩展的普通下载接管；其媒体功能可以继续使用。

接管范围是可明确关联的 HTTP(S) GET 下载。POST、blob/data、无痕及关联不明的下载继续由浏览器处理。若确认前浏览器已经完成，保留其原文件，不再另建一次下载。

助手为本地未打包扩展，需要手动更新。更多说明见 [浏览器助手 README](../browser-media-extension/README.md)。

## 网页媒体

普通文件下载不依赖额外媒体工具。下载网页媒体前，先安装：

```sh
brew install ffmpeg node pipx
pipx install yt-dlp
```

默认工具路径：

| 工具 | 默认位置 |
| :-- | :-- |
| yt-dlp | `~/.local/bin/yt-dlp` |
| FFmpeg | `/opt/homebrew/bin/ffmpeg` |
| Node.js | `/opt/homebrew/bin/node` |

若安装位置不同，在下载设置中填写实际路径。用浏览器助手选择媒体格式、音轨与字幕，再提交下载。直播和 DRM 保护内容不在支持范围。详细支持与恢复方式见 [媒体说明](../MEDIA_SNIFFER.md)。

## 路径规则与校验

本节能力已随 **build 45** 正式发布。

### 为本次下载选择路径规则

在设置的路径规则中，按域名、MIME、扩展名或组合条件设置保存目录，并调整顺序。规则默认关闭；创建新任务时，还需勾选 **「为本次下载冻结保存路径」** 才会使用。

规则作用于本次下载的保存根。任务受理后保留该次路径决定，修改规则不会移动已有任务；多文件任务保持成员的相对结构。支持的创建流程可先预览路径，预览本身不创建任务。不同协议由当前下载服务确认是否支持，网盘创建流程不提供同样的路径预览。

### 可选 SHA-256

手动创建**单个直接 HTTP(S)** 下载时，填写发布者提供的 SHA-256。应用下载后按该摘要核对，再确认成品；普通下载不强制填写摘要。

此选项不适用于磁力、网盘分享、媒体解析或多链接批次，也不与自动加种、自动解压组合使用。创建页会说明不支持的组合。[校验能力文档](VERIFIED_HTTP_COMPLETION.md) 提供开发者接口细节。

### 按协议恢复

暂停后使用「继续」恢复原任务；错误后的显式「重试」按当前协议与服务提供的能力操作。HTTP 续传需源站支持 Range，网页媒体和网盘还可能需要更新链接或重新同步登录状态。各协议保留自己的任务语义，不会把任务自动换成另一种协议。

## 网盘登录与文件选择

可粘贴分享链接，或在支持的网盘网页选择文件与目录后交给 App。百度、夸克已有真实账号文件与目录下载记录；其余平台的受控测试与真实账号状态详见 [能力矩阵](../NETDISK_PROVIDERS.md)。

登录凭据仅在内存中保留。暂停或重启后，按应用提示打开对应网页并同步登录状态，再继续原任务。

## MCP：先读取，再按需授权

**build 45 已发布。** 包内 Helper 默认提供任务列表、任务详情、进度、统计、错误五个读取工具；没有对应观测的数据会表示不可用，不以零代替。

创建、暂停、继续三个写工具默认关闭。需要由可信本机启动配置显式提供 policy，并由 App 确认授权的数据集、允许的输出根、任务 ID 和网络范围。当前创建只接受 HTTP(S)；新建任务不会自动进入允许控制的任务列表。

配置路径、安装 Helper 与授权细节集中在 [MCP Helper 指南](../cmd/kownload-mcp/README.md)。请让 App 与 Helper 来自配套版本；不要把凭据直接放进聊天或 MCP 工具参数。

## 从源码构建

使用 [config/release.json](../config/release.json) 指定的工具链版本；当前为 Go 1.25.4、Flutter 3.41.2，并需要 Xcode 26+ 与 CocoaPods。

在干净且已提交的仓库中运行：

```sh
export GO_BIN="/absolute/path/to/go/bin/go"
export FLUTTER_BIN="/absolute/path/to/flutter/bin/flutter"
scripts/macos-frontend-build.sh --preview --output /tmp/kownload-candidate-dist
```

脚本从当前提交导出源码，重新编译应用组件并生成独立预览候选，不读取已安装 App。预览包不要安装到 Applications。正式构建、对应源码和发行文件说明见 [构建与发行](RELEASING.md)。

[返回主页 ↑](../README.md)
