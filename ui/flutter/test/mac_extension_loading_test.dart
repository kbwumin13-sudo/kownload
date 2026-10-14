import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/extension.dart';
import 'package:gopeed/app/modules/extension/controllers/extension_controller.dart';

void main() {
  test(
      'macOS initialization requests only installed extensions; update checks are explicit',
      () async {
    if (!Platform.isMacOS) return;
    final extension = Extension(
        identity: 'sample',
        name: 'sample',
        author: 'Example',
        title: 'Sample',
        description: '',
        icon: '',
        version: '1.0.0',
        homepage: '',
        repository: null,
        disabled: false,
        devMode: false,
        devPath: '');
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final requests = <String>[];
    final subscription = server.listen((request) async {
      requests.add(request.uri.path);
      await request.drain<void>();
      request.response.headers.contentType = ContentType.json;
      request.response.write(jsonEncode({
        'code': 0,
        'data': request.uri.path.endsWith('/update')
            ? {'newVersion': '2.0.0'}
            : [extension.toJson()]
      }));
      await request.response.close();
    });
    addTearDown(() async {
      await subscription.cancel();
      await server.close(force: true);
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    final controller = ExtensionController();
    await controller.onInit();
    expect(controller.listFilter.value, ExtensionListFilter.installed);
    expect(controller.installedExtensions.single.identity, 'sample');
    expect(controller.updateFlags, isEmpty);
    expect(controller.storePagination.value, isNull);
    expect(requests, ['/api/v1/extensions']);

    await controller.loadInitialData();
    expect(requests, ['/api/v1/extensions', '/api/v1/extensions']);
    await controller.loadInstalled(refreshUpdates: true);
    expect(requests.skip(2),
        ['/api/v1/extensions', '/api/v1/extensions/sample/update']);
    expect(controller.updateFlags['sample'], '2.0.0');
    expect(requests.any((path) => path.contains('proxy')), isFalse);
  });
}
