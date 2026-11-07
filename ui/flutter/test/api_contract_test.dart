import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/api/model/result.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resolve_task.dart';

void main() {
  test('whole-task filters and backend resolve failures survive real HTTP',
      () async {
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final requests = <Uri>[];
    final subscription = server.listen((request) async {
      requests.add(request.uri);
      await request.drain<void>();
      request.response.headers.contentType = ContentType.json;
      request.response.write(jsonEncode(request.uri.path.endsWith('/resolve')
          ? {'code': 1001, 'msg': '获取种子信息超时，请检查网络或稍后重试'}
          : {'code': 0, 'data': null}));
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
      await expectLater(
          api.resolve(
              ResolveTask(req: Request(url: 'magnet:?xt=urn:btih:test'))),
          throwsA(isA<Exception>().having((e) => e.toString(), 'backend reason',
              contains('获取种子信息超时，请检查网络或稍后重试'))));
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
