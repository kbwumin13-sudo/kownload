# MCP 候选包合同 v1

只有源码包含 `cmd/kownload-mcp/go.mod` 时构建桥。App、Core、Native host、迁移工具、
stdio 桥及对应源码均来自同一已提交 Git archive。builder 必须先提交、合入接线输入并锁定
最后 full SHA；不能把旧 SHA 写进含未提交改动的包。不自动安装或修改 MCP 客户端配置。

包内普通文件固定为 `Contents/Helpers/kownload-mcp`。沿用
`Contents/Resources/build-manifest.json` 的 `sourceCommit/releaseVersion/buildNumber/architecture/preview`，
新增：

```json
{
  "mcp": {
    "manifestVersion": 1,
    "helperPath": "Contents/Helpers/kownload-mcp",
    "version": "<releaseVersion>+<完整sourceCommit>",
    "sourceCommit": "<与顶层相同的40位SHA>",
    "readApiVersion": 1,
    "sdkVersion": "v1.7.0",
    "architecture": "arm64",
    "sha256": "<Helper最终签名后字节摘要>"
  }
}
```

桥使用原有 `-X main.version`；已验证包内普通文件的 `--version` 必须精确返回
`kownload-mcp <mcp.version> read-api=1`。`sdkVersion` 是固定 Go SDK 版本，
不冒充客户端实际协商的 MCP 协议版本；本合同不填猜测的 spec 支持列表。

先 strip/sign 内层 Mach-O 和 frameworks，再计算 Helper 摘要写 manifest，最后封签 App。
App 签名不使用递归 `--deep --force`；签完严格验证 App，重新核 Helper 摘要和版本。
最终 DMG 只读挂载后再次核嵌入 manifest 与 Helper 字节摘要。ad-hoc 只证明本次包的
一致性，不是发布者认证、Apple 公证或系统信任授权。

许可索引取所有实际二进制的 `go version -m` 依赖并集，包含 nested 模块。
`legal/mcp-go-modules.json` 单列桥依赖及 nested go.mod/go.sum 摘要，
`legal/mcp-module-licenses/` 原字节保留该模块许可及 SDK 迁移说明。
对应源码包保留原 Git 源码（含 nested 模块、go.mod/go.sum/licenses）、根依赖索引及 ZIP，
并追加 MCP 索引与实际 nested 依赖 ZIP；相同模块归档可共用，不遗漏或升级依赖。
完整来源与许可判断仍须 S8 分发审查。

后续可信部署 CLI 显式选择 App 与受管 data root，验证上述配对、签名、摘要和版本后，
才可安装为稳定 `dataRoot/bin/kownload-mcp` 普通文件。部署不属于 MCP 工具能力。
安装 xattr 仍为 planned，键/格式由部署实现另定；不得把包内 metadata 当作已部署回执。
