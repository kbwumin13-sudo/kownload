# kownload Design

## Source of truth
Active，2026-10-14。用户确认的前景卡片参考图：用户提供的下载管理器前景卡片样图。日常入口为轻面板，管理功能为同窗口设置。

## Brand
kownload，小写。原生、轻、直接；保留小图标、标题、大输入框和清楚层次。Gopeed及适配上游的名称保留在来源与许可说明中。

2026-10-15 用户选定 B 图标：白色圆角底板与翡翠绿色折带小写 k。应用和 GitHub 使用同一图稿；菜单栏采用对应的单色 k 轮廓，以系统模板图像适配浅色、深色及选中状态。资源与导出方式见 docs/BRAND.md。

## Product goals
快速添加下载；保留全部既有后端能力、任务与配置；菜单栏常驻。隐藏不取消下载，不丢失草稿或网盘登录等待。

## Personas and jobs
本机macOS用户。快捷键粘贴链接，回车加入下载；种子/多文件/网盘按需确认，任务可暂停恢复。

## Information architecture
面板→输入与紧凑操作→最多3条任务。设置：任务、通用、下载、连接与能力、高级。扩展商店仅在高级维护中按需打开。

## Design principles
以用户样图前景卡片的气质为准，合并重复入口；系统材质决定光学效果；复杂度随操作展开。

## Visual language
完整AppKit regular玻璃，22pt主圆角，自然窗口阴影；主面板640pt宽。Apple系统字体，标题20pt，输入18pt，正文12–13pt，统一操作尺寸。无全窗口染色渐变，无层叠玻璃底板。

## Components
MacPanel掌管呈现与快捷键；CreateView共享创建流程；MacShell与SettingView组织设置；MacTaskListController共用任务状态。

## Accessibility
完整键盘焦点与中文输入；Esc尊重输入法、子菜单和sheet；系统减少透明度/动态效果；保留通知和主题选项。

## Responsive behavior
面板高度随任务/高级流程展开，保持屏幕可见区域内；设置恢复常规窗口尺寸，单引擎单窗口。

## Interaction states
⌃⌥D切换面板。成功确认后0.8秒收起，新输入取消；失败保留输入并就近显示原因。隐藏保留当前工作流；设置不随失焦关闭。

## Content voice
简短中文，功能名称直陈；技术配置仅放高级页。

## Implementation constraints
沿用Flutter+AppKit+Go；保留com.gopeed.gopeed、gopeed scheme和浏览器Native host兼容标识。公开仓库仅源码与可重现构建资料；真实账号数据、测试下载与本机产物保留项目artifacts。

## Open questions
无待定产品决策。真实网盘验收状态参见NETDISK_PROVIDERS.md及项目协作.md。
