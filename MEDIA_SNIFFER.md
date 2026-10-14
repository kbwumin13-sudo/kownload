# Gopeed 媒体嗅探

当前应用内核：`1.9.3-media.4`（配套插件 `0.3.1`）。Chrome／Edge 使用独立的 **Gopeed 媒体助手**，原有 Gopeed 下载接管插件可继续保留。最新使用后修复与安装验收见项目 `artifacts/repair-2026-10-13/验收报告.md`。

## 使用

1. 在 Chrome 打开 `chrome://extensions/`，在 Edge 打开 `edge://extensions/`。
2. 开启开发者模式，加载本仓库的 `browser-media-extension` 目录。扩展的固定 ID 为 `jcalkpjhhaachliihlkojhmafniepnoj`；应用启动时注册两个浏览器的 Native 主机许可。
3. 浏览页面后，视频旁显示下载入口，点击只处理这个播放器。来源不明确时，只允许确认已与该播放器关联的候选；未绑定的网络资源从工具栏面板手动查看。X 访客帖子页只有一个播放器时，可确认当前帖子视频。插件图标打开全页资源面板，支持视频、图片、音频筛选和明确多选批量下载。媒体检查后默认选择带音轨的最佳画质，也可以选择仅音频、名称和目录。
4. 链接过期时回到原页面重新捕获，在面板“更新暂停或失败的任务”中选择任务名称并继续。

设置中的“媒体下载”保存 yt-dlp、FFmpeg 的可执行路径和分片并发数。当前安装使用项目内固定的 yt-dlp 2026.08.19、yt-dlp-ejs 0.8.0、curl-cffi 0.16.3 与既有 FFmpeg 9.0.1；Node 使用绝对路径；普通直链使用原 HTTP 引擎，HLS／DASH 使用媒体协议。下载请求遵守 Gopeed 的代理设置，代理关闭时可直接下载。

支持常见点播、正常授权可访问的 AES-128 HLS、多清晰度和独立音视频合并。直播、DRM、无法追溯来源的 blob 数据不在首版支持范围内。

## 接口

- `POST /api/v1/media/inspect`：请求与 resolve 一致为 `{req, opts?}`，返回 `{title,thumbnail,duration,formats,subtitles,contextUrls,supported,reason?,isLive}`，外层使用现有 REST Result。
- 创建任务沿用 `POST /api/v1/tasks`，媒体请求显式设置 `req.protocol="media"`。不设置 protocol 时保留原路由。
- 媒体 extra：`header,cookies,scopedHeaders,format,title,sourcePage,sourceType,expectedMediaId,contextId,container,collectContext,subtitleLanguages,writeAutomaticSubtitles,embedSubtitles`。Cookie 包含域、路径、hostOnly 和 httpOnly；按域路径匹配，不把 Cookie 或 Authorization 当成跨域全局请求头。
- 暂停、继续、更新 URL 和删除沿用现有任务接口；`progress.mediaPhase` 区分下载与合并。重启保留分片及恢复元数据，合并成功才完成。
- Native `forward.params.path` 必须包含 `/api/v1/`。配套扩展统一转换；每次调用使用独立 `sendNativeMessage` 请求应答。

## 验证与回退

本地样本服务器：`python3 scripts/media-fixture-server.py --port 18794 --root ../artifacts/media-fixtures`。

安装后验收：`python3 scripts/media-native-acceptance.py --output ../artifacts/media-installed-final-acceptance`。

验证记录保存在项目根的 `artifacts/build`，最终安装验收结果保存在 `artifacts/media-installed-final-acceptance/acceptance.json`。Chrome 和 Edge 已实际加载并启用媒体助手，完成自动浮层、iframe 单项预选、Chrome 直链/HLS、Edge 图片批量/DASH/仅音频、合成 Cookie 与 Referer、应用自动唤醒及 SPA 切页验收。记录见 `artifacts/browser-acceptance/acceptance.json`。这些本地样本结果不代表所有网站均已验证。

升级前备份位置记录在 `artifacts/media-installed.json`。回退时退出 Gopeed，把备份内的 `Gopeed.app` 恢复至 `/Applications/Gopeed.app`，并禁用媒体助手；如需恢复安装时的数据，再恢复同一备份内的 `com.gopeed.gopeed`。数据备份目录仅本机用户可访问。

浏览器图标为“Gopeed 媒体助手”。每个播放器的入口自动显示，点击后只打开这个视频的下载界面，可选择清晰度。资源失效或未识别时不会回退到全页列表；图片和音频从工具栏资源面板筛选，批量下载需明确选择；本站关闭开关可暂停嗅探。回到动态页面后继续发现当前页面资源。


## 0.1.1 单视频下载修正（2026-10-05）

旧实现把播放器按钮和工具栏批量列表共用一个入口，只预选视频但仍显示整页资源。多播放器共用一个浮层，目标无法确定时也会打开整页列表。

现已按播放器独立绑定按钮，在点击时取得它的实际地址，后台生成含 tab、frame、document 和来源页的准确资源 ID。下载界面及 LIST/CREATE 请求均限定该 ID；页面导航使资源失效后，只提示重新捕获。无法关联的 blob 显示未识别，不会替用户选择其他资源。工具栏保留手动多选批量下载。

验收：27 项扩展测试通过。Chrome 多视频页面点击 A 只生成 A 的一个任务，iframe 单独点击只生成 iframe 的一个任务；Edge 点击 HLS B 只生成一个合并任务，界面保留清晰度选择。3 个输出均通过 ffprobe，均有音视频，时长约 8 秒，MP4 直链哈希与源文件一致。记录：`artifacts/browser-acceptance/single-target-acceptance.json`。本次未重新验收无关的内核生命周期与网站兼容性。


## 0.2.1 嗅探优化与站点适配

内核升级为 `1.9.3-media.2`，显式启用 Node 绝对路径；媒体检查按完整签名URL、授权上下文、页面代次及工具身份合并。默认元信息缓存30秒，明确格式ID和视频+音轨ID可从副本复用，复杂选择器仍独立解析；403、到期及上下文变化会重新检查。播放列表/多视频结果不会作为单视频任务提交。

插件采用增量发现、200节点初扫批次、open shadow root、可见播放器布局合并；后台按标签检查点持久化，慢任务创建不占用候选队列。普通TS网络分片不列为完整资源，明确播放器TS源保留。单视频确认、Cookie store/partition、清单引用分组与SPA往返隔离有专门回归。

用户明确选择“增强识别”后，页面脚本先确认启动，再开启60秒受限fetch/XHR观察；支持明确JSON相对URL与HLS/DASH引用。页面消息只生成候选，不直接触发下载。入口在多视频/引用视频无法确定归属时要求单项确认。

B站、X优先：B站公开6秒样本已通过安装后的Gopeed原生接口解析、下载与ffprobe音视频检查；X公开3.17秒样本亦已实际下载并检查两条轨道。无需为这些公开样本配置独立X CLI登录态。样本证据、测试日志与性能操作量比较位于 `artifacts/sniffer-optimization/`；不将公开样本通过扩展为所有账号内容均兼容。

更广的`pkg/download` race回归发现既有共享任务状态竞争；单独运行普通HTTP子用例仍可复现，未进入本次嗅探改动。媒体检查/缓存的race检查已通过。常规媒体生命周期功能回归通过，完整系统race不标为全绿。


## 0.2.2 已登录 Chrome X 应用中的单视频归属

本轮现场确认上一版仍有缺陷：X嵌套播放器容器被误算为多个视频，identify又接受了同文档未绑定资源。现统一播放器根节点，单项列表与任务接口必须明确绑定该播放器；共有video/1链接不作为多视频序号证据。

在用户已登录的Chrome安装应用X中完成两个独立播放器的单项界面检查、一个实际下载；仅创建一个与点击帖子一致的任务，约144.89秒、14354088字节，ffprobe含音视频。77项扩展测试通过。原有4个任务保留，验收任务已清理。证据：artifacts/x-player-binding-0.2.2。


## 0.3.0 整体嗅探优化（2026-10-06）

增强识别会话有独立 ID 与 60 秒期限；只有本次新发现的候选进入当前播放器确认页。明确 MSE 原始响应缓冲区→SourceBuffer→MediaSource→blob 的来源链可升级为准确归属，无法证明的候选仍需单选。新会话不会带入之前的全页网络资源，也不会默认替用户选中。常规播放不会启用页面网络 hook。hook 不复制分片内容，结束后恢复包装；转封装、复制后的缓冲区、开启前的来源、Worker/BYOB 及歧义链保持未绑定。

图片覆盖带括号/转义的 CSS url、image-set 字符串、伪元素、picture/currentSrc/srcset 与懒加载。面板显示实际备选地址，按用户选择下载，不将任意备选标作原图。元信息展示真实标题、封面、时长；媒体清晰度支持具体音轨语言和提取器提供的字幕选择，字幕默认关闭。字幕不存在、格式不支持或下载缺失会使任务失败，不能标记完成。普通 HTTP 直链页面上的独立 HTML track 目前不会自动交给字幕选择；页面提取器提供的字幕可下载并嵌入。

请求授权头只暂存在后台内存，按已观察 URL/域/路径传递，worker 重启后需要重新捕获。Cookie 按 store/partition、已确认引用域和所有相关路径组成 jar；不把 Cookie 头作为媒体全局参数。私密上下文以 0600 文件传给同一个 yt-dlp Python 环境，敏感值不进入命令行或日志。可选 curl-cffi 支持提取器要求的浏览器指纹，但重定向仍逐跳重算请求头和 Cookie。可信提取器为明确初始 API/CDN 请求设置的 Referer/User-Agent 及 API 授权头保留，scope 优先；重定向剥离后重新按目标授权。用户上下文不进入提取器全局默认头，Cookie、Proxy-Authorization 和默认授权不因这项兼容而放行。

已修复任务队列、序列化与 HTTP 状态快照的共享状态竞争；媒体更新只允许暂停/失败任务，格式或媒体身份改变时重新解析并隔离续传状态。验证包括完整 HTTP race、下载器定向 race、完整 media race、HLS fMP4、HLS ByteRange、DASH SegmentBase、字幕与请求头隔离。SegmentBase 当前以完整 BaseURL 下载成功，未宣称已有 SIDX 分段优化。

本轮源码、安装包、浏览器与实际文件证据保存于项目根 `artifacts/sniffer-system-0.3.0/`，最终结论以该目录验收报告为准。旧节记录描述历史版本，不能替代本轮结果。回退备份见该目录 `backup.json`；恢复应用与数据时使用同次备份。直播、DRM 保持原支持边界；Vimeo 当前样本明确要求登录，未宣称已通过。
