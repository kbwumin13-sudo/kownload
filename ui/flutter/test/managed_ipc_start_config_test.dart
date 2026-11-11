import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:dart_ipc/dart_ipc.dart';
import 'package:dart_ipc/src/dart_ipc_platform_interface.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/identity/managed_ipc_directory.dart';
import 'package:gopeed/app/rpc/rpc.dart';
import 'package:gopeed/util/util.dart';
import 'package:path/path.dart' as path;

// Official Unix implementation, with real listeners tracked for cleanup.
class _TrackedUnix extends DartIpcUnix {
  final sockets = <ServerSocket>[];

  @override
  Future<ServerSocket> bind(String address) async {
    final socket = await super.bind(address);
    sockets.add(socket);
    return socket;
  }
}

class _RealHttp extends HttpOverrides {
  HttpClient client() => super.createHttpClient(null);
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  late DartIpcPlatform originalIpc;
  late _TrackedUnix unix;
  setUp(() {
    originalIpc = DartIpcPlatform.instance;
    unix = _TrackedUnix();
    DartIpcPlatform.instance = unix;
  });
  tearDown(() async {
    for (final socket in unix.sockets) {
      await socket.close();
    }
    DartIpcPlatform.instance = originalIpc;
  });

  test(
    'failed default IPC preparation never caches a usable launch config',
    () async {
      final root = await Directory.systemTemp.createTemp('k14-ipc-cfg-');
      try {
        await Util.initStorageDir(previewStorage: root.path);
        final blocker = await File(
          path.join(root.path, 'ipc'),
        ).writeAsString('kept, never a socket directory');
        final app = AppController();
        final original = app.startConfig.value;
        original.startupTaskPolicy = 'imported-review';
        original.network = 'unix';
        original.address = path.join(root.path, 'custom.sock');
        for (var attempt = 0; attempt < 2; attempt++) {
          await expectLater(
            app.loadStartConfig(),
            throwsA(isA<ManagedIpcDirectoryException>()),
          );
          expect(identical(app.startConfig.value, original), true);
          expect(original.startupTaskPolicy, 'imported-review');
          expect(original.network, 'unix');
          expect(original.address, path.join(root.path, 'custom.sock'));
          expect(
            await blocker.readAsString(),
            'kept, never a socket directory',
          );
        }
      } finally {
        await root.delete(recursive: true);
      }
    },
  );

  for (final conflict in ['ipc-link', 'unsafe-root']) {
    test(
      'core rejection also blocks the later host RPC binding: $conflict',
      () async {
        final root = await Directory.systemTemp.createTemp('k11-ipc-start-');
        final target = await Directory.systemTemp.createTemp('k11-ipc-target-');
        try {
          await Util.initStorageDir(previewStorage: root.path);
          final sentinel = await File(
            path.join(target.path, 'keep'),
          ).writeAsString('unchanged target');
          final sentinelBefore = await sentinel.stat();
          final ipc = path.join(root.path, 'ipc');
          if (conflict == 'ipc-link') {
            await Link(ipc).create(target.path);
          } else {
            final result = await Process.run('/bin/chmod', ['770', root.path]);
            expect(result.exitCode, 0);
          }
          final app = AppController();
          await expectLater(
            app.loadStartConfig(),
            throwsA(isA<ManagedIpcDirectoryException>()),
          );
          // This is the real binding entry called by onReady -> _initRpcServer
          // even after main's core-init catch. No mocked mkdir, unlink or bind.
          for (var attempt = 0; attempt < 2; attempt++) {
            Object? failure;
            try {
              await startRpcServer();
            } catch (error) {
              failure = error;
            }
            expect(unix.sockets, isEmpty,
                reason: 'unsafe directory reached the actual Unix bind');
            expect(failure, isA<ManagedIpcDirectoryException>());
          }
          expect(
            await FileSystemEntity.type(
              path.join(root.path, 'ipc', 'host.sock'),
              followLinks: false,
            ),
            FileSystemEntityType.notFound,
          );
          expect(
            await FileSystemEntity.type(
              path.join(target.path, 'host.sock'),
              followLinks: false,
            ),
            FileSystemEntityType.notFound,
          );
          expect(await sentinel.readAsString(), 'unchanged target');
          expect((await sentinel.stat()).modified, sentinelBefore.modified);
          expect(
            (await target.list().toList()).map((e) => path.basename(e.path)),
            ['keep'],
          );
          if (conflict == 'ipc-link') {
            expect(await Link(ipc).target(), target.path);
          } else {
            expect(
              await FileSystemEntity.type(ipc, followLinks: false),
              FileSystemEntityType.notFound,
            );
            expect((await root.stat()).mode & 0x1ff, 0x1f8);
          }
        } finally {
          await Process.run('/bin/chmod', ['700', root.path]);
          await root.delete(recursive: true);
          await target.delete(recursive: true);
        }
      },
    );
  }

  test('safe directory and repaired retry serve real host RPC without core',
      () async {
    final root = await Directory.systemTemp.createTemp('k11-ipc-retry-');
    try {
      await Util.initStorageDir(previewStorage: root.path);
      final ipc = path.join(root.path, 'ipc');
      final blocker = await File(ipc).writeAsString('fixable conflict');
      await expectLater(
          startRpcServer(), throwsA(isA<ManagedIpcDirectoryException>()));
      await blocker.delete();
      // No Go core is started, or required to succeed. The directory safety
      // prerequisite is retried; failures are not a permanent startup latch.
      for (var attempt = 0; attempt < 2; attempt++) {
        await startRpcServer({
          '/fixture': (ctx) => ctx.writeJSON({'available': true}),
        });
        expect((await Directory(ipc).stat()).mode & 0x1ff, 0x1c0);
        final socket = path.join(ipc, 'host.sock');
        final client = _RealHttp().client();
        client.connectionFactory = (uri, proxyHost, proxyPort) =>
            Socket.startConnect(
                InternetAddress(socket, type: InternetAddressType.unix), 0);
        try {
          final response =
              await (await client.getUrl(Uri.parse('http://fixture/fixture')))
                  .close();
          expect(response.statusCode, 200);
          expect(jsonDecode(await utf8.decoder.bind(response).join()),
              {'available': true});
        } finally {
          client.close(force: true);
        }
        await unix.sockets.last.close();
      }
    } finally {
      await root.delete(recursive: true);
    }
  });
}
