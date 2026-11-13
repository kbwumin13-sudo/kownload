<a id="readme-top"></a>

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="docs/images/kownload-hero-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="docs/images/kownload-hero-light.svg">
  <img src="docs/images/kownload-hero-light.svg" width="100%" alt="kownload — 把下载收进菜单栏，把桌面留给正在做的事。Control + Option + D 唤起。">
</picture>

<div align="center">

# kownload

**一个快捷键，接住链接、磁力、网页媒体和网盘。**

常驻 macOS 菜单栏。按下 **⌃⌥D**，粘贴链接，回车开始，再回到手头的事。

A keyboard-first download manager for macOS. One shortcut. Your downloads, in one place.

[**下载 macOS 版 ↗**](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/kownload-v1.9.3-kownload.32-macos-arm64.dmg) · [发行说明](https://github.com/kbwumin13-sudo/kownload/releases/tag/v1.9.3-kownload.32) · [三步开始](#安装) · [功能一览](#功能) · [使用指南](docs/GETTING_STARTED.md) · [反馈问题](https://github.com/kbwumin13-sudo/kownload/issues)

![macOS 12+](https://img.shields.io/badge/macOS-12%2B-202b27)
![Apple Silicon](https://img.shields.io/badge/Apple_Silicon-arm64-267454)
[![License: GPL-3.0](https://img.shields.io/badge/License-GPL--3.0-267454)](LICENSE)

</div>

<p align="center">
  <img src="docs/images/kownload-panel.png" width="800" alt="kownload build 19 实机轻面板：链接输入、粘贴入口、任务筛选与下载按钮">
  <br>
  <sub>真实轻面板截图 · build 19。展示操作形态，当前版本细节以发行说明为准。</sub>
</p>

<a id="安装"></a>

## 三步，把下载交给它

**macOS 12+ · Apple Silicon（M 系列芯片）**

1. **下载并安装** — 在 [最新 Release](https://github.com/kbwumin13-sudo/kownload/releases/tag/v1.9.3-kownload.32) 下载 DMG，将 **kownload.app** 拖入 **Applications**。升级前先正常退出旧版。
2. **选好保存位置** — 打开应用，按首次引导选择下载目录；需要从网页接管下载时，再连接浏览器助手。
3. **按下 ⌃⌥D** — 粘贴链接，按 **↩** 开始。面板可以收起，任务留在后台。

安装包采用 ad-hoc 签名，尚未经过 Apple 公证。首次打开被拦截时，到「系统设置 → 隐私与安全性」选择「仍要打开」；如系统询问访问「下载」文件夹，请允许。更多步骤见 [安装与升级](docs/GETTING_STARTED.md#安装与升级)。

> **正式发行版：build 45 · 1.9.3-kownload.32**。配套浏览器助手 0.6.0。下载 [助手 ZIP](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/kownload-browser-helper-0.6.0-1.9.3-kownload.32.zip)、[完整源码包](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/kownload-v1.9.3-kownload.32-source.tar.gz) 或 [SHA-256 校验文件](https://github.com/kbwumin13-sudo/kownload/releases/download/v1.9.3-kownload.32/SHA256SUMS.txt)。

<a id="功能"></a>

## 小面板，完整的下载工作流

### 随手开始，安静运行

- **菜单栏常驻**：默认不占程序坞和 ⌘Tab；也可在设置中打开程序坞图标。
- **一个输入入口**：普通链接、多条链接、磁力和种子文件，按内容进入对应下载流程。
- **为 macOS 操作习惯设计**：键盘唤起、系统文件选择、明暗外观；收起面板后保留输入和等待中的登录流程。

<a id="浏览器助手"></a>

### 网页上的内容，也能接进来

- **Chrome / Edge 助手**：接管可明确关联的 HTTP(S) GET 普通下载，可选在 App 中确认。POST、blob/data、无痕及无法确认关联的下载保留给浏览器。
- **媒体格式由你选**：从播放器选择视频、音轨或字幕；支持 HLS、DASH 和下载后合并。媒体功能需另装 yt-dlp、FFmpeg、Node.js。
- **网盘文件与目录**：粘贴分享链接，或在网页选中文件后交给应用；按需同步登录状态。

[连接浏览器助手 →](docs/GETTING_STARTED.md#浏览器助手) · [配置媒体工具 →](docs/GETTING_STARTED.md#网页媒体) · [网盘支持范围 →](#支持范围)

### 保存到哪里、何时算完成，都更明确

**build 45 已发布**

| 你想做的事 | kownload 的方式 |
| :-- | :-- |
| 按来源或文件类型归类 | 设置域名、MIME、扩展名或组合路径规则；**每次新下载主动勾选启用**，已有任务不移动。 |
| 下载前确认落点 | 支持的流程可预览保存路径；任务受理后固定本次路径，后续规则修改不搬动它。 |
| 核对发布者提供的摘要 | 手动创建单个直接 HTTP(S) 下载时，可填写 **SHA-256**，校验通过后再确认成品。 |
| 中断后继续处理 | 根据协议及当前下载服务的能力提供继续、恢复或显式重试；网盘凭据过期时按提示重新同步。 |

路径规则和恢复按各协议的实际能力工作。HTTP 的 Range 续传依赖源站支持；SHA-256 校验用于手动单链接 HTTP(S)。[使用方式与适用范围 →](docs/GETTING_STARTED.md#路径规则与校验)

### 让 AI 助手读懂下载进度

**build 45 已发布**

通过本机 MCP Helper，查询任务列表、单个任务、进度、统计与错误。**默认只开放五个读取工具**；可信启动配置显式授权后，才增加创建、暂停、继续三个工具。

写入权限限定到指定数据集、输出根和任务 ID，不因“创建成功”自动扩大。创建当前限定 HTTP(S)；未提供的统计会明确表示不可用。[MCP 接入与授权指南 →](cmd/kownload-mcp/README.md)

<a id="支持范围"></a>

## 能下载什么

| 类型 | 支持范围 |
| :-- | :-- |
| **HTTP / HTTPS** | 多连接分段、断点续传、自定义请求参数；先确认源站能力 |
| **BT / 磁力** | 种子文件选择、Tracker、做种策略 |
| **ED2K** | ed2k 链接、服务器与节点设置 |
| **网页媒体** | 视频、音频、图片、HLS（含 AES-128）、DASH；不支持直播和 DRM 内容 |
| **网盘** | 百度、夸克、阿里、天翼、迅雷、UC、光鸭、移动、123；平台能力各有不同 |

**百度、夸克**的文件与目录下载有真实账号验收记录；其余七盘已有适配实现与受控接口测试，真实账号验收仍待补齐。网盘登录凭据仅保留在内存中，暂停或重启后可能需要重新同步。[完整网盘能力矩阵 →](NETDISK_PROVIDERS.md)

独立安装包目前仅发行 macOS arm64。仓库保留其他平台源码，不代表已提供对应的 kownload 安装包。

## 常用动作，交给键盘

| 快捷键 | 操作 |
| :-- | :-- |
| **⌃⌥D** | 唤起 / 收起轻面板，可在设置中自定义 |
| **↩** | 开始下载 |
| **Esc** | 收起面板 |
| **⌘N** / **⌘F** | 新建下载 / 搜索任务 |
| **⌘,** / **⌥⌘I** | 打开设置 / 查看任务详情 |

<details>
<summary>看看设置界面</summary>

<p align="center">
  <img src="docs/images/kownload-settings.png" width="800" alt="kownload build 19 实机设置界面，含菜单栏、快捷键、外观与使用引导">
  <br>
  <sub>真实设置截图 · build 19。</sub>
</p>

首次使用引导可在「设置 → 通用 → 使用引导」重新打开。

</details>

## 继续了解

| 想做什么 | 从这里开始 |
| :-- | :-- |
| 安装、升级、连接助手、配置媒体 | [使用指南](docs/GETTING_STARTED.md) |
| 了解网页媒体与网盘 | [媒体说明](MEDIA_SNIFFER.md) · [网盘能力矩阵](NETDISK_PROVIDERS.md) |
| 接入 AI 助手 | [MCP Helper](cmd/kownload-mcp/README.md) |
| 从源码构建、了解发行产物 | [构建与发行](docs/RELEASING.md) · [版本与工具链](config/release.json) |
| 查看设计与项目来源 | [设计约定](DESIGN.md) · [品牌资源](docs/BRAND.md) · [上游资料](docs/GOPEED_UPSTREAM.md) |

欢迎 [提交 Issue](https://github.com/kbwumin13-sudo/kownload/issues) 或贡献代码与文档。反馈时请附应用版本、复现步骤和脱敏后的错误信息。

### 开放源码，也保留来处

kownload 基于 [Gopeed](https://github.com/GopeedLab/gopeed) v1.9.3 的 Go 下载内核与 Flutter 应用继续开发，感谢上游作者与贡献者。仓库主许可证为 [GPL-3.0](LICENSE)；第三方改编、依赖与资产的许可及归属见 [THIRD_PARTY_NOTICES](THIRD_PARTY_NOTICES.md) 和 [品牌说明](docs/BRAND.md)。各 Release 随包提供对应源码、构建清单与 SHA-256 校验文件。

应用只提示新版本并打开 Release 页面，安装由你决定。没有内置在线扩展市场或安装统计上报；已安装扩展、设置、更新与手动 Git URL 安装继续保留。

<p align="center">
  <img src="docs/brand/app-icon.png" width="56" height="56" alt="kownload 翡翠绿折带图标">
  <br>
  <strong>粘贴，回车，继续专注。</strong>
  <br>
  <a href="https://github.com/kbwumin13-sudo/kownload/releases/tag/v1.9.3-kownload.32">下载 kownload</a> · <a href="#readme-top">回到顶部 ↑</a>
</p>
