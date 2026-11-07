import Foundation

enum IdentityPreferences {
  // Window, shortcut and download settings are already in Hive. The only
  // product-owned native preference is the profile-file bookmark dictionary.
  static let allowedKeys = ["netdiskProfileFileBookmarks"]
  static func importAllowedKeys(
    source: UserDefaults? = UserDefaults(suiteName: AppIdentity.legacyBundleId),
    destination: UserDefaults = .standard
  ) {
    let marker = "kownloadIdentityPreferencesImported"
    guard !destination.bool(forKey: marker) else { return }
    for key in allowedKeys where destination.object(forKey: key) == nil {
      if let value = source?.object(forKey: key) { destination.set(value, forKey: key) }
    }
    destination.set(true, forKey: marker)
  }
}
