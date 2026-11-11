import 'dart:io';

import 'package:flutter/services.dart';

import '../util/util.dart';
import 'app_identity.dart';
import 'native_manifests.dart';
import 'native_manifest_service.dart';

export 'native_manifest_service.dart';

Future<NativeManifestService>? _currentService;
Future<NativeManifestService> _service() => _currentService ??= () async {
      return NativeManifestService(
        home: Platform.environment['HOME'] ?? '',
        storageDir: Util.getStorageDir(),
        previewDirectory: Util.isIsolatedPreview
            ? macManifestDirectory('chrome',
                preview: true,
                home: '',
                override: Platform.environment['KOWNLOAD_NATIVE_MANIFEST_DIR'])
            : null,
        loadAsset: () async {
          final value = await rootBundle
              .load('assets/exec/${AppIdentity.hostExecutable}');
          return value.buffer
              .asUint8List(value.offsetInBytes, value.lengthInBytes);
        },
      );
    }();

Future<BrowserConnectionReport> inspectBrowser(String browser) async =>
    (await _service()).inspectBrowser(browser);
Future<BrowserConnectionReport> connectBrowser(String browser,
        {bool legacyCompatibility = false}) async =>
    (await _service())
        .connectBrowser(browser, legacyCompatibility: legacyCompatibility);
Future<BrowserConnectionReport> restoreBrowser(String browser,
        {String hostName = AppIdentity.hostName}) async =>
    (await _service()).restoreBrowser(browser, hostName: hostName);

/// Compatibility presentation only. This is disk configuration, never proof
/// of the active browser connection; the new settings UI uses inspectBrowser.
Future<Map<String, ManifestRoute?>> browserRoutes() async => {
      for (final browser in switchableBrowsers)
        browser: switch (await inspectBrowser(browser)) {
          final report when report.configured => ManifestRoute.thisApp,
          final report when !report.newHost.present => ManifestRoute.none,
          _ => ManifestRoute.elsewhere,
        },
    };

/// Old no-argument callers must choose a browser. Never guess Chrome or alter
/// two browsers/aliases behind the old combined settings action.
Future<List<String>> connectBrowsers() async =>
    throw StateError('请选择 Chrome 或 Edge 后连接；未修改浏览器清单');

/// Replaceable in widget tests; the services themselves only act on explicit
/// paths and a single browser. No test mode or fake profile enters production.
class BrowserConnection {
  static Future<Map<String, ManifestRoute?>> Function() routes = browserRoutes;
  static Future<List<String>> Function() connect = connectBrowsers;
  static Future<BrowserConnectionReport> Function(String) inspect =
      inspectBrowser;
  static Future<BrowserConnectionReport> Function(String,
      {bool legacyCompatibility}) connectOne = connectBrowser;
  static Future<BrowserConnectionReport> Function(String, {String hostName})
      restoreOne = restoreBrowser;
}
