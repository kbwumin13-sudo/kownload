#!/usr/bin/env swift
// Export the selected B artwork into native application and template resources.
import AppKit
import Foundation

let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
let files = FileManager.default
let brand = root.appendingPathComponent("docs/brand")
let assets = root.appendingPathComponent("ui/flutter/assets")
let artwork = NSImage(contentsOf: brand.appendingPathComponent("app-icon-artwork.png"))!
let templateArtwork = NSImage(contentsOf: brand.appendingPathComponent("menubar-artwork.png"))!

func export(size: Int, template: Bool = false, to url: URL) throws {
  let bitmap = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: size, pixelsHigh: size,
    bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
    colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0)!
  let context = NSGraphicsContext(bitmapImageRep: bitmap)!
  NSGraphicsContext.saveGraphicsState()
  NSGraphicsContext.current = context
  context.imageInterpolation = .high
  let bounds = NSRect(x: 0, y: 0, width: size, height: size)
  if template {
    // AppKit consumes the alpha silhouette, including its antialiased boundary.
    templateArtwork.draw(in: bounds, from: .zero, operation: .sourceOver, fraction: 1)
    NSColor.black.setFill()
    bounds.fill(using: .sourceAtop)
  } else {
    // Native icon export uses a clean continuous corner mask, excluding the
    // concept image's studio backdrop. The selected artwork itself is retained.
    let s = CGFloat(size) / 1254
    let b = NSRect(x: 140 * s, y: 154 * s, width: 970 * s, height: 960 * s)
    let r = 245 * s
    let c = 48 * s
    let path = NSBezierPath()
    path.move(to: NSPoint(x: b.minX + r, y: b.minY))
    path.line(to: NSPoint(x: b.maxX - r, y: b.minY))
    path.curve(to: NSPoint(x: b.maxX, y: b.minY + r),
      controlPoint1: NSPoint(x: b.maxX - c, y: b.minY),
      controlPoint2: NSPoint(x: b.maxX, y: b.minY + c))
    path.line(to: NSPoint(x: b.maxX, y: b.maxY - r))
    path.curve(to: NSPoint(x: b.maxX - r, y: b.maxY),
      controlPoint1: NSPoint(x: b.maxX, y: b.maxY - c),
      controlPoint2: NSPoint(x: b.maxX - c, y: b.maxY))
    path.line(to: NSPoint(x: b.minX + r, y: b.maxY))
    path.curve(to: NSPoint(x: b.minX, y: b.maxY - r),
      controlPoint1: NSPoint(x: b.minX + c, y: b.maxY),
      controlPoint2: NSPoint(x: b.minX, y: b.maxY - c))
    path.line(to: NSPoint(x: b.minX, y: b.minY + r))
    path.curve(to: NSPoint(x: b.minX + r, y: b.minY),
      controlPoint1: NSPoint(x: b.minX, y: b.minY + c),
      controlPoint2: NSPoint(x: b.minX + c, y: b.minY))
    path.close()
    path.addClip()
    NSColor.white.setFill()
    bounds.fill()
    artwork.draw(in: bounds, from: .zero, operation: .sourceOver, fraction: 1)
  }
  NSGraphicsContext.restoreGraphicsState()
  try files.createDirectory(at: url.deletingLastPathComponent(), withIntermediateDirectories: true)
  try bitmap.representation(using: .png, properties: [:])!.write(to: url)
}

let catalog = root.appendingPathComponent("ui/flutter/macos/Runner/Assets.xcassets/AppIcon.appiconset")
for size in [16, 32, 64, 128, 256, 512, 1024] {
  try export(size: size, to: catalog.appendingPathComponent("app_icon_\(size).png"))
}
for (name, size) in [("icon_1024.png", 1024), ("icon_512.png", 512), ("icon_macos_1024.png", 1024)] {
  try export(size: size, to: assets.appendingPathComponent("icon/\(name)"))
}
try export(size: 1024, to: brand.appendingPathComponent("app-icon.png"))
try export(size: 180, template: true, to: assets.appendingPathComponent("tray_icon/icon_mac.png"))
try export(size: 180, template: true, to: brand.appendingPathComponent("menubar-template.png"))
try export(size: 256, to: assets.appendingPathComponent("tray_icon/icon.png"))

// ICO supports PNG image entries. Keep these exports dependency-free as well.
func u16(_ value: UInt16) -> Data {
  var v = value.littleEndian
  return withUnsafeBytes(of: &v) { Data($0) }
}
func u32(_ value: UInt32) -> Data {
  var v = value.littleEndian
  return withUnsafeBytes(of: &v) { Data($0) }
}
let sizes = [16, 32, 64, 128, 256]
let pngs = try sizes.map { try Data(contentsOf: catalog.appendingPathComponent("app_icon_\($0).png")) }
var ico = u16(0) + u16(1) + u16(UInt16(sizes.count))
var offset = 6 + sizes.count * 16
for (size, png) in zip(sizes, pngs) {
  let dimension = UInt8(size == 256 ? 0 : size)
  ico.append(contentsOf: [dimension, dimension, 0, 0])
  ico.append(u16(1)); ico.append(u16(32))
  ico.append(u32(UInt32(png.count))); ico.append(u32(UInt32(offset)))
  offset += png.count
}
for png in pngs { ico.append(png) }
for name in ["icon/icon.ico", "tray_icon/icon.ico"] {
  try ico.write(to: assets.appendingPathComponent(name))
}
let master = try Data(contentsOf: brand.appendingPathComponent("app-icon.png"))
let svg = """
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" width="1024" height="1024" viewBox="0 0 1024 1024"><image width="1024" height="1024" xlink:href="data:image/png;base64,\(master.base64EncodedString())"/></svg>
"""
try svg.write(to: assets.appendingPathComponent("icon/icon.svg"), atomically: true, encoding: .utf8)
print("Exported B application icons and the monochrome menu-bar template")
