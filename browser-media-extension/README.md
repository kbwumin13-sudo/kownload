# kownload 下载助手

Chrome / Edge MV3 下载助手，支持网页媒体和九类网盘的所选文件。不申请 `downloads` 或下载拦截权限。Gopeed 原有浏览器插件可同时保留。

## 0.4.0 九网盘

支持 `baidu aliyun mcloud tcloud xunlei quark uc 123pan guangya`，依次为百度、阿里、中国移动、天翼、迅雷、夸克、UC、123、光鸭。沿用公开 manifest key 和固定扩展 ID，不改变媒体 `content.js`。

在网盘页面勾选文件，点击页面右下角统一的 **kownload 下载**。助手唤醒 App，调用 `/netdisk/browser/capture` 建立内存 session，然后用非 silent Native `create` 打开 Flutter 网盘确认页与保存位置。只提交当前选择的文件/目录，不展开目录，不把整个列表或单文件分享自动视为已选。实际 prepare、转存和任务创建由 App 确认流程完成；接口见项目 `NETDISK.md`。

App 发起网页同步时，顶层 `document_start` 引导脚本在页面可见时每 15 秒探测一次。每次先 Native `ping`，App 未运行直接结束；运行中仅以固定 `/netdisk/browser/claim` 路径检查待请求。claim 成功后采集并带 requestId/nonce 调用 capture；此流程没有 wakeup/create。nonce 不进入页面 URL、DOM 或 MAIN 参数。Native Port 仅在单次探测/采集期间存活，完成、导航或 45 秒超时即断开。

`netdisk-page.js` 通过 `chrome.scripting` 在指定顶层 document 的 MAIN world 执行，只返回固定文件字段、已有认证字段和分享信息。选择布局来自固定 LinkSwift stable/dev 源码：Vue 选中列表、React 已提交 props 的选择键、显式选中的 DOM 行。页面没有 postMessage API/Cookie 代理，也没有任意脚本执行入口；按钮仅接受真实点击。后台核对扩展 sender、标签、top frame、document、当前 URL、provider 和页面代次；SPA/hash/完整导航使旧采集失效。

Cookie 仅在获得 claim 或按钮点击后，由后台按标签所属 cookie store、当前 document partition 和固定网盘域范围读取，保留路径与过期时间。原始 Cookie/token、每文件 share_fid_token 只在内存传往 Native capture；Native create 使用过滤认证信息后的所选文件。扩展 storage 只新增非敏感的随机 instance ID，错误提示使用固定中文文案，不包含服务端原始错误、登录字段或签名 URL。

本地 mock 覆盖九类选择映射、无选择/单文件分享、跨页面失效、claim 不唤醒、所选资源 Native 提交、store/partition 与敏感字段过滤。未执行真实网盘登录、API 下载、安装或 Flutter 实机验收；页面布局改版和当前账号权限仍需后续实际确认。助手只读取现有登录/分享状态，不刷新令牌、不写站点 storage；缺少有效凭证时需在原站完成登录或打开已解锁分享后重试。

选择布局参考 [hmjz100/LinkSwift](https://github.com/hmjz100/LinkSwift)，固定原件在上级 `artifacts/linkswift-2026-10-14/upstream/LinkSwift-{stable,dev}.user.js`，保留 AGPL-3.0-or-later 归属。未复制上游 API 下载、自动登录/令牌刷新或修改原站界面的逻辑。

交接字段约定：Capture 的 `tokens` 使用原站字段名。百度保留 `bdstoken/sign/timestamp/jsToken/sekey/shareid`（数值 shareid 保留在 tokens，URL 分享身份在 share.shareId）；阿里保留 `access_token/token_type/user_id/default_drive_id/resource_drive_id`，文件 driveId 在 meta；天翼为 `accessToken`；迅雷/光鸭为 `access_token/token_type/user_id/client_id/captcha_token/deviceid`；夸克/UC 为 `stoken` 和每文件 `meta.share_fid_token`；123 为 `authorToken/LoginUuid`、文件 `hash`（Etag）及 `meta.S3KeyFlag/StorageNode`。移动云盘读取后台 Cookie 中的 authorization。已有 access_token/authorToken 自动生成 Capture.headers.Authorization，设备/验证码/客户端字段只生成固定对应 Header；这些头只存在于 capture，不能进入 create 的 task extra。

## 加载

1. 在 Chrome 打开 `chrome://extensions`，或在 Edge 打开 `edge://extensions`。
2. 开启开发者模式，选择“加载已解压的扩展程序”。
3. 选择本目录 `browser-media-extension/`。弹出面板会展示当前页视频、图片和音频。
4. 下载前确保 Gopeed 已安装并注册 Native Messaging 主机 `com.gopeed.gopeed`。

固定扩展 ID：`jcalkpjhhaachliihlkojhmafniepnoj`。Chrome 和 Edge 使用同一个 `manifest.key`，Native Messaging 主机的 `allowed_origins` 需加入 `chrome-extension://jcalkpjhhaachliihlkojhmafniepnoj/`。此公开 key 只用于固定 ID；目录中没有私钥。Main 可用该 ID 在 Chrome / Edge 实际安装验收。

面板默认隐藏宽、高都已知且小于 128 像素的图片；未知尺寸保留。“显示小图”可切换。选择媒体清单后会检查格式并默认选中最高画质的带音轨组合；每档单独视频轨会自动配音轨，保留仅音频和原有含音轨格式。暂停或失败任务可按当前资源来源页与协议查找，选择任务名称后更新资源地址并继续。每个可见播放器拥有自己的下载按钮；点击后只显示该播放器对应的一个视频，可选择清晰度、名称和目录。单视频界面不提供全选或其他资源，来源失效后提示回原页面重新点击。`blob:` 只有同一播放器显式提供清单地址时才关联，否则显示“未识别这个视频”并禁用下载。整页视频、图片、音频列表与批量下载仅通过工具栏图标进入，默认不选中任何资源。分片不会作为完整视频提交。

面板首次读取默认目录或用户点击检查、下载时才唤醒 Gopeed；被动嗅探不会唤醒。Cookie 只在点击所选资源时按资源 URL 获取并传给 Native 主机，候选状态只存 `storage.session`。站点关闭状态和“显示小图”偏好存在 `storage.local`。

Native `forward` 会统一把 `/config`、`/media/inspect`、`/tasks` 等简写路径补成 `/api/v1/...`，已带此前缀的路径保持原样。

本地检查：在此目录运行 `node --test`，并用 `node --check` 检查 JavaScript 文件。


## 0.2.0 优化

采用增量节点扫描、200节点分批初扫和可见播放器布局更新，支持open shadow root及匹配来源的about:blank/srcdoc iframe。候选在后台内存按完整URL与文档索引，按标签页批量保存到storage.session；导航、下载和任务确认强制检查点。每标签最多1000候选、全局最多3000，并有保守字节上限。普通页面切换用页面代次隔离，旧任务或解析结果不会写回新页面。

格式检查按完整签名URL、登录上下文和页面代次合并；短期缓存供重开面板和已确认格式选择复用。清单引用按明确元数据分组，不按名称/目录猜测；网络TS分片不作为完整视频列出，明确播放器源仍支持独立TS。

未识别blob入口只显示已明确绑定此播放器或当前单视频帖子的候选；未绑定的同文档网络资源仅在工具栏面板展示。“增强识别这个视频”需要页面脚本确认启动，在60秒内观察受限清单或播放器JSON。每响应最多1MiB，总读取最多4MiB，最多64地址，不捕获大媒体、分片或密钥数据，也不自动下载。脚本更新后需刷新来源页，晚开启需重新播放。明确主播放器内的YouTube watch页和Bilibili视频页可交给yt-dlp单条提取器；广告和列表页不采用页面回退，多视频结果由内核拒绝。

Cookie按当前标签所属store及frame partition读取，查不到store时拒绝提交，不回退其他窗口。子清单Cookie只按已确认引用URL的域和路径加入每任务cookie jar。Node使用配置的绝对路径显式启用。


0.2.1 增加 X/Twitter 单帖子适配：优先视频所在 article 的状态链接，显式引用视频及无法确定序号的多视频帖子保持手动确认。信息流中按钮不会把整个feed交给提取器。增强搜索需页面脚本确认，支持配置响应中明确的相对清单URL，超大块计入会话预算。


## 0.2.2 Chrome 已登录 X 应用修复

已登录 X 同一个播放器使用嵌套的 videoPlayer/videoComponent，统一按外层播放器归属，避免被当作多个视频而回退识别。未挂载video的包装迁移保持一个按钮与身份。多视频卡片的共有时间链接即使带video/1也不用于推断被点击序号；只有包围当前播放器的明确媒体链接可指定序号。

identify列表和检查/创建/更新任务接口都要求明确playerId或manualFor绑定，并在分组前筛选。点击来源不明的播放器不再列出同页其他未绑定视频；工具栏仍支持显式批量选择。

77项回归通过。在用户已登录的Chrome X安装应用窗口，分别点击两个信息流播放器，均打开single模式且各只有自身一项；第二项经UI下载仅新增一个对应帖子任务，ffprobe确认音视频，随后清理此验收任务，保留原有4项。详细结果见项目根artifacts/x-player-binding-0.2.2。


## 0.3.0

增强识别只收集当前 60 秒会话的新候选；精确 MSE 来源证据可绑定播放器，其他新候选需要手动确认一项。支持受限 JSON 配置观察与无扩展名清单，不扫描密钥正文或复制媒体缓冲区。CSS 背景括号/转义、image-set、伪元素、srcset/picture 与动态图片的识别已补全。清晰度选择保留音轨语言，提取器提供的字幕可选下载并嵌入；面板显示真实标题、时长、封面和图片备选。

授权头仅在后台内存按目标限定，Cookie 按确认引用域/path 读取；会话检查点不会写入敏感头。普通下载接管仍由官方扩展处理。更新源码后在扩展管理页重新加载，再刷新已打开的来源页（包括 Chrome 安装的 X 应用）。详细能力、安装验收与边界见仓库 MEDIA_SNIFFER.md 和项目 artifacts/sniffer-system-0.3.0/验收报告.md。
