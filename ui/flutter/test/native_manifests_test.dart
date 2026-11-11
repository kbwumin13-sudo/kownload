import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/app_identity.dart';
import 'package:gopeed/identity/browser_connection.dart';
import 'package:gopeed/identity/native_manifests.dart';
import 'package:path/path.dart' as path;

void main() {
  late Directory root;
  late String home, storage;
  late NativeManifestService service;
  const asset = [7, 8, 9, 10];
  Future<void> chmod(String p, String mode) async {
    expect((await Process.run('/bin/chmod', [mode, p])).exitCode, 0);
  }

  String directory(String browser) =>
      macNativeHostsDirectory(browser, home: home)!;
  File manifest(String browser, String name) =>
      File(path.join(directory(browser), '$name.json'));
  File backup(String browser, String name, String suffix) => File(
      path.join(storage, 'native-manifest-backups', browser, '$name.$suffix'));
  Future<void> installAsset([List<int> bytes = asset]) async {
    await File(service.executable).writeAsBytes(bytes);
    await chmod(service.executable, '700');
  }

  Future<String> writeManifest(String browser, String name,
      {Map<String, dynamic>? value}) async {
    final file = manifest(browser, name);
    await file.parent.create(recursive: true);
    final text = jsonEncode(value ?? nativeManifest(name, service.executable));
    await file.writeAsString(text);
    return text;
  }

  Future<List<String>> listing() async => (await root
      .list(recursive: true, followLinks: false)
      .map((e) => e.path)
      .toList())
    ..sort();
  setUp(() async {
    root = await Directory.systemTemp.createTemp('k14-manifest-');
    home = path.join(root.path, 'home');
    storage = path.join(root.path, 'data');
    await Directory(home).create();
    await Directory(storage).create();
    await chmod(storage, '700');
    service = NativeManifestService(
        home: home, storageDir: storage, loadAsset: () async => asset);
  });
  tearDown(() async => root.delete(recursive: true));

  test(
      'read-only missing inspection never installs, backs up, or claims a connection',
      () async {
    final before = await listing();
    final report = await service.inspectBrowser('edge');
    expect(report.newHost.owner, 'missing');
    expect(report.configured, false);
    expect(report.native, 'untested');
    expect(report.toJson()['pendingBindingConflict'], null);
    expect(report.toJson()['appReachable'], null);
    expect(await listing(), before);
    expect(await File(service.executable).exists(), false);
  });

  test(
      'Chrome new host works independently; real Gopeed-like alias and Edge stay untouched',
      () async {
    final original = await writeManifest('chrome', AppIdentity.legacyHostName,
        value: nativeManifest(AppIdentity.legacyHostName,
            '/Applications/Gopeed.app/Contents/Helpers/host'));
    final edge = await writeManifest('edge', AppIdentity.hostName,
        value: nativeManifest(AppIdentity.hostName, '/foreign/edge/host'));
    final report = await service.connectBrowser('chrome');
    expect(report.configured, true);
    expect(report.changedHostNames, [AppIdentity.hostName]);
    expect(report.legacyAlias.owner, 'foreign');
    expect(report.native, 'untested');
    expect(await manifest('chrome', AppIdentity.legacyHostName).readAsString(),
        original);
    expect(await manifest('edge', AppIdentity.hostName).readAsString(), edge);
    expect(await backup('chrome', AppIdentity.legacyHostName, 'json').exists(),
        false);
    expect(
        await backup('chrome', AppIdentity.hostName, 'absent').exists(), true);
    expect(await File(service.executable).readAsBytes(), asset);
    final repeat = await service.connectBrowser('chrome');
    expect(repeat.changedHostNames, isEmpty);
  });

  test(
      'explicit missing compatibility alias creates only selected Edge and records absent',
      () async {
    final report =
        await service.connectBrowser('edge', legacyCompatibility: true);
    expect(report.newHost.configured, true);
    expect(report.legacyAlias.configured, true);
    expect(report.changedHostNames,
        [AppIdentity.hostName, AppIdentity.legacyHostName]);
    expect(await Directory(directory('chrome')).exists(), false);
    for (final name in report.changedHostNames) {
      expect(await backup('edge', name, 'absent').exists(), true);
      expect(
          jsonDecode(
              await manifest('edge', name).readAsString())['allowed_origins'],
          contains('chrome-extension://jcalkpjhhaachliihlkojhmafniepnoj/'));
    }
  });

  test(
      'unknown or foreign legacy alias is preserved even with explicit compatibility',
      () async {
    for (final text in [
      'unreadable original',
      jsonEncode(
          nativeManifest(AppIdentity.legacyHostName, '/real/gopeed/host'))
    ]) {
      final file = manifest('chrome', AppIdentity.legacyHostName);
      await file.parent.create(recursive: true);
      await file.writeAsString(text);
      final report =
          await service.connectBrowser('chrome', legacyCompatibility: true);
      expect(report.configured, true);
      expect(report.issues, contains('legacy_alias_preserved'));
      expect(await file.readAsString(), text);
      expect(
          await backup('chrome', AppIdentity.legacyHostName, 'json').exists(),
          false);
    }
  });

  test(
      'path equality does not override bad name, type, allowlist, or foreign ownership',
      () async {
    await installAsset();
    for (final patch in <Map<String, dynamic>>[
      {'name': 'foreign'},
      {'type': 'other'},
      {
        'allowed_origins': ['chrome-extension://foreign/']
      },
      {'path': '/different/host'},
    ]) {
      final original = await writeManifest('edge', AppIdentity.hostName,
          value: {
            ...nativeManifest(AppIdentity.hostName, service.executable),
            ...patch
          });
      final report = await service.connectBrowser('edge');
      expect(report.configured, false);
      expect(report.changedHostNames, isEmpty);
      expect(report.issues, ['new_host_not_written']);
      expect(await manifest('edge', AppIdentity.hostName).readAsString(),
          original);
      expect(
          await backup('edge', AppIdentity.hostName, 'json').exists(), false);
    }
  });

  test(
      'unverified executable bytes are preserved rather than overwritten on pathname alone',
      () async {
    await installAsset([99]);
    final original = await writeManifest('chrome', AppIdentity.hostName);
    final report = await service.inspectBrowser('chrome');
    expect(report.newHost.owner, 'unknown');
    expect(report.newHost.executable, 'mismatch');
    await expectLater(
        service.connectBrowser('chrome'), throwsA(isA<FileSystemException>()));
    expect(await File(service.executable).readAsBytes(), [99]);
    expect(await manifest('chrome', AppIdentity.hostName).readAsString(),
        original);
  });

  test(
      'backup failure changes zero manifests; retry keeps the first exact original',
      () async {
    await installAsset();
    final original = await writeManifest('chrome', AppIdentity.hostName);
    final parent = Directory(path.join(storage, 'native-manifest-backups'));
    await parent.create();
    final blocker =
        await File(path.join(parent.path, 'chrome')).writeAsString('blocked');
    final failed = await service.connectBrowser('chrome');
    expect(failed.changedHostNames, isEmpty);
    expect(failed.issues, ['new_host_not_written']);
    expect(await manifest('chrome', AppIdentity.hostName).readAsString(),
        original);
    await blocker.delete();
    expect((await service.connectBrowser('chrome')).changedHostNames,
        [AppIdentity.hostName]);
    expect(await backup('chrome', AppIdentity.hostName, 'json').readAsString(),
        original);
    await writeManifest('chrome', AppIdentity.hostName, value: {
      ...nativeManifest(AppIdentity.hostName, service.executable),
      'description': 'later local description'
    });
    await service.connectBrowser('chrome');
    expect(await backup('chrome', AppIdentity.hostName, 'json').readAsString(),
        original);
    await service.restoreBrowser('chrome');
    expect(await manifest('chrome', AppIdentity.hostName).readAsString(),
        original);
  });

  test(
      'restore absent touches only specified host; later third-party edits refuse',
      () async {
    final legacy = await writeManifest('edge', AppIdentity.legacyHostName,
        value: nativeManifest(AppIdentity.legacyHostName, '/foreign/host'));
    await service.connectBrowser('edge');
    final restored = await service.restoreBrowser('edge');
    expect(restored.newHost.present, false);
    expect(await manifest('edge', AppIdentity.legacyHostName).readAsString(),
        legacy);
    await service.connectBrowser('edge');
    final changed = await writeManifest('edge', AppIdentity.hostName,
        value: nativeManifest(AppIdentity.hostName, '/third-party/host'));
    await expectLater(
        service.restoreBrowser('edge'), throwsA(isA<FileSystemException>()));
    expect(
        await manifest('edge', AppIdentity.hostName).readAsString(), changed);
  });

  test(
      'symlink manifest and executable never confer ownership or mutate target',
      () async {
    await installAsset();
    final outside = await File(path.join(root.path, 'outside.json'))
        .writeAsString('outside');
    await Directory(directory('chrome')).create(recursive: true);
    final link = await Link(manifest('chrome', AppIdentity.hostName).path)
        .create(outside.path);
    final report = await service.connectBrowser('chrome');
    expect(report.configured, false);
    expect(report.changedHostNames, isEmpty);
    expect(await outside.readAsString(), 'outside');
    expect(await link.target(), outside.path);
    await File(service.executable).delete();
    await Link(service.executable).create(outside.path);
    await expectLater(
        service.connectBrowser('edge'), throwsA(isA<FileSystemException>()));
    expect(await outside.readAsString(), 'outside');
  });

  test('browser directory symlink cannot redirect a new missing manifest',
      () async {
    final dir = Directory(directory('chrome'));
    await dir.parent.create(recursive: true);
    final elsewhere =
        await Directory(path.join(root.path, 'elsewhere')).create();
    await Link(dir.path).create(elsewhere.path);
    final report = await service.connectBrowser('chrome');
    expect(report.changedHostNames, isEmpty);
    expect(report.configured, false);
    expect(await elsewhere.list().length, 0);
  });

  test('concurrent explicit actions serialize and preserve one absent backup',
      () async {
    final reports = await Future.wait(
        [service.connectBrowser('chrome'), service.connectBrowser('chrome')]);
    expect(reports.expand((r) => r.changedHostNames).toList(),
        [AppIdentity.hostName]);
    expect(
        await backup('chrome', AppIdentity.hostName, 'absent').exists(), true);
    expect(
        await backup('chrome', AppIdentity.hostName, 'json').exists(), false);
  });

  test(
      'backup symlinks and edited absent markers refuse without changing manifest',
      () async {
    await installAsset();
    final original = await writeManifest('chrome', AppIdentity.hostName);
    final destination = backup('chrome', AppIdentity.hostName, 'json');
    await destination.parent.create(recursive: true);
    final outside = await File(path.join(root.path, 'outside-backup'))
        .writeAsString('outside');
    final link = await Link(destination.path).create(outside.path);
    expect((await service.connectBrowser('chrome')).changedHostNames, isEmpty);
    expect(await manifest('chrome', AppIdentity.hostName).readAsString(),
        original);
    expect(await outside.readAsString(), 'outside');
    await link.delete();
    await service.connectBrowser('edge');
    final edgeBefore =
        await manifest('edge', AppIdentity.hostName).readAsBytes();
    await backup('edge', AppIdentity.hostName, 'absent')
        .writeAsString('edited');
    await expectLater(
        service.restoreBrowser('edge'), throwsA(isA<FileSystemException>()));
    expect(
        await manifest('edge', AppIdentity.hostName).readAsBytes(), edgeBefore);
  });

  test('unsupported and old no-argument connect refuse without browser changes',
      () async {
    final before = await listing();
    await expectLater(service.connectBrowser('safari'), throwsArgumentError);
    await expectLater(connectBrowsers(), throwsStateError);
    expect(await listing(), before);
    expect(
        () => macManifestDirectory('chrome',
            preview: true, home: home, override: '/Users/fixture/real-browser'),
        throwsStateError);
  });

  test('Firefox manifest constants retain existing fixed extension allowlist',
      () {
    final value =
        nativeManifest(AppIdentity.hostName, '/fixture/host', firefox: true);
    expect(value.containsKey('allowed_origins'), false);
    expect(value['allowed_extensions'], AppIdentity.firefoxExtensionIds);
  });
}
