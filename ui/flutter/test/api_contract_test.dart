import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/api/model/result.dart';

void main() {
  test('whole-task actions omit an absent ID filter over real HTTP', () async {
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final requests = <Uri>[];
    final subscription = server.listen((request) async {
      requests.add(request.uri);
      await request.drain<void>();
      request.response.headers.contentType = ContentType.json;
      request.response.write(jsonEncode({'code': 0, 'data': null}));
      await request.response.close();
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    try {
      await api.continueAllTasks(null);
      await api.pauseAllTasks(null);
      await api.deleteTasks(null, false);
      expect(requests.length, 3);
      for (final uri in requests) {
        expect(uri.queryParameters, isNot(contains('id')), reason: '$uri');
      }
      await api.continueAllTasks(['first', 'second']);
      expect(requests.last.queryParametersAll['id'], ['first', 'second']);
    } finally {
      await subscription.cancel();
      await server.close(force: true);
    }
  });

  test('backend failures expose the code and message without response data',
      () {
    final result = Result<void>(code: 1001, msg: '任务不存在');
    expect(Exception(result).toString(), contains('任务不存在'));
    expect(Exception(result).toString(), contains('1001'));
  });

  test('media settings preserve an explicit JavaScript runtime path', () {
    final config = MediaConfig.fromJson({
      'ytDlpPath': '/fixed/yt-dlp',
      'ffmpegPath': '/fixed/ffmpeg',
      'jsRuntimePath': '/fixed/node',
      'connections': 4,
    });
    expect(config.toJson()['jsRuntimePath'], '/fixed/node');
    expect(MediaConfig.fromJson({}).toJson(), isNot(contains('jsRuntimePath')));
  });
}
