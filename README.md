# kownload

面向 macOS 的轻量下载器。通过 `⌃⌥D` 唤起原生玻璃面板，粘贴链接后按回车下载；任务、设置和扩展维护按需展开。基于 Gopeed 的下载内核与 Flutter 前端，窗口材质和生命周期由 AppKit 提供。

![kownload 原生轻面板](docs/images/kownload-panel.png)

## 日常使用

- 菜单栏常驻，Esc 或点击外部收起，后台下载继续。
- 普通链接、磁链、种子拖放和多行链接使用同一入口。
- 面板最多显示三条未完成任务；完整任务管理与全部下载参数在设置中。
- 浏览器助手支持页面媒体嗅探、格式选择、批量下载和网盘选中文件发送。
- 包含百度、阿里、移动、天翼、迅雷、夸克、UC、123、光鸭九类网盘适配器；登录上下文通过浏览器同步。

网盘实现与验证范围见 [NETDISK_PROVIDERS.md](NETDISK_PROVIDERS.md)。2026-10-14 已完成夸克真实账号下载及暂停、重启、重新同步后的续传验收；其他网盘以源码和模拟验证为当前证据范围。

## 构建与测试

前端使用 Flutter 3.41.2；Go、Xcode 与 CocoaPods 按项目依赖安装。构建入口和隔离数据运行方式见 [macOS 前端说明](MACOS_FRONTEND.md)。

```sh
cd ui/flutter
flutter pub get
flutter analyze
flutter test
```

原生宿主回归：`python3 ui/flutter/macos/tests/run_panel_host_tests.py`（需要已构建的插件模块）；鼠标路由：`bash scripts/test-macos-pointer-routing.sh`。

## 兼容与来源

为延续既有任务、配置和浏览器助手，保留 `com.gopeed.gopeed` 数据标识、`gopeed:` URL 与 Native Messaging 标识。内部 Go module 与 Flutter package 名继续沿用上游，产品显示名称为 kownload。

上游 [GopeedLab/gopeed](https://github.com/GopeedLab/gopeed) 基线为 v1.9.3；原项目介绍见 [保留的上游文档](docs/GOPEED_UPSTREAM.md)。媒体引擎和助手说明见 [MEDIA_SNIFFER.md](MEDIA_SNIFFER.md)，网盘接口见 [NETDISK.md](NETDISK.md)。

保留 Gopeed 的 GPL-3.0 许可证，以及网盘适配引用的 LinkSwift、OpenList 的 AGPL-3.0 许可证和归属说明，见 [LICENSE](LICENSE) 与 `third_party/`。
