import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:path/path.dart' as path;

import '../util/asset_installer.dart';
import 'app_identity.dart';
import 'native_manifests.dart';

class ManifestEvidence {
  final String hostName, owner, executable;
  final bool present, schemaValid, allowedOrigin;
  final List<String> issues;
  const ManifestEvidence(this.hostName,
      {required this.present,
      required this.schemaValid,
      required this.allowedOrigin,
      required this.owner,
      required this.executable,
      this.issues = const []});
  bool get configured =>
      owner == 'this_app' &&
      executable == 'current_asset' &&
      schemaValid &&
      allowedOrigin;
  Map<String, dynamic> toJson() => {
        'hostName': hostName,
        'present': present,
        'schemaValid': schemaValid,
        'allowedOrigin': allowedOrigin,
        'owner': owner,
        'executable': executable,
        'issues': issues,
      };
}

/// Disk evidence only. Active browser/host/Core tests must be supplied by the
/// real browser, never inferred from a matching file or a settings-page ping.
class BrowserConnectionReport {
  final String browser;
  final ManifestEvidence newHost, legacyAlias;
  final List<String> changedHostNames, issues;
  const BrowserConnectionReport(this.browser, this.newHost, this.legacyAlias,
      {this.changedHostNames = const [], this.issues = const []});
  bool get configured => newHost.configured;
  String get native => 'untested';
  Map<String, dynamic> toJson() => {
        'version': 1,
        'browser': browser,
        'newHost': newHost.toJson(),
        'legacyAlias': legacyAlias.toJson(),
        'native': native,
        'appReachable': null,
        'coreHandoffVersion': null,
        'pendingBindingConflict': null,
        'changedHostNames': changedHostNames,
        'issues': issues,
      };
}

/// One explicit browser at a time. Asset bytes come from the current App's
/// bundled resource; a matching pathname by itself is never ownership proof.
class NativeManifestService {
  final String home, storageDir;
  final String? previewDirectory;
  final Future<List<int>> Function() loadAsset;
  Future<List<int>>? _asset;
  Future<String>? _uid;
  Future<void> _writing = Future.value();
  NativeManifestService(
      {required this.home,
      required this.storageDir,
      required this.loadAsset,
      this.previewDirectory});

  String get executable => path.join(storageDir, AppIdentity.hostExecutable);
  Future<List<int>> _bytes() => _asset ??= loadAsset().then((v) {
        if (v.isEmpty) throw const FormatException('asset_unavailable');
        return List<int>.unmodifiable(v);
      });
  String _directory(String browser) {
    if (!switchableBrowsers.contains(browser) ||
        !path.isAbsolute(home) ||
        !path.isAbsolute(storageDir)) {
      throw ArgumentError('supported browser and absolute paths required');
    }
    if (previewDirectory != null) {
      final root = macManifestDirectory(browser,
          preview: true, home: '', override: previewDirectory);
      return path.join(root, browser);
    }
    return macNativeHostsDirectory(browser, home: home)!;
  }

  String _backup(String browser) =>
      path.join(storageDir, 'native-manifest-backups', browser);
  List<int> _canonical(String name) =>
      utf8.encode(const JsonEncoder.withIndent('  ')
          .convert(nativeManifest(name, executable)));
  bool _same(List<int>? a, List<int>? b) =>
      a == null || b == null ? a == b : base64Encode(a) == base64Encode(b);

  Future<String> _currentUid() =>
      _uid ??= Process.run('/usr/bin/id', ['-u']).then((r) {
        final value = r.stdout.toString().trim();
        if (r.exitCode != 0 || !RegExp(r'^\d+$').hasMatch(value)) {
          throw const FormatException('ownership_unavailable');
        }
        return value;
      });
  Future<bool> _owned(String value) async {
    final r = await Process.run('/usr/bin/stat', ['-f', '%u', value]);
    return r.exitCode == 0 && r.stdout.toString().trim() == await _currentUid();
  }

  /// Descendants of the chosen root must not redirect through a symlink. The
  /// system's /tmp alias outside that root may resolve normally in fixtures.
  Future<bool> _safePath(String base, String value) async {
    if (!path.isAbsolute(base) ||
        !path.isAbsolute(value) ||
        !(path.equals(base, value) || path.isWithin(base, value))) {
      return false;
    }
    var current = path.normalize(base);
    if (await FileSystemEntity.type(current, followLinks: false) ==
        FileSystemEntityType.link) {
      return false;
    }
    for (final part in path.split(path.relative(value, from: base))) {
      if (part == '.') continue;
      current = path.join(current, part);
      if (await FileSystemEntity.type(current, followLinks: false) ==
          FileSystemEntityType.link) {
        return false;
      }
    }
    return true;
  }

  Future<bool> _privateStore() async {
    final stat = await Directory(storageDir).stat();
    return stat.type == FileSystemEntityType.directory &&
        (stat.mode & 63) == 0 &&
        await _safePath(storageDir, storageDir) &&
        await _owned(storageDir);
  }

  Future<bool> _safeManifestPath(String browser) async {
    final dir = _directory(browser);
    final base = previewDirectory ?? home;
    return await _safePath(base, dir);
  }

  Future<String> _assetState() async {
    try {
      if (!await _privateStore() || !await _safePath(storageDir, executable)) {
        return 'unavailable';
      }
      final stat = await File(executable).stat();
      if (stat.type != FileSystemEntityType.file) return 'unavailable';
      if ((stat.mode & 18) != 0 ||
          (stat.mode & 64) == 0 ||
          !await _owned(executable)) {
        return 'mismatch';
      }
      return (await File(executable).openRead().transform(sha256).first)
                  .toString() ==
              sha256.convert(await _bytes()).toString()
          ? 'current_asset'
          : 'mismatch';
    } catch (_) {
      return 'unavailable';
    }
  }

  Future<ManifestEvidence> _inspect(
      String browser, String name, String asset) async {
    final file = File(path.join(_directory(browser), '$name.json'));
    try {
      final type = await FileSystemEntity.type(file.path, followLinks: false);
      if (type == FileSystemEntityType.notFound) {
        return ManifestEvidence(name,
            present: false,
            schemaValid: false,
            allowedOrigin: false,
            owner: 'missing',
            executable: asset);
      }
      if (type != FileSystemEntityType.file ||
          !await _safeManifestPath(browser) ||
          !await _owned(file.path) ||
          ((await file.stat()).mode & 18) != 0) {
        return ManifestEvidence(name,
            present: true,
            schemaValid: false,
            allowedOrigin: false,
            owner: 'unknown',
            executable: asset,
            issues: const ['manifest_ownership_unverified']);
      }
      final value = jsonDecode(await file.readAsString());
      final schema = value is Map &&
          value['name'] == name &&
          value['type'] == 'stdio' &&
          value['path'] is String &&
          path.isAbsolute(value['path']);
      final origins = value is Map ? value['allowed_origins'] : null;
      final allowed = AppIdentity.chromiumExtensionIds
          .map((id) => 'chrome-extension://$id/')
          .toSet();
      final origin = origins is List &&
          origins.contains(
              'chrome-extension://${AppIdentity.chromiumExtensionIds.first}/') &&
          origins.every((v) => v is String && allowed.contains(v));
      final ownPath = schema && value['path'] == executable;
      final ours = schema && origin && ownPath && asset == 'current_asset';
      return ManifestEvidence(name,
          present: true,
          schemaValid: schema,
          allowedOrigin: origin,
          owner: ours
              ? 'this_app'
              : schema && !ownPath
                  ? 'foreign'
                  : 'unknown',
          executable: ownPath ? asset : 'mismatch',
          issues: [
            if (!schema) 'invalid_manifest',
            if (!origin) 'origin_mismatch',
            if (!ownPath) 'foreign_route',
            if (ownPath && asset != 'current_asset') 'asset_unverified'
          ]);
    } catch (_) {
      return ManifestEvidence(name,
          present: true,
          schemaValid: false,
          allowedOrigin: false,
          owner: 'unknown',
          executable: asset,
          issues: const ['manifest_unreadable']);
    }
  }

  Future<BrowserConnectionReport> inspectBrowser(String browser) async {
    _directory(browser);
    final asset = await _assetState();
    return BrowserConnectionReport(
        browser,
        await _inspect(browser, AppIdentity.hostName, asset),
        await _inspect(browser, AppIdentity.legacyHostName, asset));
  }

  Future<void> _permissions(String value, String mode) async {
    final r = await Process.run('/bin/chmod', [mode, value]);
    if (r.exitCode != 0) throw const FileSystemException('permission_failed');
  }

  Future<void> _ensureAsset() async {
    if (!await _privateStore() || !await _safePath(storageDir, executable)) {
      throw const FileSystemException('asset_ownership_unverified');
    }
    final type = await FileSystemEntity.type(executable, followLinks: false);
    if (type == FileSystemEntityType.notFound) {
      await writeInstalledAsset(await _bytes(), executable, executable: true);
    }
    if (await _assetState() != 'current_asset') {
      throw const FileSystemException('asset_unverified');
    }
  }

  Future<List<int>?> _readCurrent(File file) async {
    final type = await FileSystemEntity.type(file.path, followLinks: false);
    if (type == FileSystemEntityType.notFound) return null;
    if (type != FileSystemEntityType.file) {
      throw const FileSystemException('manifest_conflict');
    }
    return file.readAsBytes();
  }

  Future<void> _backupFirst(
      String browser, String name, List<int>? original) async {
    final dir = Directory(_backup(browser));
    if (!await _safePath(storageDir, dir.path)) {
      throw const FileSystemException('backup_conflict');
    }
    await dir.create(recursive: true);
    await _permissions(dir.path, '700');
    final file = File(path.join(dir.path, '$name.json'));
    final absent = File(path.join(dir.path, '$name.absent'));
    final ft = await FileSystemEntity.type(file.path, followLinks: false);
    final at = await FileSystemEntity.type(absent.path, followLinks: false);
    if (ft != FileSystemEntityType.notFound ||
        at != FileSystemEntityType.notFound) {
      if (!((ft == FileSystemEntityType.file &&
              at == FileSystemEntityType.notFound) ||
          (at == FileSystemEntityType.file &&
              ft == FileSystemEntityType.notFound))) {
        throw const FileSystemException('backup_conflict');
      }
      final existing = ft == FileSystemEntityType.file ? file : absent;
      if (!await _owned(existing.path) ||
          ((await existing.stat()).mode & 18) != 0 ||
          (at == FileSystemEntityType.file && await absent.length() != 0)) {
        throw const FileSystemException('backup_conflict');
      }
      return; // Never replace the first original/absent marker.
    }
    final target = original == null ? absent : file;
    final stage = await dir.createTemp('.kownload-native-backup-');
    try {
      await _permissions(stage.path, '700');
      final staged = File(path.join(stage.path, 'original'));
      await staged.writeAsBytes(original ?? const [], flush: true);
      await _permissions(staged.path, '600');
      // Same-directory hard-link publication is atomic and fails if the
      // destination already exists. rename() would replace the first original.
      // Abrupt exit before this point leaves only unpublished private staging.
      final published =
          await Process.run('/bin/ln', [staged.path, target.path]);
      if (published.exitCode != 0) {
        throw const FileSystemException('backup_conflict');
      }
    } finally {
      await stage.delete(recursive: true);
    }
  }

  Future<void> _replace(File file, List<int> bytes, List<int>? expected) async {
    final stage = await file.parent.createTemp('.kownload-manifest-');
    try {
      final temp = File(path.join(stage.path, 'manifest'));
      await temp.writeAsBytes(bytes, flush: true);
      await _permissions(temp.path, '600');
      if (!_same(await _readCurrent(file), expected)) {
        throw const FileSystemException('manifest_conflict');
      }
      await temp.rename(file.path);
    } finally {
      await stage.delete(recursive: true);
    }
  }

  Future<bool> _write(String browser, String name) async {
    final evidence = await _inspect(browser, name, await _assetState());
    if (!const ['missing', 'this_app'].contains(evidence.owner)) {
      throw const FileSystemException('manifest_conflict');
    }
    if (!await _safeManifestPath(browser)) {
      throw const FileSystemException('manifest_conflict');
    }
    final file = File(path.join(_directory(browser), '$name.json'));
    final original = await _readCurrent(file), bytes = _canonical(name);
    if (_same(original, bytes)) return false;
    // Backup is committed before the first manifest mutation. Failure here
    // leaves that manifest byte-for-byte unchanged.
    await _backupFirst(browser, name, original);
    await file.parent.create(recursive: true);
    await _replace(file, bytes, original);
    return true;
  }

  Future<BrowserConnectionReport> _serialize(
      Future<BrowserConnectionReport> Function() work) {
    final result = _writing.then((_) => work());
    _writing = result.then<void>((_) {}, onError: (Object _, StackTrace __) {});
    return result;
  }

  Future<BrowserConnectionReport> connectBrowser(String browser,
          {bool legacyCompatibility = false}) =>
      _serialize(() =>
          _connectBrowser(browser, legacyCompatibility: legacyCompatibility));

  Future<BrowserConnectionReport> _connectBrowser(String browser,
      {bool legacyCompatibility = false}) async {
    _directory(browser);
    final changed = <String>[], issues = <String>[];
    await _ensureAsset();
    for (final name in [
      AppIdentity.hostName,
      if (legacyCompatibility) AppIdentity.legacyHostName
    ]) {
      try {
        if (await _write(browser, name)) changed.add(name);
      } catch (_) {
        issues.add(name == AppIdentity.hostName
            ? 'new_host_not_written'
            : 'legacy_alias_preserved');
      }
    }
    final result = await inspectBrowser(browser);
    return BrowserConnectionReport(browser, result.newHost, result.legacyAlias,
        changedHostNames: List.unmodifiable(changed),
        issues: List.unmodifiable(issues));
  }

  Future<BrowserConnectionReport> restoreBrowser(String browser,
          {String hostName = AppIdentity.hostName}) =>
      _serialize(() => _restoreBrowser(browser, hostName: hostName));

  Future<BrowserConnectionReport> _restoreBrowser(String browser,
      {String hostName = AppIdentity.hostName}) async {
    _directory(browser);
    if (!const [AppIdentity.hostName, AppIdentity.legacyHostName]
        .contains(hostName)) {
      throw ArgumentError('unsupported host');
    }
    final evidence = await _inspect(browser, hostName, await _assetState());
    final file = File(path.join(_directory(browser), '$hostName.json'));
    if (!evidence.configured ||
        !_same(await _readCurrent(file), _canonical(hostName)) ||
        !await _safePath(storageDir, _backup(browser))) {
      throw const FileSystemException('restore_conflict');
    }
    final backup = File(path.join(_backup(browser), '$hostName.json'));
    final absent = File(path.join(_backup(browser), '$hostName.absent'));
    final bt = await FileSystemEntity.type(backup.path, followLinks: false),
        at = await FileSystemEntity.type(absent.path, followLinks: false);
    if (bt == FileSystemEntityType.file &&
        at == FileSystemEntityType.notFound) {
      if (!await _owned(backup.path) ||
          ((await backup.stat()).mode & 18) != 0) {
        throw const FileSystemException('backup_unavailable');
      }
      await _replace(file, await backup.readAsBytes(), _canonical(hostName));
    } else if (at == FileSystemEntityType.file &&
        bt == FileSystemEntityType.notFound) {
      if (!await _owned(absent.path) ||
          ((await absent.stat()).mode & 18) != 0 ||
          await absent.length() != 0) {
        throw const FileSystemException('backup_unavailable');
      }
      if (!_same(await _readCurrent(file), _canonical(hostName))) {
        throw const FileSystemException('restore_conflict');
      }
      await file.delete();
    } else {
      throw const FileSystemException('backup_unavailable');
    }
    return inspectBrowser(browser);
  }
}
