import Cocoa
import Carbon
import FlutterMacOS

// Native behavior harness: retain real host code and AppKit, never start an
// application Flutter engine or register the production plugins.
func RegisterGeneratedPlugins(registry: FlutterPluginRegistry) {}

private final class DropTarget: NSView {}
private final class InputView: NSView {
  override var acceptsFirstResponder: Bool { true }
}
private final class RenderView: NSView {
  override var isFlipped: Bool { true }
}
private final class InputController: NSViewController {
  var received: [NSEvent.EventType] = []
  let renderView = RenderView()
  override func loadView() {
    // Match FlutterViewController's unflipped wrapper and its autoresizing,
    // flipped, layer-backed FlutterView without starting an engine.
    view = InputView()
    renderView.wantsLayer = true
    renderView.autoresizingMask = [.width, .height]
    view.addSubview(renderView)
  }
  override func mouseDown(with event: NSEvent) { received.append(event.type) }
  override func mouseDragged(with event: NSEvent) { received.append(event.type) }
  override func mouseUp(with event: NSEvent) { received.append(event.type) }
  override func rightMouseDown(with event: NSEvent) { received.append(event.type) }
  override func rightMouseUp(with event: NSEvent) { received.append(event.type) }
  override func otherMouseDown(with event: NSEvent) { received.append(event.type) }
  override func otherMouseUp(with event: NSEvent) { received.append(event.type) }
  override func scrollWheel(with event: NSEvent) { received.append(event.type) }
}
private final class TestWindow: MainFlutterWindow {
  var testKey = true
  var testSheet: NSWindow?
  override var isKeyWindow: Bool { testKey }
  override var attachedSheet: NSWindow? { testSheet }
}

@main
struct PanelHostTest {
  static func drain() { RunLoop.current.run(until: Date(timeIntervalSinceNow: 0.04)) }
  static func main() {
    let metadataFixture = Data(#"{"profile":{"info_cache":{"Default":{"name":"学习","user_name":"private@example.test"},"Profile 2":{"name":"工作"},"../unsafe":{"name":"bad"}}},"os_crypt":{"encrypted_key":"secret-fixture"},"tokens":{"value":"secret-fixture"}}"#.utf8)
    let metadata = try! MainFlutterWindow.browserProfileMetadata(metadataFixture)
    precondition(metadata.contains("学习") && metadata.contains("工作"))
    precondition(!metadata.contains("secret-fixture") && !metadata.contains("private@example.test") && !metadata.contains("../unsafe"))
    do {
      _ = try MainFlutterWindow.browserProfileMetadata(Data("[]".utf8))
      preconditionFailure("Invalid metadata must be rejected")
    } catch {}
    print("PASS: browser profile metadata excludes credentials and rejects invalid data")
    _ = NSApplication.shared
    NSApp.setActivationPolicy(.accessory)
    let window = TestWindow(contentRect: NSRect(x: 0, y: 0, width: 1100, height: 720),
      styleMask: [.titled, .resizable, .closable], backing: .buffered, defer: true)
    window.isReleasedWhenClosed = false
    let metadataDirectory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    try! FileManager.default.createDirectory(at: metadataDirectory, withIntermediateDirectories: true)
    let metadataURL = metadataDirectory.appendingPathComponent("Local State")
    try! metadataFixture.write(to: metadataURL)
    let oldBookmarks = UserDefaults.standard.dictionary(forKey: "netdiskProfileFileBookmarks")
    UserDefaults.standard.set([metadataURL.path: Data("obsolete-bookmark".utf8)], forKey: "netdiskProfileFileBookmarks")
    var recoveredMetadata: Any?
    window.handleBrowserProfileCall(FlutterMethodCall(methodName: "read", arguments: ["path": metadataURL.path])) { recoveredMetadata = $0 }
    precondition((recoveredMetadata as? String)?.contains("学习") == true, "Obsolete bookmarks must not block OS-permitted reads")
    UserDefaults.standard.set(oldBookmarks, forKey: "netdiskProfileFileBookmarks")
    try! FileManager.default.removeItem(at: metadataDirectory)
    print("PASS: obsolete profile bookmarks preserve permitted metadata reads")
    let input = InputController()
    window.installContentController(input)
    var events: [(String, Any?)] = []
    window.sendPanelEvent = { events.append(($0, $1)) }
    window.observePresentationEvents()
    @discardableResult func call(_ method: String, _ arguments: Any? = nil) -> Any? {
      var called = false
      var value: Any?
      window.handlePanelCall(FlutterMethodCall(methodName: method, arguments: arguments)) { value = $0; called = true }
      precondition(called, "Every method must complete its Flutter result")
      return value
    }
    func appearance(_ method: String, _ arguments: Any? = nil) {
      var called = false
      window.handleAppearanceCall(FlutterMethodCall(methodName: method, arguments: arguments)) { _ in called = true }
      precondition(called)
    }
    func geometry(_ stage: String, width: CGFloat, height: CGFloat) {
      // A frame-only check passes before AppKit's next layout, which used to
      // collapse content moved out of NSGlassEffectView to zero size.
      let root = window.contentView!
      root.layoutSubtreeIfNeeded()
      drain()
      root.layoutSubtreeIfNeeded()
      let expected = NSRect(x: 0, y: 0, width: width, height: height)
      func equal(_ actual: NSRect, _ expected: NSRect, _ label: String) {
        precondition(abs(actual.minX - expected.minX) < 0.01 &&
                     abs(actual.minY - expected.minY) < 0.01 &&
                     abs(actual.width - expected.width) < 0.01 &&
                     abs(actual.height - expected.height) < 0.01,
                     "\(stage) \(label): \(actual), expected \(expected)")
      }
      for (label, view) in [("root", root), ("surface", root.subviews.first!),
                            ("wrapper", input.view), ("render", input.renderView)] {
        equal(view.bounds, expected, "\(label) bounds")
        equal(view.frame, expected, "\(label) frame")
        equal(view.convert(view.bounds, to: nil), expected, "\(label) window coordinates")
        equal(view.visibleRect.intersection(view.bounds), expected, "\(label) visible content")
      }
      // Flutter's y=0 is at the top. A representative semantic/text rectangle
      // must remain inside the window, including after the titlebar returns.
      let textRect = NSRect(x: 24, y: 18, width: 120, height: 28)
      let textInWindow = input.renderView.convert(textRect, to: nil)
      equal(textInWindow, NSRect(x: 24, y: height - 46, width: 120, height: 28), "flipped text")
      let contentOnScreen = window.convertToScreen(expected)
      precondition(contentOnScreen.contains(window.convertToScreen(textInWindow)), "\(stage) text outside content")
      precondition(window.frame.contains(contentOnScreen), "\(stage) content outside window")
    }
    precondition(call("setPresentation", ["mode": "panel"]) == nil)
    geometry("initial panel", width: 640, height: 260)
    precondition(window.styleMask == .borderless && window.level == .floating)
    precondition(window.canBecomeKey && !window.canBecomeMain)
    precondition(window.contentView!.bounds.size == NSSize(width: 640, height: 260))
    precondition(window.minSize == .zero && window.contentMinSize == .zero)
    precondition(window.toolbar == nil && window.titleVisibility == .hidden && window.hasShadow)
    precondition(window.collectionBehavior.contains([.canJoinAllSpaces, .fullScreenAuxiliary]))
    precondition(!window.collectionBehavior.contains(.fullScreenPrimary))
    if #available(macOS 13.0, *) { precondition(window.collectionBehavior.contains(.canJoinAllApplications)) }
    let container = window.contentViewController!
    let root = window.contentView!
    precondition(container.children.count == 1 && container.children[0] === input)
    if #available(macOS 26.0, *) {
      window.rebuildSurface(reduceTransparency: false)
      let glass = root.subviews.first as! NSGlassEffectView
      precondition(root.subviews.count == 1 && glass.contentView === input.view)
      precondition(input.view.isDescendant(of: glass) && glass.style == .regular && glass.cornerRadius == 22)
      geometry("glass panel", width: 640, height: 260)
    }
    window.rebuildSurface(reduceTransparency: false, supportsGlass: false)
    let fallback = root.subviews.first as! NSVisualEffectView
    precondition(fallback.material == .popover && input.view.superview === fallback)
    geometry("glass to fallback", width: 640, height: 260)
    window.rebuildSurface(reduceTransparency: true)
    let solid = root.subviews.first!
    precondition(!(solid is NSVisualEffectView) && solid.layer?.backgroundColor?.alpha == 1)
    precondition(solid.layer?.cornerRadius == 22 && input.view.superview === solid)
    geometry("fallback to solid", width: 640, height: 260)
    window.rebuildSurface(reduceTransparency: false)
    geometry("solid to glass", width: 640, height: 260)

    // Same DropTarget and controller through the new glass hierarchy.
    let drop = DropTarget(frame: input.view.bounds)
    drop.autoresizingMask = [.width, .height]
    drop.registerForDraggedTypes([.fileURL])
    input.view.addSubview(drop)
    func mouse(_ type: NSEvent.EventType, _ point: NSPoint = NSPoint(x: 320, y: 130)) -> NSEvent {
      NSEvent.mouseEvent(with: type, location: point, modifierFlags: [], timestamp: 1,
        windowNumber: window.windowNumber, context: nil, eventNumber: 1, clickCount: 1, pressure: 1)!
    }
    for sequence: [NSEvent.EventType] in [[.leftMouseDown, .leftMouseDragged, .leftMouseUp],
                                        [.rightMouseDown, .rightMouseUp], [.otherMouseDown, .otherMouseUp]] {
      sequence.forEach { window.sendEvent(mouse($0)) }
      if Array(input.received.suffix(sequence.count)) != sequence {
        let point = root.convert(NSPoint(x: 320, y: 130), from: nil)
        let diagnostic = "pointer hit=\(String(describing: root.hitTest(point))) root=\(root.frame) input=\(input.view.frame) drop=\(drop.frame) received=\(input.received)\n"
        FileHandle.standardError.write(Data(diagnostic.utf8))
      }
      precondition(Array(input.received.suffix(sequence.count)) == sequence)
    }
    let escape = NSEvent.keyEvent(with: .keyDown, location: .zero, modifierFlags: [], timestamp: 1,
      windowNumber: window.windowNumber, context: nil, characters: "\u{1b}", charactersIgnoringModifiers: "\u{1b}",
      isARepeat: false, keyCode: 53)!
    precondition(!window.performKeyEquivalent(with: escape), "Escape must continue to Flutter/IME")

    // Legacy appearance/layout commands must not change panel geometry/style.
    appearance("windowMinimum")
    appearance("toolbar", ["section": "任务", "dark": true, "modal": true])
    appearance("sidebar", ["width": 208])
    appearance("glassRegions", [["id": 1, "x": 0, "y": 0, "width": 640, "height": 260]])
    appearance("windowTint", ["top": 0xff000000, "bottom": 0xffffffff])
    precondition(root.subviews.count == 1 && window.toolbar == nil)
    precondition(window.minSize == .zero && window.contentMinSize == .zero && window.styleMask == .borderless)
    precondition(window.title == "kownload")
    precondition(window.firstResponder === input.view)
    precondition(call("resizePanel", ["height": 330]) == nil)
    precondition(root.bounds.size == NSSize(width: 640, height: 330) && drop.bounds.size == root.bounds.size)
    geometry("panel resize", width: 640, height: 330)
    precondition(call("setPresentation", ["mode": "settings", "width": 920, "height": 650, "show": false]) == nil)
    geometry("panel to settings", width: 920, height: 650)
    precondition(window.contentViewController === container && container.children[0] === input)
    precondition(window.styleMask == [.titled, .closable, .resizable, .miniaturizable])
    precondition(window.level == .normal && window.canBecomeMain && window.titleVisibility == .visible)
    precondition(window.contentMinSize == NSSize(width: 800, height: 560) && window.toolbar == nil)
    precondition(window.collectionBehavior.contains(.fullScreenPrimary) && !window.collectionBehavior.contains(.canJoinAllSpaces))
    precondition(window.title == "kownload — 设置")
    precondition(call("show") == nil)
    precondition(window.isVisible)
    precondition(events.last?.0 == "visibility") // Settings must not focus panel input.
    window.testKey = false
    window.resignKey(); drain()
    precondition(window.isVisible, "Settings must survive loss of key status")
    window.testKey = true
    call("resizePanel", ["height": 350])
    precondition(window.contentView!.bounds.size == NSSize(width: 920, height: 650))
    window.setContentSize(NSSize(width: 980, height: 690))
    geometry("settings resize", width: 980, height: 690)
    let savedSize = call("getState", nil) as! [String: Any]
    precondition(savedSize["width"] as? CGFloat == 980 && savedSize["height"] as? CGFloat == 690,
                 "settings persistence must use content size, excluding the title bar")
    window.rebuildSurface(reduceTransparency: false)
    geometry("settings appearance rebuild", width: 980, height: 690)
    call("setPresentation", ["mode": "panel", "show": true])
    geometry("settings to panel", width: 640, height: 350)
    precondition(window.contentView!.bounds.size == NSSize(width: 640, height: 350))
    precondition(events.last?.0 == "focusInput" && events.last?.1 == nil)
    precondition(window.contentViewController === container && drop.superview === input.view)
    if let screen = NSScreen.screens.first(where: { $0.frame.contains(NSEvent.mouseLocation) }) {
      precondition(abs(window.frame.midX - screen.visibleFrame.midX) < 1)
    }
    call("setPresentation", ["mode": "settings", "show": false])
    geometry("restored settings", width: 980, height: 690)
    call("setPresentation", ["mode": "panel", "show": true])
    geometry("second return to panel", width: 640, height: 350)
    precondition(drop.superview === input.view && drop.bounds == input.view.bounds)
    call("setTransient", ["active": true])
    window.testKey = false
    window.resignKey(); drain()
    precondition(window.isVisible, "A transient native operation must keep the panel")
    call("setTransient", ["active": false]); drain()
    precondition(!window.isVisible && window.contentViewController === container && drop.superview === input.view)
    window.testKey = true
    call("show")
    window.testSheet = NSWindow()
    window.testKey = false
    window.resignKey(); drain()
    precondition(window.isVisible, "An attached sheet must keep the panel")
    window.testSheet = nil
    window.resignKey(); drain()
    precondition(!window.isVisible)
    window.testKey = true
    call("show")
    let trackingMenu = NSMenu(title: "Native transient menu")
    NotificationCenter.default.post(name: NSMenu.didBeginTrackingNotification, object: trackingMenu)
    window.testKey = false
    window.resignKey(); drain()
    precondition(window.isVisible, "Native menu tracking must keep the panel")
    NotificationCenter.default.post(name: NSMenu.didEndTrackingNotification, object: trackingMenu)
    drain()
    precondition(!window.isVisible, "The panel must dismiss after menu tracking ends without regained focus")
    window.testKey = true
    call("show")
    window.close()
    precondition(!window.isVisible && window.contentViewController === container)
    let state = call("getState") as! [String: Any]
    precondition(state["visible"] as? Bool == false && state["mode"] as? String == "panel")
    window.openPanel()
    precondition(events.last?.0 == "command" && events.last?.1 as? String == "openPanel")
    let nativeMenu = NSMenu(title: "Main")
    let appItem = NSMenuItem(title: "Gopeed", action: nil, keyEquivalent: "")
    let appMenu = NSMenu(title: "Gopeed")
    appItem.submenu = appMenu
    appMenu.addItem(NSMenuItem(title: "About Gopeed", action: nil, keyEquivalent: ""))
    appMenu.addItem(NSMenuItem(title: "Quit Gopeed", action: nil, keyEquivalent: "q"))
    appMenu.addItem(NSMenuItem(title: "Hide Gopeed", action: nil, keyEquivalent: "h"))
    nativeMenu.addItem(appItem)
    NSApp.mainMenu = nativeMenu
    window.configureMenus()
    precondition(appItem.title == "kownload" && appMenu.title == "kownload")
    precondition(appMenu.items.contains(where: { $0.title == "关于 kownload" }))
    precondition(appMenu.items.contains(where: { $0.title == "退出 kownload" }))
    let settings = appMenu.items.first(where: { $0.keyEquivalent == "," })!
    precondition(NSApp.sendAction(settings.action!, to: settings.target, from: settings))
    precondition(events.last?.0 == "command" && events.last?.1 as? String == "settings")
    let fileMenu = nativeMenu.items.first(where: { $0.title == "文件" })!.submenu!
    let create = fileMenu.items.first(where: { $0.keyEquivalent == "n" })!
    precondition(NSApp.sendAction(create.action!, to: create.target, from: create))
    precondition(events.last?.0 == "command" && events.last?.1 as? String == "newTask")

    for (method, args) in [("setPresentation", ["mode": "bad"] as [String: Any]),
                           ("setPresentation", ["mode": "panel", "show": 1]),
                           ("setPresentation", ["mode": "panel", "width": -1]),
                           ("resizePanel", ["height": Double.nan]),
                           ("resizePanel", ["height": true]),
                           ("setTransient", ["active": 1]),
                           ("registerHotKey", ["keyCode": 2.5, "modifiers": 6144]),
                           ("registerHotKey", ["keyCode": -1, "modifiers": 6144])] {
      precondition(call(method, args) is FlutterError, "Reject malformed \(method) without mutation")
    }
    precondition(call("unknown") as? NSObject === FlutterMethodNotImplemented)

    // Real Carbon success, duplicate/conflict status and retained registration.
    let hotKey = PanelHotKey()
    var pressCount = 0
    hotKey.onPressed = { pressCount += 1 }
    let modifiers = UInt32(controlKey | optionKey | shiftKey | cmdKey)
    let status = hotKey.register(keyCode: 40, modifiers: modifiers)
    precondition(status == noErr, "Native test chord unavailable: \(status)")
    precondition(hotKey.register(keyCode: 40, modifiers: modifiers) == noErr)
    var carbonEvent: EventRef?
    precondition(CreateEvent(nil, OSType(kEventClassKeyboard), UInt32(kEventHotKeyPressed), 0,
      EventAttributes(kEventAttributeUserEvent), &carbonEvent) == noErr)
    var hotKeyID = EventHotKeyID(signature: 0x6B776C64, id: 1)
    precondition(SetEventParameter(carbonEvent, EventParamName(kEventParamDirectObject),
      EventParamType(typeEventHotKeyID), MemoryLayout<EventHotKeyID>.size, &hotKeyID) == noErr)
    precondition(SendEventToEventTarget(carbonEvent, GetApplicationEventTarget()) == noErr && pressCount == 1)
    ReleaseEvent(carbonEvent)
    var reserved: EventHotKeyRef?
    precondition(RegisterEventHotKey(41, modifiers, EventHotKeyID(signature: 0x74657374, id: 1),
      GetApplicationEventTarget(), 0, &reserved) == noErr)
    let conflict = hotKey.register(keyCode: 41, modifiers: modifiers)
    precondition(conflict == OSStatus(eventHotKeyExistsErr), "Must report actual Carbon conflict: \(conflict)")
    precondition(hotKey.register(keyCode: 40, modifiers: modifiers) == noErr)
    var competitor: EventHotKeyRef?
    precondition(RegisterEventHotKey(40, modifiers, EventHotKeyID(signature: 0x74657374, id: 2),
      GetApplicationEventTarget(), 0, &competitor) == OSStatus(eventHotKeyExistsErr), "Failed replacement must preserve the previous effective registration")
    hotKey.unregister()
    precondition(RegisterEventHotKey(40, modifiers, EventHotKeyID(signature: 0x74657374, id: 2),
      GetApplicationEventTarget(), 0, &competitor) == noErr, "Shutdown must release the Carbon hotkey")
    if let reserved = reserved { UnregisterEventHotKey(reserved) }
    if let competitor = competitor { UnregisterEventHotKey(competitor) }
    let registered = call("registerHotKey", ["keyCode": 42, "modifiers": Int(modifiers)]) as! [String: Any]
    precondition(registered["registered"] as? Bool == true && registered["status"] as? Int == Int(noErr))
    window.unregisterHotKey()
    print("PASS: native panel/settings, post-layout bounds/origins/window coordinates across presentation/resize/material changes, glass/fallback/solid content, retained input/drop tree, pointer routing, IME Escape, focus/transient/sheet/menu, appearance isolation, kownload menu commands, channel validation, real Carbon event/conflict and cleanup")
  }
}
