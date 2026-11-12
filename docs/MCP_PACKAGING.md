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

先处理内层 Mach-O 和 frameworks；MCP Helper 已由 Go `-w -s` 构建，不再执行
Apple strip（其 cgo 间接符号可能被拒绝），仍正常签名。再计算 Helper 摘要写 manifest，最后封签 App。
App 签名不使用递归 `--deep --force`；签完严格验证 App，重新核 Helper 摘要和版本。
最终 DMG 只读挂载后再次核嵌入 manifest 与 Helper 字节摘要。ad-hoc 只证明本次包的
一致性，不是发布者认证、Apple 公证或系统信任授权。

许可索引取所有实际二进制的 `go version -m` 依赖并集，包含 nested 模块。
`legal/mcp-go-modules.json` 单列桥依赖及 nested go.mod/go.sum 摘要，
`legal/mcp-module-licenses/` 原字节保留该模块许可及 SDK 迁移说明。
对应源码包保留原 Git 源码（含 nested 模块、go.mod/go.sum/licenses）、根依赖索引及 ZIP，
并追加 MCP 索引与实际 nested 依赖 ZIP；相同模块归档可共用，不遗漏或升级依赖。
下载许可和源码 ZIP 时使用包含该依赖固定 checksum 的根或 nested `go.sum` 上下文，
并与实际二进制中的版本和 checksum 对照；不向根锁文件补写 nested 依赖或跳过校验。
完整来源与许可判断仍须 S8 分发审查。

唯一的仓库内依赖例外是 nested 桥的
`github.com/GopeedLab/gopeed v0.0.0 => ../.. (devel)`：
collector 保留实际 BuildInfo replacement，核根/nested go.mod 的 module 身份，
并以只读 go list 核 Replace.Dir、Dir 和 GoMod 都指向本次 archive 根。
只对该精确 self case，索引写 sourceKind=repository、sourceCommit=本次锁定40位SHA、
sourcePath=.，保留空 module sum，不伪造上游版本/sum/ZIP。源码包根即其对应源码，
保留原仓库源码及 LICENSE/THIRD_PARTY_NOTICES 原字节；builder 核原 Git tar 的 commit
及其 Go 源码、mod/sum、许可与当前构建树一致，且原成员已收入最终源码包，才豁免该项 ZIP。
未知、外来、sibling/outside 或不匹配的 replacement 拒绝；其他实际外部模块仍逐项核原
sum、go.mod sum、下载源版本及原 ZIP，未共享的旧桥按原流程打包。此例外不启用写工具、
不改变授权/执行能力，也不构成发布者认证或法律兼容性结论。

隔离联调默认仍编译 `work/preview-data`。需要验证部署 CLI 的标准受管根时，
构建可显式传 `--preview --preview-home <已存在的短 /private/tmp 私有HOME>`，
数据根编译为该 HOME 下的 `Library/Application Support/io.github.kbwumin13-sudo.kownload`。
构建前验证当前 UID、逐层普通目录、无链接和不可 group/world 写，并按 UTF-8 字节
拒绝超过 103 字节的 API/host socket 路径；失败时不创建 output/work 目录。
这不创建或更改 HOME/受管根。启动候选和显式部署 CLI 时使用同一个
`HOME`（App 另需相同的 `CFFIXED_USER_HOME`），再核实际 PID、identity 与 socket。
没有该参数时原 CI 行为不变；预览 bundle、禁协议导入和自动剪贴板守卫继续生效，
该参数不提供运行时鉴权或测试数据注入。

后续可信部署 CLI 显式选择 App 与受管 data root，验证上述配对、签名、摘要和版本后，
才可安装为稳定 `dataRoot/bin/kownload-mcp` 普通文件。部署不属于 MCP 工具能力。
安装 xattr 由独立部署实现定义；包内 metadata 不表示稳定入口已部署，也不证明快照保留了 xattr。
