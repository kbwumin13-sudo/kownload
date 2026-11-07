# macOS 构建与发行

当前目标：macOS 12+、Apple Silicon。正式应用身份、数据目录和 host 协议本阶段保持兼容，候选另用临时身份。媒体工具由用户另外安装。这里的流程不读取已安装应用或旧基线包。

## 构建输入

`config/release.json` 是 App 版本、内核/Release 修订、构建号和 SDK 版本的唯一输入。例如 appVersion `1.9.3`、revision `15`、buildNumber `26` 对应内核/Release `1.9.3-kownload.15`、应用 `1.9.3 (26)`。build25 为 `.14`，公开 build19 为 `.8`；应用逐段比较版本，只提示严格更新的最终版本。

构建需 Xcode 26+（原生面板使用其 SDK）；macOS CI 显式选择镜像提供的 Xcode 26.3。实际版本写入构建清单。

先提交源码，然后设置 `GO_BIN` 和 `FLUTTER_BIN` 指向文件中指定版本，运行：

```sh
scripts/macos-frontend-build.sh --preview --output /tmp/kownload-candidate-dist
```

构建脚本只接受干净的当前提交，导出其源码到临时目录，重新构建 Go 内核、host、Flutter 和原生插件；Flutter 引擎来自固定 SDK。组件来源、SDK 与版本写入 `build-manifest.json`。Go 使用 `-trimpath`；Swift/Clang 用源/调试路径映射，Dart 分离调试信息，全部 Mach-O 去除本机调试符号后只做 ad-hoc 候选签名，不使用开发者凭据。

`--preview` 会在最终 plist 使用临时 Bundle ID，移除 URL/文件关联，编译时固定临时数据目录。不要将预览包安装到 Applications。预览包不安装正式 Native manifest 或注册系统 scheme。脚本打印候选 App 和临时数据路径。

## 产物与验证

- `kownload-v<release>-macos-arm64[-preview].dmg`
- `kownload-browser-helper-<helper version>-<release>.zip`
- `kownload-v<release>-source.tar.gz`
- `SHA256SUMS.txt`、`build-manifest.json`、App/DMG路径扫描结果

DMG 实际挂载为只读后再次扫描全部 App 文件并验证签名；个人 home 路径未清零则打包失败。App 的 `Contents/Resources/legal/` 包含主许可、第三方许可、实际 Go 模块的版权/许可证、Go/Flutter/Dart SDK许可、CocoaPods声明、源码位置。Flutter 本身生成 `NOTICES.Z`。助手 ZIP 也带许可与源码说明。

源代码包包含该提交的受控源码及实际链接 Go 模块的原始源码 ZIP；模块清单还提供固定版本、校验和及公开来源。`pubspec.lock` 固定 Flutter 包来源、版本/commit和哈希，polyfill附固定 npm tarball 的完整性与许可证。源码包和构建说明必须与二进制一起提供，不能只链接会变化的 main 分支。

## 发行

只有经过验收且获发布授权时，才创建对应 `v<release>` 标签并使用手动 `kownload macOS release` 工作流。默认只构建和上传工作流工件；`publish` 默认 false。发布作业只接受已存在且与版本配置一致的标签，不引用旧 DockerHub/QpkgBuild 或非macOS目标。

正式分发请在同一 Release 附 DMG、助手 ZIP、对应源码包、构建清单和校验文件。当前本地候选的提交尚未公开，包内 `SOURCE.txt` 会明确这一点。App 只打开对应 Release 页面，没有自动下载或安装器；不得把预览包当成正式更新推荐。

兼容身份迁移、正式签名/公证、媒体环境随包都不在本阶段。
