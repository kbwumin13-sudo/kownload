# kownload Design

## Source of truth
Active，2026-10-26，第二轮精修。视觉依据为用户提供的觅迹截图与本机 Miji MainView；用户反馈首版仍粗糙、缺少 Apple 感，要求进一步打磨材质、比例与层次。保留输入行、轻量筛选、紧凑文件列表、底部状态栏；管理功能为同窗口设置。

## Brand
kownload，小写。原生、轻、直接；保留小图标、标题、大输入框和清楚层次。Gopeed及适配上游的名称保留在来源与许可说明中。

2026-10-15 用户选定 B 图标：白色圆角底板与翡翠绿色折带小写 k。应用和 GitHub 使用同一图稿；菜单栏采用对应的单色 k 轮廓，以系统模板图像适配浅色、深色及选中状态。资源与导出方式见 docs/BRAND.md。

## Product goals
快速添加下载；保留全部既有后端能力、任务与配置；菜单栏常驻。隐藏不取消下载，不丢失草稿或网盘登录等待。

## Personas and jobs
非开发者 macOS 用户。快捷键粘贴链接，回车加入下载；种子/多文件/网盘按需确认，任务可暂停恢复。设置只需说明保存位置、下载行为与功能接入。

## Information architecture
面板→输入→状态筛选与紧凑任务→底部操作提示。管理导航收敛为下载列表、通用、下载。移除高级页、扩展列表/商店/维护、脚本、API/监听和协议调优；底层能力与已有配置保留。首次提交下载弹出简短教程，确认保存位置并说明浏览器、视频、网盘使用方式；成功配置后持久化完成标记，取消或失败不标记。通用设置可重新打开。旧 connections / advanced 入口回通用。

## Design principles
以觅迹的层级、留白与低装饰感为准；简短标题、无大面积品牌色、无卡片套卡片。系统材质决定光学效果；复杂度随实际下载操作展开。

## Visual language
由 AppKit 提供窗口材质，Flutter 仅加轻 tint，不用接近不透明的底色遮住原生材质；减少透明度时改用实色。22pt主圆角、细轮廓和自然窗口阴影；主面板640pt宽，输入20pt、任务正文13pt、次级信息11–12pt，任务行58pt。Apple系统字体贯穿主题、输入及换行测量；浅色 #F5F5F7，深色 #1D1D1F，蓝色 #007AFF / #0A84FF。按钮28pt高、6pt圆角，蓝色用于进度和有输入时的下载动作；导航与面板筛选使用淡中性色。设置采用560pt宽的分组表单，10pt圆角及0.5pt分隔线，常规行46pt；开关缩小视觉但保留44×32pt点击区域。首次教程440pt宽，文字17/12pt。保留品牌图标。

## Components
MacPanel掌管呈现与快捷键；CreateView共享创建流程；MacShell与SettingView组织设置；MacTaskListController共用任务状态。

## Accessibility
完整键盘焦点与中文输入；Esc尊重输入法、子菜单和sheet；系统减少透明度/动态效果；保留通知和主题选项。

## Responsive behavior
面板高度随任务/高级流程展开，保持屏幕可见区域内；设置恢复常规窗口尺寸，单引擎单窗口。

## Interaction states
⌃⌥D切换面板。成功确认后0.8秒收起，新输入取消；失败保留输入并就近显示原因。隐藏保留当前工作流；设置不随失焦关闭。

## Content voice
简短中文，按用户目的命名；不要求用户理解扩展、Node、User-Agent、Tracker、API 或监听地址。必要故障原因仍可查看。

## Implementation constraints
沿用Flutter+AppKit+Go；保留com.gopeed.gopeed、gopeed scheme和浏览器Native host兼容标识。公开仓库仅源码与可重现构建资料；真实账号数据、测试下载与本机产物保留项目artifacts。

## Open questions
无待定产品决策。Claude 正在修复内核与窗口问题；本轮修改展示与设置入口，以隔离数据验收候选应用，正式安装与修复版本汇合。真实网盘验收状态参见 NETDISK_PROVIDERS.md 及项目协作.md。
