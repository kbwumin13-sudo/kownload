import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/rpc/rpc.dart';
import 'package:gopeed/identity/app_identity_rpc.dart';
import 'package:path/path.dart' as path;

class _RealHttp extends HttpOverrides {
  HttpClient client() => super.createHttpClient(null);
}

void main() {
  late Directory root;
  setUp(() async {
    root = await Directory.systemTemp.createTemp('k14-app-identity-');
  });
  tearDown(() async => root.delete(recursive: true));

  test('cold canonical socket fingerprint survives ipc directory creation',
      () async {
    final actual = await Directory(path.join(root.path, 'data')).create();
    final alias = await Link(path.join(root.path, 'alias')).create(actual.path);
    final socket = path.join(alias.path, 'ipc', 'host.sock');
    final expected = sha256
        .convert(utf8.encode(
            path.join(await actual.resolveSymbolicLinks(), 'ipc', 'host.sock')))
        .toString();
    expect(await nativeRouteScope(socket), expected);
    await Directory(path.join(actual.path, 'ipc')).create();
    expect(await nativeRouteScope(socket), expected);
    await expectLater(nativeRouteScope('relative.sock'), throwsFormatException);
  });

  test('identity is read-only, typed and uses actual route/build not body',
      () async {
    final rpc = AppIdentityRpc(
        socket: path.join(root.path, 'ipc', 'host.sock'),
        preview: true,
        sourceCommit: List.filled(40, 'b').join(),
        releaseVersion: '1.9.3-kownload.26');
    final before = await root.list(recursive: true).map((e) => e.path).toList();
    final value = await rpc.command({
      'method': 'hostInfo',
      'params': {'version': 1},
      'preview': false,
      'routeScope': 'spoofed',
      'clientId': 'do-not-register',
      'Cookie': 'private-secret',
      'url': 'https://private.invalid/'
    });
    expect(value['product'], 'kownload');
    expect(value['routeScope'], await nativeRouteScope(rpc.socket));
    expect(value['build'], {
      'sourceCommit': List.filled(40, 'b').join(),
      'releaseVersion': '1.9.3-kownload.26',
      'preview': true
    });
    expect(value.keys.toSet(),
        {'version', 'product', 'identityProtocol', 'build', 'routeScope'});
    expect(jsonEncode(value), isNot(contains(root.path)));
    expect(jsonEncode(value), isNot(contains('private-secret')));
    expect(
        await root.list(recursive: true).map((e) => e.path).toList(), before);
  });

  test('unknown build stays unknown and unsupported requests reject', () async {
    final rpc = AppIdentityRpc(
        socket: path.join(root.path, 'host.sock'),
        preview: false,
        sourceCommit: '/Users/private',
        releaseVersion: 'https://private.invalid/');
    final value = await rpc.command({
      'method': 'hostInfo',
      'params': {'version': 1}
    });
    expect(value['build'], {
      'sourceCommit': 'unknown',
      'releaseVersion': 'unknown',
      'preview': false
    });
    for (final request in <Map<String, dynamic>>[
      {},
      {
        'method': 'sync',
        'params': {'version': 1}
      },
      {
        'method': 'hostInfo',
        'params': {'version': 2}
      },
      {
        'method': 'hostInfo',
        'params': {'version': 1.0}
      },
    ]) {
      await expectLater(rpc.command(request), throwsFormatException);
    }
  });

  test('actual HTTP identity endpoint is typed and rejects business actions',
      () async {
    final rpc = AppIdentityRpc(
        socket: path.join(root.path, 'host.sock'), preview: true);
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((r) => rpc.handle(RpcContext(r, r.response)));
    final client = _RealHttp().client();
    Future<(int, Map)> call(String method, Map body) async {
      final request = await client.openUrl(
          method, Uri.parse('http://127.0.0.1:${server.port}/identity'));
      if (method == 'POST') request.write(jsonEncode(body));
      final response = await request.close();
      return (
        response.statusCode,
        jsonDecode(await utf8.decoder.bind(response).join()) as Map
      );
    }

    try {
      final ok = await call('POST', {
        'method': 'hostInfo',
        'params': {'version': 1}
      });
      expect(ok.$1, 200);
      expect(ok.$2['data']['routeScope'], await nativeRouteScope(rpc.socket));
      final bad = await call('POST', {
        'method': 'create',
        'params': {'Cookie': 'private-secret'}
      });
      expect(bad.$1, 400);
      expect(bad.$2['data'], null);
      expect(jsonEncode(bad.$2), isNot(contains('private-secret')));
      expect((await call('GET', {})).$1, 400);
      expect(await root.list().length, 0);
    } finally {
      client.close(force: true);
      await server.close(force: true);
    }
  });
}
