import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/identity/active_task_batch_route.dart';
import 'package:gopeed/identity/app_identity_rpc.dart';
import 'package:path/path.dart' as path;

void main() {
  late Directory root;
  setUp(() async {
    root = await Directory.systemTemp.createTemp('k14-active-route-');
  });
  tearDown(() async => root.delete(recursive: true));

  test('boot route uses the same canonical reference as actual App identity',
      () async {
    final actual = await Directory(path.join(root.path, 'actual')).create();
    final alias = await Link(path.join(root.path, 'alias')).create(actual.path);
    final socket = path.join(alias.path, 'ipc', 'host.sock');
    final before = await root.list(recursive: true).map((e) => e.path).toList();
    final route = await localBootTaskBatchRoute(socket);
    final identity =
        await AppIdentityRpc(socket: socket, preview: true).command({
      'method': 'hostInfo',
      'params': {'version': 1},
    });
    expect(route!.toJson(), {
      'product': identity['product'],
      'identityProtocol': identity['identityProtocol'],
      'routeScope': identity['routeScope'],
    });
    expect(
        await root.list(recursive: true).map((e) => e.path).toList(), before);
    await Directory(path.join(actual.path, 'ipc')).create();
    expect((await localBootTaskBatchRoute(socket))!.toJson(), route.toJson());
    final other = await localBootTaskBatchRoute(
        path.join(root.path, 'another', 'ipc', 'host.sock'));
    expect(other!.routeScope, isNot(route.routeScope));
  });

  test('unavailable route stays null and does not repair paths', () async {
    expect(await localBootTaskBatchRoute('relative.sock'), isNull);
    final file = File(path.join(root.path, 'ordinary'));
    await file.writeAsBytes([1, 2, 3]);
    expect(await localBootTaskBatchRoute(path.join(file.path, 'host.sock')),
        isNull);
    expect(await file.readAsBytes(), [1, 2, 3]);
    expect(await root.list().length, 1);
  });

  test(
      'boot reference stays bound to the actual client endpoint and generation',
      () async {
    final route =
        await localBootTaskBatchRoute(path.join(root.path, 'ipc', 'host.sock'));
    final address = path.join(root.path, 'ipc', 'api.sock');
    api.init('unix', address, 'synthetic-token', taskBatchRoute: route);
    final first = api.activeApiSnapshot!;
    expect(first.network, 'unix');
    expect(first.address, address);
    expect(first.taskBatchRoute!.toJson(), route!.toJson());
    expect(first.isCurrent, isTrue);
    api.init('unix', '$address.changed', 'synthetic-token',
        taskBatchRoute: route);
    final changed = api.activeApiSnapshot!;
    expect(first.isCurrent, isFalse);
    expect(changed.address, address);
    expect(changed.taskBatchRoute, isNull);
    expect(changed.isCurrent, isTrue);
    expect(await root.list().length, 0);
  });
}
