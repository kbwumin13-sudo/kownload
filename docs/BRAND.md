# kownload 品牌图标

kownload 采用已选定的 B 方案：白色圆角底板，搭配翡翠绿色折带构成的小写 **k**。应用与 GitHub 产品介绍使用同一图标。

<p align="center">
  <img src="brand/app-icon.png" width="160" height="160" alt="kownload B 方案：白色圆角底板与翡翠绿色折带小写 k">
</p>

| 资源 | 用途 |
| :-- | :-- |
| [app-icon.png](brand/app-icon.png) | 应用图标与 GitHub README 展示；底板以外保持透明 |
| [menubar-template.png](brand/menubar-template.png) | 菜单栏单色模板图标；透明背景，保留小写 k 的轮廓 |

菜单栏使用模板图标，由 macOS 随系统浅色、深色外观自动调整显示颜色，保持清晰可辨。翡翠绿色用于应用图标，菜单栏由系统着色。

图标保留透明留白。菜单栏使用纯黑轮廓的透明 PNG，并以 `isTemplate: true` 注册，由系统决定实际显示颜色；不将彩色底板缩成菜单栏图标。

原始选定图稿保存在本目录 `app-icon-artwork.png`，单色图稿为 `menubar-artwork.png`。在 macOS 仓库根目录运行 `swift scripts/export-brand-icons.swift`，可用 AppKit 重新导出透明圆角 PNG、七种应用图标分辨率、ICO 与自包含 SVG。应用位图保留 B 图稿的颜色和折带质感；SVG 是嵌入该位图的展示封装。
