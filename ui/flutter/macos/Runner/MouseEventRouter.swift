import Cocoa

/// Keep each Flutter press/drag/release sequence together while preserving
/// native titlebar buttons and the drag receiver's file-drop callbacks.
final class MouseEventRouter {
  private var capturedButtons = Set<Int>()

  func reset() { capturedButtons.removeAll() }

  func forward(_ event: NSEvent, hitView: NSView?, to responder: NSResponder?, window: NSWindow? = nil) -> Bool {
    guard let responder = responder else { return false }
    let button: Int
    switch event.type {
    case .leftMouseDown, .leftMouseDragged, .leftMouseUp: button = 0
    case .rightMouseDown, .rightMouseDragged, .rightMouseUp: button = 1
    default: button = event.buttonNumber
    }
    let down: Set<NSEvent.EventType> = [.leftMouseDown, .rightMouseDown, .otherMouseDown]
    let up: Set<NSEvent.EventType> = [.leftMouseUp, .rightMouseUp, .otherMouseUp]
    let drag: Set<NSEvent.EventType> = [.leftMouseDragged, .rightMouseDragged, .otherMouseDragged]
    if down.contains(event.type) || event.type == .scrollWheel {
      if let window = window {
        if window.styleMask.contains(.resizable), !window.styleMask.contains(.fullScreen) {
          let point = event.locationInWindow
          // AppKit owns the frame edges, including the bottom resize corners.
          if point.x <= 5 || point.x >= window.frame.width - 5 || point.y <= 5 { return false }
        }
        for kind in [NSWindow.ButtonType.closeButton, .miniaturizeButton, .zoomButton] {
          if let control = window.standardWindowButton(kind), !control.isHiddenOrHasHiddenAncestor,
             control.bounds.contains(control.convert(event.locationInWindow, from: nil)) { return false }
        }
      }
      guard let hit = hitView else { return false }
      let name = String(describing: type(of: hit))
      guard name == "DropTarget" || name.hasSuffix(".DropTarget") else { return false }
      if down.contains(event.type) { capturedButtons.insert(button) }
    } else if up.contains(event.type) || drag.contains(event.type) {
      guard capturedButtons.contains(button) else { return false }
      if up.contains(event.type) { capturedButtons.remove(button) }
    } else { return false }
    switch event.type {
    case .leftMouseDown: responder.mouseDown(with: event)
    case .leftMouseUp: responder.mouseUp(with: event)
    case .leftMouseDragged: responder.mouseDragged(with: event)
    case .rightMouseDown: responder.rightMouseDown(with: event)
    case .rightMouseUp: responder.rightMouseUp(with: event)
    case .rightMouseDragged: responder.rightMouseDragged(with: event)
    case .otherMouseDown: responder.otherMouseDown(with: event)
    case .otherMouseUp: responder.otherMouseUp(with: event)
    case .otherMouseDragged: responder.otherMouseDragged(with: event)
    case .scrollWheel: responder.scrollWheel(with: event)
    default: return false
    }
    return true
  }
}
