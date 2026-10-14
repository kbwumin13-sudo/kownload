import Cocoa
import Carbon
import FlutterMacOS
import window_manager

@available(macOS 26.0, *)
private final class PanelGlassEffectView: NSGlassEffectView {
  override func hitTest(_ point: NSPoint) -> NSView? {
    // NSGlassEffectView's default hit test can consume the event at the glass
    // itself. Route into its actual content, including desktop_drop's receiver.
    // hitTest takes coordinates in the receiver's superview, not its bounds.
    let local = convert(point, from: superview)
    guard bounds.contains(local) else { return nil }
    if let content = contentView, let parent = content.superview,
       let hit = content.hitTest(parent.convert(local, from: self)) { return hit }
    return super.hitTest(point)
  }
}

/// Owns one Carbon registration. Register the replacement first so a conflict
/// never removes the user's working shortcut.
final class PanelHotKey {
  private static let signature: OSType = 0x6B776C64 // kwld
  private var reference: EventHotKeyRef?
  private var handler: EventHandlerRef?
  private var key: (code: UInt32, modifiers: UInt32)?
  private var identifier: UInt32 = 0
  var onPressed: (() -> Void)?

  func register(keyCode: UInt32, modifiers: UInt32) -> OSStatus {
    if let key = key, reference != nil, key.code == keyCode, key.modifiers == modifiers { return noErr }
    if handler == nil {
      var eventType = EventTypeSpec(eventClass: OSType(kEventClassKeyboard), eventKind: UInt32(kEventHotKeyPressed))
      let status = InstallEventHandler(GetApplicationEventTarget(), { _, event, context in
        guard let event = event, let context = context else { return OSStatus(eventNotHandledErr) }
        let owner = Unmanaged<PanelHotKey>.fromOpaque(context).takeUnretainedValue()
        var hotKeyID = EventHotKeyID()
        let status = GetEventParameter(event, EventParamName(kEventParamDirectObject),
          EventParamType(typeEventHotKeyID), nil, MemoryLayout<EventHotKeyID>.size, nil, &hotKeyID)
        guard status == noErr, hotKeyID.signature == PanelHotKey.signature,
              hotKeyID.id == owner.identifier else { return OSStatus(eventNotHandledErr) }
        owner.onPressed?()
        return noErr
      }, 1, &eventType, Unmanaged.passUnretained(self).toOpaque(), &handler)
      guard status == noErr else { return status }
    }
    let nextID = identifier &+ 1
    var replacement: EventHotKeyRef?
    let status = RegisterEventHotKey(keyCode, modifiers,
      EventHotKeyID(signature: Self.signature, id: nextID), GetApplicationEventTarget(), 0, &replacement)
    guard status == noErr, let replacement = replacement else { return status == noErr ? OSStatus(paramErr) : status }
    let previous = reference
    reference = replacement
    identifier = nextID
    key = (keyCode, modifiers)
    if let previous = previous { UnregisterEventHotKey(previous) }
    return status
  }

  func unregister() {
    if let reference = reference { UnregisterEventHotKey(reference) }
    if let handler = handler { RemoveEventHandler(handler) }
    reference = nil
    handler = nil
    key = nil
  }

  deinit { unregister() }
}

class MainFlutterWindow: NSWindow {
  enum Presentation: String { case panel, settings }
  private(set) var presentation: Presentation = .panel
  private var panelSize = NSSize(width: 640, height: 260)
  private var settingsSize = NSSize(width: 1100, height: 720)
  private var settingsOrigin: NSPoint?
  private var changingPresentation = false
  private var transientActive = false
  private var trackingMenus = Set<ObjectIdentifier>()
  private var lastVisibility: Bool?
  private let pointerRouter = MouseEventRouter()
  private weak var pointerController: NSViewController?
  private var containerController: NSViewController?
  private var appearanceChannel: FlutterMethodChannel?
  private var panelChannel: FlutterMethodChannel?
  private var observers: [(NotificationCenter, NSObjectProtocol)] = []
  private let hotKey = PanelHotKey()
  // Also used by the native harness without starting a Flutter engine.
  var sendPanelEvent: ((String, Any?) -> Void)?

  override var canBecomeKey: Bool { true }
  override var canBecomeMain: Bool { presentation == .settings }

  override func performKeyEquivalent(with event: NSEvent) -> Bool {
    // Escape must reach Flutter's text input/IME before any panel dismissal.
    if event.keyCode == 53 { return false }
    let flags = event.modifierFlags.intersection(.deviceIndependentFlagsMask)
    if presentation == .settings, event.charactersIgnoringModifiers?.lowercased() == "f",
       flags.contains([.command, .control]), !flags.contains(.option), !flags.contains(.shift) {
      toggleFullScreen(nil)
      return true
    }
    return super.performKeyEquivalent(with: event)
  }

  override func resignKey() {
    pointerRouter.reset()
    super.resignKey()
    dismissPanelAfterFocusChange()
  }

  private func dismissPanelAfterFocusChange() {
    // Let AppKit attach sheets or begin menu tracking in this same event turn.
    DispatchQueue.main.async { [weak self] in
      guard let self = self, self.presentation == .panel, !self.changingPresentation,
            self.isVisible, !self.isKeyWindow, self.attachedSheet == nil,
            !self.transientActive, self.trackingMenus.isEmpty else { return }
      self.hidePanel()
    }
  }

  override func sendEvent(_ event: NSEvent) {
    let pointerTypes: Set<NSEvent.EventType> = [.leftMouseDown, .leftMouseUp, .leftMouseDragged,
      .rightMouseDown, .rightMouseUp, .rightMouseDragged,
      .otherMouseDown, .otherMouseUp, .otherMouseDragged, .scrollWheel]
    if pointerTypes.contains(event.type), let content = contentView {
      // The DropTarget stays a descendant of the same Flutter view when that
      // view moves into glass.contentView; never route through a sibling layer.
      let hit = content.hitTest(content.convert(event.locationInWindow, from: nil))
      if [.leftMouseDown, .rightMouseDown, .otherMouseDown].contains(event.type) {
        appearanceChannel?.invokeMethod("pointerModifiers", arguments: [
          "shift": event.modifierFlags.contains(.shift), "meta": event.modifierFlags.contains(.command)])
      }
      if pointerRouter.forward(event, hitView: hit, to: pointerController, window: self) {
        focusFlutter()
        return
      }
    }
    super.sendEvent(event)
  }

  override func awakeFromNib() {
    super.awakeFromNib()
    let flutter = FlutterViewController()
    flutter.backgroundColor = .clear
    installContentController(flutter)
    // Plugins discover the existing Flutter controller through this child tree.
    RegisterGeneratedPlugins(registry: flutter)
    isReleasedWhenClosed = false
    hiddenWindowAtLaunch() // Consume window_manager's one-time launch hiding now.
    setPresentation(.panel)
    orderOut(nil)

    let messenger = flutter.engine.binaryMessenger
    let panel = FlutterMethodChannel(name: "gopeed/macos_panel", binaryMessenger: messenger)
    panelChannel = panel
    sendPanelEvent = { [weak panel] method, arguments in panel?.invokeMethod(method, arguments: arguments) }
    panel.setMethodCallHandler { [weak self] call, result in
      guard let self = self else { result(nil); return }
      self.handlePanelCall(call, result: result)
    }
    hotKey.onPressed = { [weak self] in self?.sendPanelEvent?("command", "togglePanel") }

    let appearance = FlutterMethodChannel(name: "gopeed/macos_appearance", binaryMessenger: messenger)
    appearanceChannel = appearance
    appearance.setMethodCallHandler { [weak self] call, result in
      guard let self = self else { result(nil); return }
      self.handleAppearanceCall(call, result: result)
    }
    observe(NSWorkspace.shared.notificationCenter, NSWorkspace.accessibilityDisplayOptionsDidChangeNotification) { [weak self] _ in
      self?.rebuildSurface()
      if let self = self { self.appearanceChannel?.invokeMethod("appearanceChanged", arguments: self.appearanceValues()) }
    }
    observePresentationEvents()
    DispatchQueue.main.async { [weak self] in self?.configureMenus() }
  }

  func observePresentationEvents() {
    observe(.default, NSMenu.didBeginTrackingNotification) { [weak self] notification in
      if let menu = notification.object as? NSMenu { self?.trackingMenus.insert(ObjectIdentifier(menu)) }
    }
    observe(.default, NSMenu.didEndTrackingNotification) { [weak self] notification in
      if let menu = notification.object as? NSMenu { self?.trackingMenus.remove(ObjectIdentifier(menu)) }
      self?.dismissPanelAfterFocusChange()
    }
    observe(.default, NSWindow.didEndSheetNotification, object: self) { [weak self] _ in self?.dismissPanelAfterFocusChange() }
    for name in [NSApplication.didHideNotification, NSApplication.didUnhideNotification,
                 NSWindow.didMiniaturizeNotification, NSWindow.didDeminiaturizeNotification] {
      observe(.default, name) { [weak self] _ in self?.publishVisibility() }
    }
  }

  private func observe(_ center: NotificationCenter, _ name: Notification.Name, object: AnyObject? = nil,
                       using block: @escaping (Notification) -> Void) {
    observers.append((center, center.addObserver(forName: name, object: object, queue: .main, using: block)))
  }

  /// Preserve this controller (and its engine, responders and drop receiver)
  /// across both presentations and accessibility changes.
  func installContentController(_ controller: NSViewController) {
    let container = NSViewController()
    container.view = NSView(frame: NSRect(origin: .zero, size: contentView?.bounds.size ?? panelSize))
    container.addChild(controller)
    containerController = container
    pointerController = controller
    contentViewController = container
    rebuildSurface()
  }

  func rebuildSurface(reduceTransparency: Bool? = nil, supportsGlass: Bool? = nil) {
    guard let container = containerController, let flutterView = pointerController?.view else { return }
    let hadFocus = firstResponder === flutterView || (firstResponder as? NSView)?.isDescendant(of: flutterView) == true
    if #available(macOS 26.0, *) {
      for case let glass as NSGlassEffectView in container.view.subviews where glass.contentView === flutterView {
        glass.contentView = nil
      }
    }
    flutterView.removeFromSuperview()
    // NSGlassEffectView disables this flag when it constrains its contentView.
    // Restore frame/autoresizing layout before reparenting into a plain/effect
    // surface; otherwise the next Auto Layout pass collapses the Flutter wrapper.
    // A new glass surface takes ownership of its content constraints again.
    flutterView.translatesAutoresizingMaskIntoConstraints = true
    container.view.subviews.forEach { $0.removeFromSuperview() }
    let reduced = reduceTransparency ?? NSWorkspace.shared.accessibilityDisplayShouldReduceTransparency
    let surface: NSView
    if presentation == .panel, !reduced {
      if #available(macOS 26.0, *), supportsGlass != false {
        let glass = PanelGlassEffectView(frame: container.view.bounds)
        glass.style = .regular
        glass.cornerRadius = 22
        if #available(macOS 27.0, *) { glass.effectIsInteractive = true }
        glass.contentView = flutterView
        surface = glass
      } else {
        let effect = NSVisualEffectView(frame: container.view.bounds)
        effect.material = .popover
        effect.blendingMode = .behindWindow
        effect.state = .active
        effect.addSubview(flutterView)
        surface = effect
      }
    } else {
      surface = NSView(frame: container.view.bounds)
      surface.wantsLayer = true
      effectiveAppearance.performAsCurrentDrawingAppearance {
        surface.layer?.backgroundColor = NSColor.windowBackgroundColor.cgColor
      }
      surface.addSubview(flutterView)
    }
    surface.autoresizingMask = [.width, .height]
    // Clip Flutter and fallback materials at the same natural window outline.
    surface.wantsLayer = true
    surface.layer?.cornerRadius = presentation == .panel ? 22 : 0
    surface.layer?.masksToBounds = true
    container.view.addSubview(surface)
    flutterView.frame = surface.bounds
    flutterView.autoresizingMask = [.width, .height]
    if hadFocus { makeFirstResponder(flutterView) }
    invalidateShadow()
  }

  func setPresentation(_ mode: Presentation, width: CGFloat? = nil, height: CGFloat? = nil, show: Bool? = nil) {
    changingPresentation = true
    defer { changingPresentation = false }
    if presentation == .settings {
      settingsSize = contentView?.bounds.size ?? settingsSize
      settingsOrigin = frame.origin
    }
    presentation = mode
    pointerRouter.reset()
    // Reset every layout constraint before changing style: settings' minimum
    // must not clamp the compact panel, including repeated calls in one mode.
    contentMinSize = .zero
    minSize = .zero
    maxSize = NSSize(width: CGFloat.greatestFiniteMagnitude, height: CGFloat.greatestFiniteMagnitude)
    contentMaxSize = maxSize
    toolbar = nil
    toolbarStyle = .automatic
    titlebarAppearsTransparent = false
    titleVisibility = mode == .panel ? .hidden : .visible
    title = mode == .panel ? "kownload" : "kownload — 设置"
    isMovableByWindowBackground = false
    isOpaque = false
    hasShadow = true
    backgroundColor = mode == .panel ? .clear : .windowBackgroundColor
    hidesOnDeactivate = false
    isExcludedFromWindowsMenu = mode == .panel
    animationBehavior = .none
    if mode == .panel {
      styleMask = [.borderless]
      level = .floating
      collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary, .transient, .ignoresCycle]
      if #available(macOS 13.0, *) { collectionBehavior.insert(.canJoinAllApplications) }
      panelSize = NSSize(width: width ?? panelSize.width, height: height ?? panelSize.height)
      if let screen = NSScreen.screens.first(where: { $0.frame.contains(NSEvent.mouseLocation) }) ?? NSScreen.main {
        panelSize.height = min(panelSize.height, max(180, screen.visibleFrame.height - 80))
        panelSize.width = min(panelSize.width, screen.visibleFrame.width - 32)
      }
      setContentSize(panelSize)
      positionPanel()
    } else {
      styleMask = [.titled, .closable, .miniaturizable, .resizable]
      level = .normal
      collectionBehavior = [.managed, .moveToActiveSpace, .fullScreenPrimary, .participatesInCycle]
      settingsSize = NSSize(width: max(800, width ?? settingsSize.width), height: max(560, height ?? settingsSize.height))
      contentMinSize = NSSize(width: 800, height: 560)
      setContentSize(settingsSize)
      if let origin = settingsOrigin { setFrameOrigin(origin) } else { center() }
    }
    rebuildSurface()
    focusFlutter()
    if show == true { showPanel() }
    if show == false { hidePanel() }
  }

  private func positionPanel() {
    let pointer = NSEvent.mouseLocation
    guard let screen = NSScreen.screens.first(where: { $0.frame.contains(pointer) }) ?? NSScreen.main else { return }
    let area = screen.visibleFrame
    setFrameOrigin(NSPoint(x: area.midX - frame.width / 2, y: area.maxY - frame.height - 64))
  }

  func showPanel() {
    if isMiniaturized { deminiaturize(nil) }
    if presentation == .panel { positionPanel() }
    NSApp.unhide(nil)
    NSApp.activate(ignoringOtherApps: true)
    makeKeyAndOrderFront(nil)
    focusFlutter()
    publishVisibility()
    if presentation == .panel { sendPanelEvent?("focusInput", nil) }
  }

  func hidePanel() { orderOut(nil) }
  func openPanel() { sendPanelEvent?("command", "openPanel") }
  func unregisterHotKey() { hotKey.unregister() }

  private var visibleToUser: Bool { isVisible && !isMiniaturized && !NSApp.isHidden }
  private func publishVisibility() {
    let visible = visibleToUser
    guard lastVisibility != visible else { return }
    lastVisibility = visible
    sendPanelEvent?("visibility", ["visible": visible])
  }

  override func order(_ place: NSWindow.OrderingMode, relativeTo otherWin: Int) {
    super.order(place, relativeTo: otherWin)
    publishVisibility()
  }
  override func orderOut(_ sender: Any?) {
    super.orderOut(sender)
    pointerRouter.reset()
    publishVisibility()
  }
  // No close path may destroy the retained Flutter content/controller.
  override func close() { hidePanel() }
  override func performClose(_ sender: Any?) { hidePanel() }

  func handlePanelCall(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
    let args = call.arguments as? [String: Any] ?? [:]
    func invalid(_ message: String) { result(FlutterError(code: "invalid_arguments", message: message, details: nil)) }
    func boolean(_ key: String) -> Bool? {
      guard let value = args[key] as? NSNumber, CFGetTypeID(value) == CFBooleanGetTypeID() else { return nil }
      return value.boolValue
    }
    func dimension(_ key: String) -> CGFloat? {
      guard let number = args[key] as? NSNumber,
            CFGetTypeID(number) != CFBooleanGetTypeID(), number.doubleValue.isFinite, number.doubleValue > 0 else { return nil }
      return CGFloat(number.doubleValue)
    }
    switch call.method {
    case "setPresentation":
      guard let value = args["mode"] as? String, let mode = Presentation(rawValue: value) else { invalid("mode must be panel or settings"); return }
      for key in ["width", "height"] where args[key] != nil && dimension(key) == nil { invalid("\(key) must be a positive finite number"); return }
      if args["show"] != nil && boolean("show") == nil { invalid("show must be a bool"); return }
      setPresentation(mode, width: dimension("width"), height: dimension("height"), show: boolean("show"))
      result(nil)
    case "resizePanel":
      guard let height = dimension("height") else { invalid("height must be a positive finite number"); return }
      let availableHeight = (screen ?? NSScreen.main)?.visibleFrame.height ?? (height + 80)
      panelSize.height = min(height, max(180, availableHeight - 80))
      if presentation == .panel {
        let top = frame.maxY
        setContentSize(panelSize)
        setFrameOrigin(NSPoint(x: frame.minX, y: top - frame.height))
        invalidateShadow()
      }
      result(nil)
    case "show": showPanel(); result(nil)
    case "hide": hidePanel(); result(nil)
    case "setTransient":
      guard let active = boolean("active") else { invalid("active must be a bool"); return }
      transientActive = active
      if !active { dismissPanelAfterFocusChange() }
      result(nil)
    case "registerHotKey":
      func uint32(_ key: String) -> UInt32? {
        guard let value = args[key] as? NSNumber, CFGetTypeID(value) != CFBooleanGetTypeID() else { return nil }
        return UInt32(exactly: value.doubleValue)
      }
      guard let code = uint32("keyCode"), let modifiers = uint32("modifiers") else { invalid("keyCode and modifiers must be UInt32 integers"); return }
      let status = hotKey.register(keyCode: code, modifiers: modifiers)
      result(["registered": status == noErr, "status": Int(status)])
    case "getState": result(["visible": visibleToUser, "mode": presentation.rawValue])
    default: result(FlutterMethodNotImplemented)
    }
  }

  func handleAppearanceCall(_ call: FlutterMethodCall, result: @escaping FlutterResult) {
    let args = call.arguments as? [String: Any] ?? [:]
    switch call.method {
    case "appearance": result(appearanceValues())
    case "toolbar":
      appearance = NSAppearance(named: args["dark"] as? Bool == true ? .darkAqua : .aqua)
      rebuildSurface()
      result(nil)
    // The panel channel exclusively owns presentation. Legacy Dart layout
    // messages cannot recreate old overlapping materials or constraints.
    case "windowMinimum", "inspector", "sidebar", "glassRegions", "windowTint": result(nil)
    case "symbol":
      let name = args["name"] as? String ?? ""
      let size = (args["size"] as? NSNumber)?.doubleValue ?? 36
      guard let image = NSImage(systemSymbolName: name, accessibilityDescription: nil),
            let styled = image.withSymbolConfiguration(NSImage.SymbolConfiguration(pointSize: CGFloat(size), weight: .regular)),
            let data = styled.tiffRepresentation, let rep = NSBitmapImageRep(data: data),
            let png = rep.representation(using: .png, properties: [:]) else { result(nil); return }
      result(FlutterStandardTypedData(bytes: png))
    default: result(FlutterMethodNotImplemented)
    }
  }

  func configureMenus() {
    title = presentation == .panel ? "kownload" : "kownload — 设置"
    guard let menu = NSApp.mainMenu else { return }
    menu.items.first?.title = "kownload"
    if let appMenu = menu.items.first?.submenu {
      appMenu.title = "kownload"
      for item in appMenu.items {
        if item.keyEquivalent == "q" { item.title = "退出 kownload" }
        if item.keyEquivalent == "h" { item.title = item.keyEquivalentModifierMask.contains(.option) ? "隐藏其他" : "隐藏 kownload" }
        if item.title.hasPrefix("About") { item.title = "关于 kownload" }
        if item.title == "Services" { item.title = "服务" }
        if item.title == "Show All" { item.title = "显示全部" }
      }
      let settings = appMenu.items.first(where: { $0.keyEquivalent == "," }) ?? NSMenuItem(title: "设置…", action: #selector(openSettings), keyEquivalent: ",")
      settings.title = "设置…"; settings.action = #selector(openSettings); settings.target = self
      if settings.menu == nil { appMenu.insertItem(settings, at: min(2, appMenu.items.count)) }
    }
    // Reuse the NIB menus so Cocoa's responder-chain commands remain intact.
    func existing(_ titles: [String], localized: String) -> NSMenu {
      if let item = menu.items.first(where: { titles.contains($0.title) }), let submenu = item.submenu {
        item.title = localized; submenu.title = localized; return submenu
      }
      let item = NSMenuItem(title: localized, action: nil, keyEquivalent: "")
      let submenu = NSMenu(title: localized); item.submenu = submenu
      menu.insertItem(item, at: min(1, menu.items.count)); return submenu
    }
    let file = existing(["File", "文件"], localized: "文件")
    let create = NSMenuItem(title: "新建下载…", action: #selector(newTask), keyEquivalent: "n"); create.target = self
    file.insertItem(create, at: 0)
    let close = NSMenuItem(title: "关闭窗口", action: #selector(hideMainWindow), keyEquivalent: "w"); close.target = self
    file.addItem(close)
    let edit = existing(["Edit", "编辑"], localized: "编辑")
    let names = ["Undo":"撤销", "Redo":"重做", "Cut":"剪切", "Copy":"复制", "Paste":"粘贴", "Select All":"全选", "Find":"查找", "Start Dictation…":"开始听写…", "Emoji & Symbols":"表情与符号", "Paste and Match Style":"粘贴并匹配样式", "Delete":"删除", "Find…":"查找…", "Find and Replace…":"查找并替换…", "Find Next":"查找下一个", "Find Previous":"查找上一个", "Use Selection for Find":"使用所选内容查找", "Jump to Selection":"跳到所选内容", "Spelling and Grammar":"拼写和语法", "Substitutions":"替换", "Transformations":"转换", "Speech":"语音"]
    func localize(_ menu: NSMenu) {
      for item in menu.items {
        if let name = names[item.title] { item.title = name }
        if item.keyEquivalentModifierMask == [.command] {
          if item.keyEquivalent == "f" { item.target = self; item.action = #selector(searchTasks) }
          if item.keyEquivalent == "a" { item.target = self; item.action = #selector(selectAllItems) }
        }
        if let submenu = item.submenu { localize(submenu) }
      }
    }
    localize(edit)
    let view = existing(["View", "显示"], localized: "显示")
    let inspector = NSMenuItem(title: "显示／隐藏详情", action: #selector(toggleInspector), keyEquivalent: "i")
    inspector.keyEquivalentModifierMask = [.command, .option]; inspector.target = self; view.insertItem(inspector, at: 0)
    for item in view.items {
      if item.title == "Enter Full Screen" { item.title = "进入全屏" }
      if item.title == "Show Toolbar" { item.title = "显示工具栏" }
      if item.title == "Hide Toolbar" { item.title = "隐藏工具栏" }
    }
    let window = existing(["Window", "窗口"], localized: "窗口")
    for item in window.items {
      if item.title == "Minimize" { item.title = "最小化" }
      if item.title == "Minimize All" { item.title = "全部最小化" }
      if item.title == "Zoom All" { item.title = "全部缩放" }
      if item.title == "Move & Resize" { item.title = "移动与调整大小" }
      if item.title == "Zoom" { item.title = "缩放" }
      if item.title == "Bring All to Front" { item.title = "前置全部窗口" }
    }
    if let help = menu.items.first(where: { $0.title == "Help" }) { help.title = "帮助"; help.submenu?.title = "帮助" }
  }

  @objc private func toggleInspector() { appearanceChannel?.invokeMethod("command", arguments: "inspector") }
  @objc private func hideMainWindow() { hidePanel() }
  private func focusFlutter() { if let view = pointerController?.view { makeFirstResponder(view) } }
  @objc private func newTask() { sendPanelEvent?("command", "newTask") }
  @objc private func openSettings() { sendPanelEvent?("command", "settings") }
  @objc private func toggleSidebar() { appearanceChannel?.invokeMethod("command", arguments: "sidebar") }
  @objc private func searchTasks() { focusFlutter(); appearanceChannel?.invokeMethod("command", arguments: "search") }
  @objc private func selectAllItems() { appearanceChannel?.invokeMethod("command", arguments: "selectAll") }

  private func appearanceValues() -> [String: Bool] {
    let supportsGlass: Bool
    if #available(macOS 26.0, *) { supportsGlass = true } else { supportsGlass = false }
    return ["reduceTransparency": NSWorkspace.shared.accessibilityDisplayShouldReduceTransparency,
            "reduceMotion": NSWorkspace.shared.accessibilityDisplayShouldReduceMotion,
            "supportsGlass": supportsGlass]
  }

  deinit {
    hotKey.unregister()
    for (center, observer) in observers { center.removeObserver(observer) }
  }
}
