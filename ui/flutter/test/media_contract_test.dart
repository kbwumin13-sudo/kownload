import 'dart:convert';
import 'dart:io';

import 'package:dio/dio.dart' show CancelToken;
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/api/model/media_inspection.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resolve_task.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/rpc/rpc.dart';

void main() {
  test('web media uses the original resolve, inspect and discard routes',
      () async {
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final requests = <String>[];
    final inspectBodies = <Map>[];
    final subscription = server.listen((request) async {
      final path = request.uri.path;
      requests.add('${request.method} $path');
      final String body = await utf8.decoder.bind(request).join();
      request.response.headers.contentType = ContentType.json;
      if (path == '/api/v1/media/inspect') {
        inspectBodies.add(jsonDecode(body) as Map);
        request.response.write(jsonEncode({
          'code': 1,
          'msg': inspectBodies.length == 1
              ? '视频解析工具无法启动，请检查设置中的工具路径'
              : 'Unexpected extractor output, signed URL ?secret=private',
        }));
      } else {
        request.response.write(jsonEncode({
          'code': 0,
          'data': path == '/api/v1/resolve'
              ? {
                  'id': 'page-rid',
                  'httpInlineHtml': true,
                  'res': {'files': []},
                }
              : null,
        }));
      }
      await request.response.close();
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    final snapshot = api.activeApiSnapshot!;
    final task = ResolveTask(
        req: Request(
            url: 'https://fixture.test/watch',
            protocol: 'media',
            extra: {'sourceType': 'pasted-page'}));
    try {
      final page =
          await api.resolve(ResolveTask(req: Request(url: task.req!.url)));
      expect(page.httpInlineHtml, isTrue);
      final known =
          await api.inspectWebMedia(task, CancelToken(), snapshot: snapshot);
      expect(known.issue, MediaPreviewIssue.tools);
      final unknown =
          await api.inspectWebMedia(task, CancelToken(), snapshot: snapshot);
      expect(unknown.issue, MediaPreviewIssue.unavailable);
      expect(
          inspectBodies,
          [task.toJson(), task.toJson()]
              .map((body) => jsonDecode(jsonEncode(body)))
              .toList());
      await api.discardResolvedTask(page.id, snapshot: snapshot);
      expect(requests, [
        'POST /api/v1/resolve',
        'POST /api/v1/media/inspect',
        'POST /api/v1/media/inspect',
        'DELETE /api/v1/resolve/page-rid',
      ]);
    } finally {
      await subscription.cancel();
      await server.close(force: true);
    }
  });

  test('old config gains media defaults and settings round-trip retains paths',
      () {
    final cfg = DownloaderConfig();
    final old = cfg.toJson();
    (old['protocolConfig'] as Map).remove('media');
    final restored = DownloaderConfig.fromJson(old);
    expect(restored.protocolConfig.media.connections, 4);
    restored.protocolConfig.media.ytDlpPath = '/local/yt-dlp';
    restored.protocolConfig.media.ffmpegPath = '/local/ffmpeg';
    final saved = DownloaderConfig.fromJson(restored.toJson());
    expect(saved.protocolConfig.media.ytDlpPath, '/local/yt-dlp');
    expect(saved.protocolConfig.media.ffmpegPath, '/local/ffmpeg');
  });

  test('media request selection and merge phase survive serialization', () {
    final req = Request(
        url: 'https://example.test/play?sign=a+b',
        protocol: 'media',
        extra: {'format': 'video+audio'});
    final parsed = Request.fromJson(req.toJson());
    expect(parsed.protocol, 'media');
    expect(parsed.url, req.url);
    final old = Request.fromJson({'url': 'https://example.test/image.jpg'});
    expect(old.protocol, isNull);
    expect(old.toJson(), isNot(contains('protocol')));
    final progress = Progress(
        used: 0,
        speed: 0,
        downloaded: 4,
        uploadSpeed: 0,
        uploaded: 0,
        mediaPhase: 'merge');
    expect(Progress.fromJson(progress.toJson()).mediaPhase, 'merge');
    final legacy = progress.toJson()..remove('mediaPhase');
    expect(Progress.fromJson(legacy).mediaPhase, '');
  });

  test('native forwarding decodes Chinese and emoji across UTF-8 chunks',
      () async {
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final requestFuture = server.first;
    final client = HttpClient();
    final post =
        await client.postUrl(Uri.parse('http://127.0.0.1:${server.port}'));
    final bytes = utf8.encode(jsonEncode({'title': '视频 🎬', 'name': '测试.mp4'}));
    post.add(bytes.sublist(0, 12));
    post.add(bytes.sublist(12));
    final response = post.close();
    final incoming = await requestFuture;
    final ctx = RpcContext(incoming, incoming.response);
    expect((await ctx.readJSON())['title'], '视频 🎬');
    expect(jsonDecode(await ctx.readText())['name'], '测试.mp4');
    await incoming.response.close();
    await (await response).drain<void>();
    client.close(force: true);
    await server.close(force: true);
  });
}
