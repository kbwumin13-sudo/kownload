import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/netdisk.dart';

void main() {
  test('Unix netdisk confirmation accepts a batch taking over 60 seconds',
      () async {
    final directory = await Directory.systemTemp.createTemp('kownload api ');
    final server = await HttpServer.bind(
        InternetAddress('${directory.path}/core.sock',
            type: InternetAddressType.unix),
        0);
    addTearDown(() async {
      await server.close(force: true);
      await directory.delete(recursive: true);
    });
    final received = <String, dynamic>{};
    server.listen((request) async {
      final body = await utf8.decoder.bind(request).join();
      final route = request.uri.path;
      dynamic data = [];
      if (route.endsWith('/prepare')) {
        final payload = jsonDecode(body) as Map<String, dynamic>;
        received.addAll(payload);
        // The production handler permits 300 seconds for an entire batch.
        // This used to hit Dio's generic 60-second response timeout.
        await Future<void>.delayed(const Duration(seconds: 61));
        data = {
          'batchId': 'batch',
          'reqs': [
            {
              'req': {'url': payload['source']['url']},
              'opts': payload['opts']
            }
          ]
        };
      } else if (route.endsWith('/batch')) {
        final payload = jsonDecode(body) as Map<String, dynamic>;
        expect((payload['reqs'] as List).single['opts']['extra']['connections'],
            4);
        data = ['created-task'];
      }
      request.response.headers.contentType = ContentType.json;
      request.response.write(jsonEncode({'code': 0, 'data': data}));
      await request.response.close();
    });
    api.init('unix', '${directory.path}/core.sock', '');
    final client = NetdiskApi();
    expect(await client.providers(), isEmpty);
    final prepared = await client.prepare(
        const NetdiskSource(
            provider: 'quark',
            url: 'https://pan.quark.cn/s/test',
            shareId: 'test'),
        'session',
        [const NetdiskFile(id: 'file', name: '文件.mov', size: 100)],
        Options(path: '/downloads', extra: OptsExtraHttp(connections: 4)),
        proxy: RequestProxy(mode: RequestProxyMode.follow));
    expect(await client.createBatch(prepared), ['created-task']);
    expect(received['source']['shareId'], 'test');
    expect(received['files'].single['name'], '文件.mov');
    expect(received['proxy']['mode'], 'follow');
    expect(received['opts']['extra']['connections'], 4);
  }, timeout: const Timeout(Duration(seconds: 90)));
}
