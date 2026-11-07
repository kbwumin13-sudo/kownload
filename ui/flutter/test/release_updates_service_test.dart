import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/util/release_updates.dart';

void main() {
  test(
      'development UI uses core build25 version and only fetches release metadata',
      () async {
    var tag = 'v1.9.3-kownload.8';
    final requests = <String>[];
    final targets = <String?>[];
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final subscription = server.listen((request) async {
      requests.add(request.uri.path);
      targets.add(request.headers.value('X-Target-Uri'));
      await request.drain<void>();
      request.response.headers.contentType = ContentType.json;
      request.response.write(jsonEncode(request.uri.path.endsWith('/info')
          ? {
              'code': 0,
              'data': {'version': '1.9.3-kownload.14'}
            }
          : {'tag_name': tag, 'body': 'Changes'}));
      await request.response.close();
    });
    addTearDown(() async {
      await subscription.cancel();
      await server.close(force: true);
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    expect(await checkUpdate(), isNull);
    tag = 'v1.9.3-kownload.15';
    expect((await checkUpdate())!.version, '1.9.3-kownload.15');
    expect(requests,
        ['/api/v1/info', '/api/v1/proxy', '/api/v1/info', '/api/v1/proxy']);
    expect(
        targets.whereType<String>(),
        everyElement(
            'https://api.github.com/repos/kbwumin13-sudo/kownload/releases/latest'));
  });
}
