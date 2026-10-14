import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/downloader_config.dart';

void main() {
  test('saving an unrelated setting preserves native netdisk config over HTTP',
      () async {
    final netdisk = <String, dynamic>{
      'connections': 7,
      'useServerCtime': true,
      'userAgent': 'custom-netdisk-agent',
    };
    final original = DownloaderConfig().toJson();
    (original['protocolConfig'] as Map)['netdisk'] = netdisk;
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    Map<String, dynamic>? saved;
    final subscription = server.listen((request) async {
      if (request.method == 'PUT') {
        saved = jsonDecode(await utf8.decoder.bind(request).join());
      } else {
        await request.drain<void>();
      }
      request.response.headers.contentType = ContentType.json;
      request.response.write(jsonEncode({
        'code': 0,
        'data': request.method == 'GET' ? original : null,
      }));
      await request.response.close();
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    try {
      final config = await api.getConfig();
      config.maxRunning = 5;
      await api.putConfig(config);
      expect(saved?['maxRunning'], 5);
      expect(saved?['protocolConfig']['netdisk'], netdisk);
      expect(DownloaderConfig().toJson()['protocolConfig'],
          isNot(contains('netdisk')));
    } finally {
      await subscription.cancel();
      await server.close(force: true);
    }
  });
}
