import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/app/services/browser_handoff.dart';
import 'package:gopeed/app/services/browser_integration.dart';
import 'package:gopeed/app/rpc/rpc.dart';

class _RealHttp extends HttpOverrides {}

void main() {
  test(
      'HTTP prepare ACK loss and App restart query the same durable held task without ordinary create',
      () async {
    final original = HttpOverrides.current;
    HttpOverrides.global = _RealHttp();
    final records = <String, Map<String, dynamic>>{};
    var creates = 0, starts = 0, ordinary = 0;
    final prepared = Completer<void>(), releasePrepareReply = Completer<void>();
    final core = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    core.listen((r) async {
      Map<String, dynamic> data;
      var isPrepare = false;
      if (r.uri.path != '/api/v1/browser/handoff') {
        ordinary++;
        data = {};
      } else if (r.method == 'GET') {
        data = {'version': 1};
      } else {
        final body = jsonDecode(await utf8.decoder.bind(r).join()) as Map;
        final id = body['operationId'];
        if (body['action'] == 'prepare') {
          isPrepare = true;
          records.putIfAbsent(id, () {
            creates++;
            return {
              'version': 1,
              'operationId': id,
              'state': 'prepared',
              'taskId': 'held-one',
              'taskIds': ['held-one'],
              'download': body['download']
            };
          });
        } else if (body['action'] == 'commit' &&
            records[id]?['state'] == 'prepared') {
          records[id]!['state'] = 'committed';
          starts++;
        }
        data = records[id] ??
            {
              'version': 1,
              'operationId': id,
              'state': 'unknown',
              'taskIds': []
            };
      }
      if (isPrepare) {
        if (!prepared.isCompleted) prepared.complete();
        await releasePrepareReply.future;
      }
      r.response.headers.contentType = ContentType.json;
      r.response.write(jsonEncode({'code': 0, 'msg': '', 'data': data}));
      await r.response.close();
      if (data['state'] == 'prepared' && !prepared.isCompleted)
        prepared.complete();
    });
    api.init('tcp', '127.0.0.1:${core.port}', '');
    BrowserHandoff fresh() => BrowserHandoff(
        request: api.browserHandoffRequest,
        preferences: (_, __) => {'enabled': true, 'confirm': false},
        confirm: (_) async {
          fail('no confirmation');
        });
    var app = fresh();
    final bridge = BrowserIntegration(
        read: () => null,
        write: (_) async {
          fail('no Hive write');
        },
        handoffCommand: (value) => app.command(value));
    final host = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    host.listen((r) async {
      try {
        await bridge.handle(RpcContext(r, r.response));
      } catch (_) {
        try {
          await r.response.close();
        } catch (_) {}
      }
    });
    final lostClient = _RealHttp().createHttpClient(null),
        queryClient = _RealHttp().createHttpClient(null);
    final identity = {
      'operationId': 'op-one',
      'clientId': 'client-one',
      'browser': 'chrome'
    };
    final download = {
      'id': 9,
      'startTime': '2026-11-11T00:00:00Z',
      'urlHash': List.filled(64, 'a').join()
    };
    Future<Map> post(Map body) async {
      final q = await queryClient
          .postUrl(Uri.parse('http://127.0.0.1:${host.port}/browser'));
      q.write(jsonEncode(body));
      final r = await q.close();
      return jsonDecode(await utf8.decoder.bind(r).join());
    }

    try {
      final request = await lostClient
          .postUrl(Uri.parse('http://127.0.0.1:${host.port}/browser'));
      request.write(jsonEncode({
        'action': 'handoffPrepare',
        ...identity,
        'download': download,
        'req': {'url': 'https://fixture.test/file'},
        'opts': {}
      }));
      final dropped = () async {
        try {
          final r = await request.close();
          await r.drain<void>();
        } catch (_) {}
      }();
      await prepared.future;
      lostClient.close(force: true);
      releasePrepareReply.complete();
      await dropped;
      app = fresh(); // Drop all App-only accepted state and caches.
      final result = await post({'action': 'handoffQuery', ...identity});
      expect(result['data']['state'], 'prepared');
      expect(result['data']['taskIds'], ['held-one']);
      expect(creates, 1);
      expect(starts, 0);
      expect(ordinary, 0);
      final commit = {
        'action': 'handoffCommit',
        ...identity,
        'taskId': 'held-one',
        'download': download,
        'cancelConfirmed': true,
        'browserState': {'state': 'interrupted', 'error': 'USER_CANCELED'}
      };
      expect((await post(commit))['data']['state'], 'committed');
      expect((await post(commit))['data']['taskIds'], ['held-one']);
      expect(creates, 1);
      expect(starts, 1);
      expect(ordinary, 0);
    } finally {
      lostClient.close(force: true);
      queryClient.close(force: true);
      await host.close(force: true);
      await core.close(force: true);
      HttpOverrides.global = original;
    }
  });
}
