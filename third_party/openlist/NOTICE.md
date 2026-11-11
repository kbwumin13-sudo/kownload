# OpenList

内置网盘适配参考 OpenListTeam/OpenList 的 139、123、189、189pc、aliyundrive、aliyundrive_share、quark_uc、guangyapan 与 thunder_browser 驱动。

- 上游：https://github.com/OpenListTeam/OpenList
- 固定源码：[482464c2affc5eeba5d4a25bb272aeb352ec5918](https://github.com/OpenListTeam/OpenList/tree/482464c2affc5eeba5d4a25bb272aeb352ec5918)
- 版权：OpenListTeam 及上游贡献者；具体归属保留在上游源文件。
- 许可：GNU AGPL v3；[本仓许可证原文](LICENSE)与[该固定来源 LICENSE](https://github.com/OpenListTeam/OpenList/blob/482464c2affc5eeba5d4a25bb272aeb352ec5918/LICENSE)逐字节一致。
- 改造：统一会话、来源身份、逐文件解析、批次专用目录、副本身份核验与清理契约；改造后的相关源码随本项目提供。

其他 API 调用证据与对应来源见 [NETDISK_PROVIDERS.md](../../NETDISK_PROVIDERS.md)。Gopeed下载核心/Flutter基础及kownload修改见[总来源声明](../../THIRD_PARTY_NOTICES.md)。

## 已核百度摘要解码片段（2026-11-11）

[本仓internal/netdisk/baidu_hash.go的BaiduMD5](../../internal/netdisk/baidu_hash.go)明确引用并改造[固定上游drivers/baidu_netdisk/util.go的DecryptMd5（453–470行）](https://github.com/OpenListTeam/OpenList/blob/482464c2affc5eeba5d4a25bb272aeb352ec5918/drivers/baidu_netdisk/util.go#L453-L470)：字节偏移9处的标记（从0计数）、逐位XOR和四段置换相对应。本仓增加32字符长度、标记范围与非法字符校验，统一输出小写摘要，并通过现有错误类型返回无效输入。

该文件在 **2026-10-26** 的本仓提交[e97808ae3f06dae008f55f617a7b77dc48e85d13](https://github.com/kbwumin13-sudo/kownload/commit/e97808ae3f06dae008f55f617a7b77dc48e85d13)加入（提交时间+08:00）。这是可核引入记录，不推断算法最初创作日期。本来源声明于 **2026-11-11** 补齐映射；本次没有修改Go实现。

固定上游util.go没有单独的版权署名行。本索引保留既有OpenListTeam及上游贡献者项目级归属，不据提交者姓名推定该函数的版权人，也不新增未经核实的个人作者或年份。更早的片段谱系、具体权利人和组合许可范围仍待确认；LICENSE中的许可证文本版权不充当算法作者版权。本条来源为OpenList，不能归入LinkSwift选择模型的来源范围。
