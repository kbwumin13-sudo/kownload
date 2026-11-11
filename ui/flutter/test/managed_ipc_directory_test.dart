import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/managed_ipc_directory.dart';
import 'package:path/path.dart' as path;

void main() {
  late Directory root;
  late Directory ipc;
  Future<void> chmod(String target, String mode) async {
    expect((await Process.run('/bin/chmod', [mode, target])).exitCode, 0);
  }

  Future<void> ensure() => ensureManagedIpcDirectory(storageRoot: root.path);
  Matcher reason(String value) => isA<ManagedIpcDirectoryException>().having(
        (e) => e.reason,
        'reason',
        value,
      );

  setUp(() async {
    root = await Directory.systemTemp.createTemp('k14-ipc-');
    ipc = Directory(path.join(root.path, 'ipc'));
    await chmod(root.path, '700');
  });
  tearDown(() async {
    await chmod(root.path, '700');
    await root.delete(recursive: true);
  });

  test('new default IPC becomes 0700 without changing the data root', () async {
    await chmod(root.path, '755');
    final before = await root.stat();
    await ensure();
    expect((await ipc.stat()).mode & 0x1ff, 0x1c0);
    expect((await root.stat()).mode, before.mode);
    expect(await root.list().map((v) => path.basename(v.path)).toList(), [
      'ipc',
    ]);
  });

  test(
    'owned 0755 upgrade preserves live socket and file bytes/metadata',
    () async {
      await ipc.create();
      await chmod(ipc.path, '755');
      final file = await File(
        path.join(ipc.path, 'kept.data'),
      ).writeAsBytes([9, 3, 7], flush: true);
      await chmod(file.path, '640');
      final socketPath = path.join(ipc.path, 'kept.sock');
      final server = await ServerSocket.bind(
        InternetAddress(socketPath, type: InternetAddressType.unix),
        0,
      );
      final listening = server.listen((client) {
        client.listen((bytes) => client.add(bytes), onDone: client.destroy);
      });
      final fileBefore = await file.stat(),
          socketBefore = await File(socketPath).stat();
      try {
        await ensure();
        expect((await ipc.stat()).mode & 0x1ff, 0x1c0);
        final fileAfter = await file.stat(),
            socketAfter = await File(socketPath).stat();
        for (final pair in [
          (fileBefore, fileAfter),
          (socketBefore, socketAfter),
        ]) {
          expect(pair.$2.type, pair.$1.type);
          expect(pair.$2.mode, pair.$1.mode);
          expect(pair.$2.modified, pair.$1.modified);
          expect(pair.$2.changed, pair.$1.changed);
          expect(pair.$2.size, pair.$1.size);
        }
        expect(await file.readAsBytes(), [9, 3, 7]);
        final client = await Socket.connect(
          InternetAddress(socketPath, type: InternetAddressType.unix),
          0,
        );
        client.add([4, 2]);
        expect(await client.first.timeout(const Duration(seconds: 3)), [4, 2]);
        client.destroy();
      } finally {
        await listening.cancel();
        await server.close();
      }
    },
  );

  test('repeated private-directory call leaves metadata unchanged', () async {
    await ensure();
    final before = await ipc.stat();
    await ensure();
    final after = await ipc.stat();
    expect(after.mode, before.mode);
    expect(after.modified, before.modified);
    expect(after.changed, before.changed);
  });

  test(
    'non-directory IPC refuses without changing its bytes or mode',
    () async {
      final file = await File(ipc.path).writeAsString('keep this file');
      final before = await file.stat();
      await expectLater(ensure(), throwsA(reason('directory_conflict')));
      expect((await file.stat()).mode, before.mode);
      expect((await file.stat()).changed, before.changed);
      expect(await file.readAsString(), 'keep this file');
    },
  );

  test('IPC symlink refuses and leaves its target untouched', () async {
    final target = await Directory(path.join(root.path, 'target')).create();
    await chmod(target.path, '755');
    final file = await File(
      path.join(target.path, 'kept'),
    ).writeAsString('kept');
    final link = await Link(ipc.path).create(target.path);
    final before = await target.stat();
    await expectLater(ensure(), throwsA(reason('directory_conflict')));
    expect((await target.stat()).mode, before.mode);
    expect((await target.stat()).changed, before.changed);
    expect(await file.readAsString(), 'kept');
    expect(await link.target(), target.path);
  });

  test('root symlink refuses without creating an IPC child', () async {
    final alias = await Link(path.join(root.path, 'alias')).create(root.path);
    await expectLater(
      ensureManagedIpcDirectory(storageRoot: alias.path),
      throwsA(reason('directory_conflict')),
    );
    expect(await ipc.exists(), false);
    expect(await alias.target(), root.path);
  });

  test('real foreign-owned tmp root refuses before any mutation', () async {
    final own = await Process.run('/usr/bin/id', ['-u']);
    final owner = await Process.run('/usr/bin/stat', [
      '-f',
      '%u',
      '/private/tmp',
    ]);
    expect(own.exitCode, 0);
    expect(owner.exitCode, 0);
    expect(owner.stdout.toString().trim(), isNot(own.stdout.toString().trim()));
    final before = await FileSystemEntity.type(
      '/private/tmp/ipc',
      followLinks: false,
    );
    await expectLater(
      ensureManagedIpcDirectory(storageRoot: '/private/tmp'),
      throwsA(reason('foreign_directory')),
    );
    expect(
      await FileSystemEntity.type('/private/tmp/ipc', followLinks: false),
      before,
    );
  });

  test('group-writable root refuses without repair or mkdir', () async {
    await chmod(root.path, '770');
    await expectLater(ensure(), throwsA(reason('unsafe_root_permissions')));
    expect((await root.stat()).mode & 0x1ff, 0x1f8);
    expect(await ipc.exists(), false);
  });

  test('actual mkdir permission failure propagates without fallback', () async {
    await chmod(root.path, '500');
    await expectLater(ensure(), throwsA(reason('permission_failed')));
    expect(await ipc.exists(), false);
    expect((await root.stat()).mode & 0x1ff, 0x140);
  });

  test('relative or filesystem-root inputs refuse without mutation', () async {
    for (final candidate in ['relative', '/']) {
      await expectLater(
        ensureManagedIpcDirectory(storageRoot: candidate),
        throwsA(reason('invalid_managed_root')),
      );
    }
    expect(await root.list().length, 0);
  });
}
