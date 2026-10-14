import Cocoa
import FlutterMacOS
import app_links

@main
class AppDelegate: FlutterAppDelegate {
  override func applicationWillFinishLaunching(_ notification: Notification) {
    super.applicationWillFinishLaunching(notification)
    NSApp.setActivationPolicy(.accessory)
  }

  override func applicationDidFinishLaunching(_ notification: Notification) {
    super.applicationDidFinishLaunching(notification)
    NSApp.setActivationPolicy(.accessory)
    (mainFlutterWindow as? MainFlutterWindow)?.configureMenus()
  }

  override func applicationWillTerminate(_ notification: Notification) {
    (mainFlutterWindow as? MainFlutterWindow)?.unregisterHotKey()
    super.applicationWillTerminate(notification)
  }

  public override func application(_ application: NSApplication,
                                  continue userActivity: NSUserActivity,
                                  restorationHandler: @escaping ([any NSUserActivityRestoring]) -> Void) -> Bool {

    guard let url = AppLinks.shared.getUniversalLink(userActivity) else {
      return false
    }
    
    AppLinks.shared.handleLink(link: url.absoluteString)
    
    return false // Returning true will stop the propagation to other packages
  }

  override func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool {
    return false
  }

  override func applicationSupportsSecureRestorableState(_ app: NSApplication) -> Bool {
    return true
  }

  override func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
    (mainFlutterWindow as? MainFlutterWindow)?.openPanel()
    return true
  }

  override func application(_ sender: NSApplication, openFile filename: String) -> Bool {
    let url = URL(fileURLWithPath: filename)
    AppLinks.shared.handleLink(link: url.absoluteString)
    return false;
  }

  override func application(_ application: NSApplication, open urls: [URL]) {
    // Only handle the first file
    if(urls.isEmpty) {
      return
    }
    AppLinks.shared.handleLink(link: urls.first!.absoluteString)
  }
}
