import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/extension.dart';
import 'package:gopeed/app/modules/extension/controllers/extension_controller.dart';

void main() {
  test('manual install, settings and update never call the old market proxy',
      () async {
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final requests = <String>[];
    final subscription = server.listen((request) async {
      requests.add('${request.method} ${request.uri.path}');
      await request.drain<void>();
      request.response.headers.contentType = ContentType.json;
      final ext = Extension(
          identity: 'fixture',
          name: 'fixture',
          author: 'Fixture',
          title: 'Fixture',
          description: '',
          icon: '',
          version: '1.0.0',
          homepage: '',
          repository: null,
          disabled: false,
          devMode: false,
          devPath: '');
      final data = request.uri.path.endsWith('/update') &&
              request.method == 'GET'
          ? {'newVersion': '2.0.0'}
          : request.uri.path == '/api/v1/extensions' && request.method == 'POST'
              ? 'fixture'
              : request.method == 'GET'
                  ? [ext.toJson()]
                  : null;
      request.response.write(jsonEncode({'code': 0, 'data': data}));
      await request.response.close();
    });
    addTearDown(() async {
      await subscription.cancel();
      await server.close(force: true);
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    final controller = ExtensionController();
    await controller.loadInitialData();
    await controller.installFromUrl('https://github.com/example/fixture');
    await Future<void>.delayed(const Duration(milliseconds: 30));
    await controller.upgradeExtension(controller.installedExtensions.single);
    await Future<void>.delayed(const Duration(milliseconds: 30));
    expect(controller.installedExtensions.single.identity, 'fixture');
    expect(requests, contains('POST /api/v1/extensions'));
    expect(requests, contains('POST /api/v1/extensions/fixture/update'));
    expect(
        requests.every((path) => path.contains('/api/v1/extensions')), isTrue);
    expect(
        requests
            .any((path) => path.contains('proxy') || path.contains('install/')),
        isFalse);
  });
}
