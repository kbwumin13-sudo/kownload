import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/rpc/browser_submission.dart';
import 'package:gopeed/app/rpc/rpc.dart';
import 'package:gopeed/app/services/media_path_submission.dart';

import 'support/scoped_task_batch_fixture.dart';

void main() {
  test('media path keeps encoded operation and only queries after lost reply',
      () async {
    final f = ScopedBatchFixture();
    await f.start();
    addTearDown(f.close);
    var badCode = true, stored = false, posts = 0, claims = 0;
    const id = 'media:original';
    final request = {
      'req': {
        'url': 'https://fixture.invalid/watch',
        'protocol': 'media',
        'extra': {
          'format': 'v+a',
          'header': {'Referer': 'https://fixture.invalid'}
        }
      },
      'opts': {'path': '/owned/root', 'name': 'chosen.mp4'}
    };
    f.override = (q) async {
      if (q.uri.path == '/api/v1/dataset/identity') return f.handle(q);
      if (q.uri.path.endsWith('/capability')) {
        await f.respond(q, {
          'scopeVersion': 1,
          'datasetId': f.dataset,
          'kind': MediaPathSubmission.kind,
          'requestVersion': 1,
          'supported': true,
          'requiredCapabilities': [MediaPathSubmission.capability],
          'methods': {
            'create': 'POST',
            'query': 'GET',
            'cancel': 'POST',
            'preview': 'POST'
          }
        });
      } else if (q.method == 'POST') {
        posts++;
        final body = f.requests.last['body'] as Map;
        expect(body['operationId'], id);
        expect(body['request'], request);
        expect(body['pathIntent'],
            {'version': 1, 'path': '/owned/root', 'origin': 'user_selected'});
        stored = true;
        // A lost client acknowledgement, not a Bolt commit fault injection.
        await f.respond(q, null, status: 503, code: 1000);
      } else {
        expect(q.uri.pathSegments.last, id);
        final data = {
          ...f.wrapper(
              f.receipt(id, state: stored ? 'settled' : 'unknown', count: 1)),
          'kind': MediaPathSubmission.kind,
          'requiredCapabilities':
              stored ? [MediaPathSubmission.capability] : <String>[],
        };
        q.response.headers.contentType = ContentType.json;
        q.response.write(jsonEncode({'code': badCode ? 0.0 : 0, 'data': data}));
        await q.response.close();
      }
    };
    final service = MediaPathSubmission(
        ready: () => true,
        claim: ({
          required sessionId,
          required commandId,
          required operationId,
          required request,
        }) {
          claims++;
          expect(sessionId, 'session-1');
          expect(commandId, 'media-1');
          expect(operationId, id);
          return {
            'version': 1,
            'path': '/owned/root',
            'origin': 'user_selected'
          };
        });
    final rpc = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    final subscription = rpc.listen((q) => service
        .handle(RpcContext(q, q.response), create: q.uri.path == '/create'));
    addTearDown(() async {
      await subscription.cancel();
      await rpc.close(force: true);
    });
    final client = HttpClient();
    addTearDown(() => client.close(force: true));
    Future<(int, Map)> call(String path) async {
      final q =
          await client.postUrl(Uri.parse('http://127.0.0.1:${rpc.port}$path'));
      q.headers.set(operationHeader, id);
      q.headers.set('X-Kownload-Host-Meta',
          jsonEncode({'sessionId': 'session-1', 'commandId': 'media-1'}));
      q.write(jsonEncode(path == '/create' ? request : {}));
      final response = await q.close();
      return (
        response.statusCode,
        jsonDecode(await utf8.decoder.bind(response).join()) as Map
      );
    }

    expect((await call('/query')).$1, 503);
    expect(posts, 0);
    expect(claims, 0);
    badCode = false;
    expect((await call('/create')).$1, 503);
    expect(posts, 1);
    expect(claims, 2);
    final query = await call('/query');
    expect(query.$1, 200);
    expect(query.$2['submission'], {
      'state': 'created',
      'taskIds': ['$id-task-0']
    });
    expect((await call('/create')).$1, 503);
    expect(posts, 1);
  });
}
