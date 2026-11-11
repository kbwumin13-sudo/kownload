# LinkSwift

内置网盘下载能力参考并适配 hmjz100/LinkSwift 的各站点实现。

- 上游：https://github.com/hmjz100/LinkSwift
- 作者：Hmjz100、油小猴；原项目：https://github.com/syhyz1990/baiduyun
- 稳定源码：[9fb5b9c7a069a02b335788b1352d107c7308a6f6](https://github.com/hmjz100/LinkSwift/tree/9fb5b9c7a069a02b335788b1352d107c7308a6f6)（1.1.3）
- 开发源码：[88f775d84a57764def07a32da9457cf3043ad11b](https://github.com/hmjz100/LinkSwift/tree/88f775d84a57764def07a32da9457cf3043ad11b)（1.1.4）
- 上游脚本头版权原文：`© 2022 hmjz100`；`@author` 为 `Hmjz100、油小猴`。见[固定脚本头](https://github.com/hmjz100/LinkSwift/blob/88f775d84a57764def07a32da9457cf3043ad11b/（改）网盘直链下载助手.user.js#L1)。
- 许可：AGPL-3.0-or-later（源码声明）；[本仓许可证原文](LICENSE)与[该固定来源 LICENSE](https://github.com/hmjz100/LinkSwift/blob/88f775d84a57764def07a32da9457cf3043ad11b/LICENSE)逐字节一致。

改造包括提取网页上下文、内置Go适配、任务恢复、临时转存及清理；源码复用与适配部分继续遵循上游许可。

## 已核片段与修改记录（2026-11-11）

下表均指上述固定开发源码，不以相同的1.1.4版本号代替提交身份。本仓函数位于 [browser-media-extension/netdisk-page.js](../../browser-media-extension/netdisk-page.js)。

| 本仓位置 | 固定上游位置 | 可确证的来源与改造 |
| --- | --- | --- |
| `readNetdiskPage` 的百度布局与选择分支 | [getSelectedList，6384–6403行](https://github.com/hmjz100/LinkSwift/blob/88f775d84a57764def07a32da9457cf3043ad11b/（改）网盘直链下载助手.user.js#L6384-L6403) | 文件已明确声明selection layouts derived；保留`.file-list`、`.wp-s-core-pan`和固定require选择入口，改为有界MAIN-world采集、统一文件记录与显式选择校验。 |
| `propsOf` / `selected` | [findReact，1967–1992行](https://github.com/hmjz100/LinkSwift/blob/88f775d84a57764def07a32da9457cf3043ad11b/（改）网盘直链下载助手.user.js#L1967-L1992)、[夸克选择，8279–8296行](https://github.com/hmjz100/LinkSwift/blob/88f775d84a57764def07a32da9457cf3043ad11b/（改）网盘直链下载助手.user.js#L8279-L8296) | 参考并改造React属性/选择键模型，本仓限制遍历深度并处理多种记录字段；逐表达的创作谱系未全部确认。 |
| 迅雷已有凭据读取分支 | [getToken的credentials_/captcha_读取，7477–7488行](https://github.com/hmjz100/LinkSwift/blob/88f775d84a57764def07a32da9457cf3043ad11b/（改）网盘直链下载助手.user.js#L7477-L7488) | 既有注释明确参考LinkSwift存储模型；本仓按客户端与有效期选择已有凭据，没有把上游刷新流程整体搬入页面采集函数。 |

Kownload修改的可核Git记录（日期取提交记录的+08:00时间）：

- **2026-10-14**：[4d20c368ed29275dc8a5f52281aec8870fb4f8cb](https://github.com/kbwumin13-sudo/kownload/commit/4d20c368ed29275dc8a5f52281aec8870fb4f8cb)加入该页面采集文件，已含派生布局声明、有界采集和统一文件记录。
- **2026-10-26**：[e97808ae3f06dae008f55f617a7b77dc48e85d13](https://github.com/kbwumin13-sudo/kownload/commit/e97808ae3f06dae008f55f617a7b77dc48e85d13)修改对象归一化、天翼已初始化browserId读取及迅雷/光鸭凭据期限、客户端和sub隔离。
- **2026-11-11**：补齐本来源索引与文件头声明；本次仅修改文字/注释，未改上述运行逻辑。

这些日期标识本仓可核版本，不推断首次创作时间或为第三方新增版权年份。光鸭web bundle现只有文件名注释，固定URL/内容摘要及逐表达来源仍待确认；Go provider的完整片段谱系和组合许可范围也未在此表中全部闭合。上述上游版权、作者及许可说明继续保留，不据此宣称全原创或已通过全部法律兼容性审查。
