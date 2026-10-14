import Cocoa

private final class DropTarget: NSView {}
private final class PointerReceiver: NSResponder {
  var received: [NSEvent.EventType] = []
  override func mouseDown(with event: NSEvent) { received.append(event.type) }
  override func mouseUp(with event: NSEvent) { received.append(event.type) }
  override func mouseDragged(with event: NSEvent) { received.append(event.type) }
  override func rightMouseDown(with event: NSEvent) { received.append(event.type) }
  override func rightMouseUp(with event: NSEvent) { received.append(event.type) }
  override func rightMouseDragged(with event: NSEvent) { received.append(event.type) }
  override func otherMouseDown(with event: NSEvent) { received.append(event.type) }
  override func otherMouseUp(with event: NSEvent) { received.append(event.type) }
  override func otherMouseDragged(with event: NSEvent) { received.append(event.type) }
  override func scrollWheel(with event: NSEvent) { received.append(event.type) }
}

@main
struct PointerRoutingTest {
  static func main() {
    _ = NSApplication.shared
    let overlay = DropTarget(frame: NSRect(x: 0, y: 0, width: 800, height: 600))
    overlay.registerForDraggedTypes([.fileURL])
    let receiver = PointerReceiver()
    let router = MouseEventRouter()
    func event(_ type: NSEvent.EventType, _ point: NSPoint = NSPoint(x: 730, y: 520)) -> NSEvent {
      NSEvent.mouseEvent(with: type, location: point, modifierFlags: [], timestamp: 1,
        windowNumber: 0, context: nil, eventNumber: 1, clickCount: 1, pressure: 1)!
    }
    let sequences: [[NSEvent.EventType]] = [
      [.leftMouseDown, .leftMouseDragged, .leftMouseUp],
      [.rightMouseDown, .rightMouseDragged, .rightMouseUp],
      [.otherMouseDown, .otherMouseDragged, .otherMouseUp]]
    for sequence in sequences {
      let isolated = MouseEventRouter()
      precondition(!isolated.forward(event(sequence[0]), hitView: NSView(), to: receiver))
      precondition(!isolated.forward(event(sequence[2]), hitView: overlay, to: receiver))
      let before = receiver.received.count
      precondition(router.forward(event(sequence[0]), hitView: overlay, to: receiver))
      precondition(router.forward(event(sequence[1]), hitView: nil, to: receiver))
      precondition(router.forward(event(sequence[2]), hitView: NSView(), to: receiver))
      precondition(Array(receiver.received.suffix(3)) == sequence)
      precondition(receiver.received.count == before + 3)
      precondition(!router.forward(event(sequence[2]), hitView: overlay, to: receiver))
    }
    let scroll = NSEvent(cgEvent: CGEvent(scrollWheelEvent2Source: nil, units: .line,
      wheelCount: 1, wheel1: -1, wheel2: 0, wheel3: 0)!)!
    precondition(router.forward(scroll, hitView: overlay, to: receiver))
    precondition(overlay.registeredDraggedTypes.contains(.fileURL))
    let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 800, height: 600),
      styleMask: [.titled, .closable, .miniaturizable, .resizable, .fullSizeContentView],
      backing: .buffered, defer: true)
    for kind in [NSWindow.ButtonType.closeButton, .miniaturizeButton, .zoomButton] {
      let button = window.standardWindowButton(kind)!
      let point = button.convert(NSPoint(x: button.bounds.midX, y: button.bounds.midY), to: nil)
      let event = NSEvent.mouseEvent(with: .leftMouseDown, location: point,
        modifierFlags: [], timestamp: 1, windowNumber: window.windowNumber, context: nil,
        eventNumber: 1, clickCount: 1, pressure: 1)!
      let before = receiver.received.count
      guard !router.forward(event, hitView: overlay, to: receiver, window: window),
            receiver.received.count == before else {
        fputs("FAIL: full-size drag overlay swallowed system window button \(kind.rawValue)\n", stderr)
        exit(1)
      }
    }
    for point in [NSPoint(x: 2, y: 300), NSPoint(x: 798, y: 300), NSPoint(x: 400, y: 2)] {
      precondition(!router.forward(event(.leftMouseDown, point), hitView: overlay, to: receiver, window: window), "AppKit must receive resize-edge presses")
    }
    let red = window.standardWindowButton(.closeButton)!
    let redPoint = red.convert(NSPoint(x: red.bounds.midX, y: red.bounds.midY), to: nil)
    let before = receiver.received.count
    precondition(router.forward(event(.leftMouseDown), hitView: overlay, to: receiver, window: window))
    precondition(router.forward(event(.leftMouseDragged, redPoint), hitView: overlay, to: receiver, window: window))
    precondition(router.forward(event(.leftMouseUp, redPoint), hitView: overlay, to: receiver, window: window))
    precondition(receiver.received.count == before + 3, "A Flutter release over a traffic light must finish the captured press")
    precondition(router.forward(event(.leftMouseDown), hitView: overlay, to: receiver))
    router.reset()
    let afterReset = receiver.received.count
    precondition(!router.forward(event(.leftMouseDragged), hitView: overlay, to: receiver))
    precondition(!router.forward(event(.leftMouseUp), hitView: overlay, to: receiver))
    precondition(receiver.received.count == afterReset, "Lost focus must clear a stale capture")
    print("PASS: captured pointer sequences, native traffic lights, scrolling and file-drop registration")
  }
}
