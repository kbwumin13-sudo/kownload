# kownload 下载助手 · Chrome／Edge 商店送审草稿

适用源码为本分支的 MV3 助手，配套桌面端为 macOS kownload。Chrome Web Store 与 Microsoft Edge Add-ons 分开送审。发布者资料、实际商店 ID、正式端到端截图和审核结果均待取得。

## 商店文案

| 字段 | Chrome Web Store | Microsoft Edge Add-ons |
| --- | --- | --- |
| 名称 | kownload 下载助手 / kownload Download Assistant | kownload 下载助手 / kownload Download Assistant |
| 简介草案 | 将普通下载、选中的网页媒体和网盘文件交给 macOS 上的 kownload。 | 将选中的网页下载交给 macOS 上的 kownload。 |
| English short description | Send browser downloads and selected web media or cloud files to kownload on macOS. | Send selected downloads to kownload on macOS. |

**中文详情**　此扩展将浏览器中的下载交给 macOS 上的 kownload。安装并打开配套桌面 App 后，在工具栏检查 Native Host 握手、App 是否可达以及身份，再同步设置。普通下载接管需单独授权，并在 App 中选择确认或自动交接。网页媒体和受支持网盘的文件由你选择。也可选中网页链接，在 App 中预览、勾选并确认批量下载。浏览器会继续处理未启用接管或无法安全交接的下载。

**English description**　Send downloads from your browser to the kownload desktop app on macOS. Check your local connection from the extension toolbar and sync settings once the native host and desktop app match. Handling ordinary downloads requires separate permission and an App preference. You choose the web media and files from supported cloud services that you want to send. Select links in the page to preview a batch in the app before confirming any downloads. The browser continues to handle downloads when handoff is disabled or unavailable.

**Single purpose**　Delegate eligible browser downloads and user-selected web resources to the local kownload desktop app. Edge 的短简介取自上传 manifest，准备正式包时须让 manifest 与其中一条已认可的简介一致。审核备注应提供与实际待审扩展配套的 macOS App 获取方式和安装说明，写出可用商店来源对应的操作。 [Chrome 隐私填写](https://developer.chrome.com/docs/webstore/cws-dashboard-privacy) · [Edge 发布表单](https://learn.microsoft.com/en-us/microsoft-edge/extensions/publish/publish-extension)

## 当前权限的人工审核依据

| 声明 | 用途与人工核对 |
| --- | --- |
| `nativeMessaging` | 通过固定名称与本机宿主握手，再经 Unix socket 到配套 App。核对助手来源和宿主内允许列表，以及旧官方助手的独立兼容路径。 |
| `storage` | 保存资料随机标识、路由绑定、偏好缓存、同步时间和交接回执日志，候选另存在 session storage。检查恢复记录只含非敏感元数据，没有 URL、Cookie 或 token。 |
| `tabs` | 从明确的弹窗资源操作确定活动页和 tab，定位文件选择与媒体会话。检查连接的消息支路不查询 tabs。 |
| `webNavigation` | 跟踪 tab、frame、document 导航及页面代次，废弃旧页面候选或采集。 |
| `webRequest` | 观察媒体候选并关联浏览器下载和实际 HTTP(S) GET 请求，继承匹配的请求头。审查普通下载、媒体、XHR 与 POST 的分流和保留浏览器场景。 |
| `cookies` | 为选中的资源或授权的网盘采集读取标签所属 cookie store、分区及匹配域的凭据。检查无跨站继承、无任意 Cookie 代理。 |
| `scripting` | 按明确的采集操作读取受支持网盘的所选文件和认证字段，或在当前 HTTP(S) 顶层文档的选区读取锚点和选中的 HTTP(S) 文本。选区导入复用现有权限。 |
| `<all_urls>`、全 frame 媒体脚本 | 页面媒体可来自任意站点和跨域 frame，普通下载来源不限于固定站点。网盘脚本仍只匹配 manifest 列出的域。审核时用多个来源核实覆盖面与披露，按两家最小权限政策逐项复查。 |
| 可选 `downloads` | 工具栏明确授权后才能对普通下载做暂停、恢复、查询或取消。App 的接管偏好仍须另行开启。拒绝授权后浏览器保持原下载流程。 |

政策与权限说明以发布当天的官方表单为准。 [Chrome Web Store 政策](https://developer.chrome.com/docs/webstore/program-policies/policies) · [Edge 开发者政策](https://learn.microsoft.com/en-us/legal/microsoft-edge/extensions/developer-policies)

## 隐私披露草案

扩展的单一用途是将可交接下载发给用户本机的 kownload。随功能处理网页与媒体 URL、页面和下载元数据、文件名、所选网盘文件信息，以及来源网站为下载所需的请求头或认证材料。按真实功能向商店填写「网站内容」「浏览活动」「认证信息」等适用字段，即使处理位于本机。服务端或目标网盘作为下载来源处理自己的请求。扩展的桌面通信由浏览器 Native Messaging 到本机宿主，再经本机 socket 到桌面 App。目录解析、转存、下载可能由 App 与来源服务通信。

- 打开弹窗仅显示扩展身份及读取自身的下载权限。「检查连接」发送一帧宿主探针并读取上次同步记录。它不访问网页、请求权限或发送下载，也不会唤醒 App。
- 原有网页媒体识别可在允许站点中被动发现候选 URL 及媒体元数据，候选保存在浏览器会话。必要的请求头在后台内存按来源暂存。只有显式查看、检查、下载、网盘选择或认领的动作会走各自的 Native 业务链路。网盘认证材料按已授权选择或 App 认领范围在内存处理，与最终任务请求的字段边界不同。
- 点击「预览选中链接」才读取当前网页正文选区。只导入选区锚点和合法 HTTP(S) 文本，跨站链接以无认证方式导入。同源链接仅可带原下载观察中按原始 URL、标签和文档匹配的现有请求头。此操作不读取 Cookie store、不探测链接、不保存选区，也不暂停、取消或创建下载。只有用户在 App 勾选并确认后，所选链接才会成为任务。
- 普通接管的恢复日志保存浏览器下载标识、开始时间、URL 摘要、操作和任务标识及交接阶段。App 的任务记录可保存来源 URL 和下载请求所需的认证头，按任务生命周期管理。公开隐私政策应列明实际任务和凭据留存、删除方式、联系人、使用和共享限制，并对照最终源码及发布者实际运营。发布前将托管后的公开政策填入商店表单。
- 接管顺序为暂停浏览器 → 设置和能力检查 → 按偏好确认 → Prepare 留下暂停任务 → 取消匹配的浏览器下载 → 新查询证明同一项为 `interrupted / USER_CANCELED` → Commit 同一任务。已由浏览器完成的文件保留；未知 Prepare ACK 只查原 operationId，必要时归还给浏览器且迟到结果仅结束待接管任务。确认取消后的未知 Commit ACK 只查或重试同一任务。用户明确在 App 取消确认是单独结果，限制于匹配的下载项。

[Chrome 隐私字段与政策链接](https://developer.chrome.com/docs/webstore/cws-dashboard-privacy) · [Edge 发布表单与隐私政策](https://learn.microsoft.com/en-us/microsoft-edge/extensions/publish/publish-extension)

## 素材尺寸与状态

| 文件 | 规格 | 状态 |
| --- | --- | --- |
| `chrome-icon-128.png` | 128 × 128 | 现有 kownload 品牌图标的商店字段草稿。待确认正式上传包的图标及品牌。 |
| `edge-logo-300.png` | 300 × 300 | Edge 推荐 300 × 300，最小 128 × 128。既有品牌图标的尺寸草稿。 |
| `small-promo-440x280.png` | 440 × 280 | 共用宣传图草稿，待品牌与发布者复核。Edge 的该项为可选。 |
| `popup-fixture-1280x800.png` | 1280 × 800 | 浏览器连接阶段的弹窗草稿，使用两份模拟状态并带醒目标记。正式图在隔离 Chrome／Edge 以实际商店来源和配套 App 重拍，包含最终「预览选中链接」入口。 |

Chrome 截图可用 1280 × 800 或 640 × 400。Edge 截图可用 1280 × 800 或 640 × 480，最多六张。正式截图脚本建议顺序为无敏感信息的首次检查、成功身份匹配／同步、网页选区导入和 App 批量确认、普通下载确认。两边共用 1280 × 800 规格。 [Chrome 图片规范](https://developer.chrome.com/docs/webstore/images) · [Edge 图片和字段规范](https://learn.microsoft.com/en-us/microsoft-edge/extensions/publish/publish-extension)

## 发布前逐项核对

- [ ] 确认 Chrome 发布者账号、Edge Partner Center 账号及发布者联系信息有权使用。准备可公开访问的隐私政策、支持页、配套 App 的官方下载入口和审查者复测说明。
- [ ] 从两家控制台／实际安装详情记录各自真实 ID。未打包固定 ID 为 `jcalkpjhhaachliihlkojhmafniepnoj`。它由当前公开 `manifest.key` 得到，不能推出两家签发的 ID。按实际签发方式决定是否调整待上传 manifest 的公钥，复核相应浏览器中实际 ID。 [Chrome manifest key](https://developer.chrome.com/docs/extensions/reference/manifest/key) · [Edge Native Messaging](https://learn.microsoft.com/en-us/microsoft-edge/extensions/developer-guide/native-messaging)
- [ ] 将确认的商店来源纳入统一身份定义并重新生成／核对 Go origin 校验和 Flutter manifest 生成。分别查看实际登记的 Chrome、Edge 的新名称清单。使用 `jq -e --arg origin "chrome-extension://${store_id}/" '.allowed_origins | index($origin) != null' "$manifest_path"` 核对对应项。保留未打包和现有官方兼容所需的来源，逐项核查真实 Host 接受，白名单禁止通配。 [Chrome Native Messaging](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging) · [Edge Native Messaging](https://learn.microsoft.com/en-us/microsoft-edge/extensions/developer-guide/native-messaging)
- [ ] 在隔离且无真实账户的 Chrome／Edge 资料验证实际商店扩展 → 登记的本机 Host → 正确 App → 同步，并重拍真实截图。用受控普通下载验证同一个任务完成，复核拒绝权限、已有浏览器完成、显式取消和未知 ACK。用受控数据核对网页媒体与网盘隐私披露和权限依据。
- [ ] 核对正式 manifest、权限、商店表单、公开隐私政策和发行源码／第三方许可。配对的 App 如需更新须单独安排。
- [ ] 取得用户对正式商店提交和任何新增权限的明确授权，再提交。

安装操作见 [安装与首次连接](../browser-media-extension/README.md#安装与首次连接)。原生请求及交接契约见 [Native Messaging](NATIVE_MESSAGING.md)。
