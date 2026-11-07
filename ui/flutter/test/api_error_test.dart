import 'dart:io';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resolve_task.dart';

// Exercise the public API through Dio's IO adapter without waiting 60 seconds.
class _FailingHttpClient extends Fake implements HttpClient {
  DioExceptionType failure = DioExceptionType.receiveTimeout;

  @override
  Future<HttpClientRequest> openUrl(String method, Uri url) async {
    throw DioException(
        requestOptions: RequestOptions(path: url.toString()), type: failure);
  }

  @override
  dynamic noSuchMethod(Invocation invocation) => null;
}

void main() {
  final client = _FailingHttpClient();
  const magnet = 'magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567';
  setUpAll(() {
    api.init('tcp', '127.0.0.1:9999', '');
  });

  Future<void> resolveWithFailure(
          DioExceptionType type, String url, Matcher matcher) =>
      HttpOverrides.runZoned(() async {
        client.failure = type;
        await expectLater(
            api.resolve(ResolveTask(req: Request(url: url))), throwsA(matcher));
      }, createHttpClient: (_) => client);

  test('waiting for magnet resolve reports a metadata timeout', () async {
    await resolveWithFailure(
        DioExceptionType.receiveTimeout,
        magnet,
        isA<api.TimeoutException>()
            .having((e) => e.toString(), 'message', '获取种子信息超时，请检查网络或稍后重试'));
  });
  test('send timeout reports that the request could not be sent', () async {
    await resolveWithFailure(
        DioExceptionType.sendTimeout,
        magnet,
        isA<api.TimeoutException>()
            .having((e) => e.toString(), 'message', '发送请求超时，请检查下载服务连接或稍后重试'));
  });
  for (final type in [
    DioExceptionType.connectionTimeout,
    DioExceptionType.connectionError
  ]) {
    test('$type reports a service connection problem instead of peer timeout',
        () async {
      await resolveWithFailure(
          type,
          magnet,
          isA<api.ConnectionException>().having(
              (e) => e.toString(), 'message', '无法连接下载服务，请检查服务是否运行及连接设置'));
    });
  }
  test('ordinary URL timeout does not claim torrent metadata timed out',
      () async {
    await resolveWithFailure(
        DioExceptionType.receiveTimeout,
        'https://example.com/archive.zip',
        isA<api.TimeoutException>()
            .having((e) => e.toString(), 'message', '解析下载链接超时，请检查网络或稍后重试'));
  });
  test('task actions use the generic request timeout message', () async {
    await HttpOverrides.runZoned(() async {
      client.failure = DioExceptionType.receiveTimeout;
      await expectLater(
          api.pauseTask('task'),
          throwsA(isA<api.TimeoutException>()
              .having((e) => e.toString(), 'message', '请求超时，请稍后重试')));
    }, createHttpClient: (_) => client);
  });
}
