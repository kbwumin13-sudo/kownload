import 'dart:async';
import 'dart:collection';

import 'package:get/get.dart';

import '../../../../api/api.dart';
import '../../../../api/model/extension.dart';
import '../../../../api/model/install_extension.dart';
import '../../../../api/model/store_extension.dart';
import '../../../../api/model/switch_extension.dart';

class ExtensionListItem {
  final Extension? installed;
  final StoreExtension? store;

  const ExtensionListItem({this.installed, this.store});

  bool get isInstalled => installed != null;

  String get id => installed?.identity ?? store!.id;

  String get title => installed?.title ?? store?.title ?? '';

  String get author => installed?.author ?? store?.author ?? '';

  String get description => installed?.description ?? store?.description ?? '';

  String get version => installed?.version ?? store?.version ?? '0.0.0';

  String? get homepage {
    final installedHomepage = installed?.homepage;
    if (installedHomepage != null && installedHomepage.isNotEmpty) {
      return installedHomepage;
    }
    return store?.homepage;
  }

  String? get repoUrl {
    final installedRepo = installed?.repository?.url;
    if (installedRepo != null && installedRepo.isNotEmpty) {
      return installedRepo;
    }
    return store?.repoUrl;
  }

  int get stars => store?.stars ?? 0;

  int get installCount => store?.installCount ?? 0;

  String? get icon => store?.icon;
}

class ExtensionController extends GetxController {
  static const manualInstallBusyKey = '__manual_install__';

  final installedExtensions = <Extension>[].obs;
  final updateFlags = <String, String>{}.obs;

  final loadingInstalled = false.obs;
  final busyExtensionIds = <String>{}.obs;
  final showInstallTools = false.obs;

  final devMode = false.obs;
  var _devModeCount = 0;

  bool pendingInstallHandled = false;

  UnmodifiableMapView<String, Extension> get installedMap =>
      UnmodifiableMapView(
          {for (final ext in installedExtensions) ext.identity: ext});

  List<ExtensionListItem> get displayItems => installedExtensions
      .map((extension) => ExtensionListItem(installed: extension))
      .toList();

  @override
  Future<void> onInit() async {
    super.onInit();
    await loadInitialData();
  }

  Future<void> loadInitialData() => loadInstalled();

  Future<void> loadInstalled({bool refreshUpdates = false}) async {
    loadingInstalled.value = true;
    try {
      installedExtensions.value = await getExtensions();
      if (refreshUpdates) {
        await checkUpdate();
      }
    } finally {
      loadingInstalled.value = false;
    }
  }

  Future<void> checkUpdate() async {
    final nextFlags = <String, String>{};
    for (final ext in installedExtensions) {
      try {
        final resp = await upgradeCheckExtension(ext.identity);
        if (resp.newVersion.isNotEmpty) {
          nextFlags[ext.identity] = resp.newVersion;
        }
      } catch (_) {
        // Ignore single extension check failures to avoid breaking the whole list.
      }
    }
    updateFlags.assignAll(nextFlags);
  }

  void toggleInstallTools() {
    showInstallTools.value = !showInstallTools.value;
  }

  Extension? findInstalled(StoreExtension extension) {
    return installedMap[extension.id];
  }

  bool canUpdateFromStore(StoreExtension extension) {
    final installed = findInstalled(extension);
    if (installed == null) return false;
    return _compareVersion(extension.version, installed.version) > 0;
  }

  bool canUpdateItem(ExtensionListItem item) =>
      item.installed != null &&
      updateFlags.containsKey(item.installed!.identity);

  Future<void> installFromStore(StoreExtension extension) =>
      installFromUrl((extension.directory ?? '').trim().isEmpty
          ? extension.repoUrl
          : '${extension.repoUrl}#${extension.directory!.trim()}');

  Future<void> installFromUrl(String url, {bool devInstall = false}) async {
    await _runBusy(manualInstallBusyKey, () async {
      await installExtension(InstallExtension(devMode: devInstall, url: url));
      await loadInstalled();
      _backgroundCheckUpdate();
    });
  }

  Future<void> toggleExtension(Extension extension, bool enabled) async {
    await _runBusy(extension.identity, () async {
      await switchExtension(
          extension.identity, SwitchExtension(status: enabled));
      await loadInstalled(refreshUpdates: false);
    });
  }

  Future<void> removeExtension(Extension extension) async {
    await _runBusy(extension.identity, () async {
      await deleteExtension(extension.identity);
      await loadInstalled(refreshUpdates: false);
      updateFlags.remove(extension.identity);
    });
  }

  Future<void> upgradeExtension(Extension extension) async {
    await _runBusy(extension.identity, () async {
      await updateExtension(extension.identity);
      await loadInstalled(refreshUpdates: false);
      _backgroundCheckUpdate();
    });
  }

  void tryOpenDevMode() {
    if (_devModeCount == 0) {
      Future.delayed(const Duration(seconds: 2), () {
        if (devMode.value) return;
        devMode.value = false;
        _devModeCount = 0;
      });
    }
    _devModeCount++;
    if (_devModeCount >= 5) {
      devMode.value = true;
    }
  }

  static int _compareVersion(String a, String b) {
    final aNums = _toVersionNumbers(a);
    final bNums = _toVersionNumbers(b);
    final maxLen = aNums.length > bNums.length ? aNums.length : bNums.length;

    for (var i = 0; i < maxLen; i++) {
      final left = i < aNums.length ? aNums[i] : 0;
      final right = i < bNums.length ? bNums[i] : 0;
      if (left > right) return 1;
      if (left < right) return -1;
    }
    return 0;
  }

  static List<int> _toVersionNumbers(String version) {
    return version
        .split(RegExp(r'[^0-9]+'))
        .where((part) => part.isNotEmpty)
        .map((part) => int.tryParse(part) ?? 0)
        .toList();
  }

  Future<void> _runBusy(String id, Future<void> Function() action) async {
    if (busyExtensionIds.contains(id)) return;
    busyExtensionIds.add(id);
    try {
      await action();
    } finally {
      busyExtensionIds.remove(id);
    }
  }

  void _backgroundCheckUpdate() {
    unawaited(checkUpdate());
  }
}
