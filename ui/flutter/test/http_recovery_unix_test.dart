import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/http_recovery_api.dart';
import 'package:gopeed/api/model/http_recovery.dart';
import 'package:gopeed/database/task_batch_journal.dart';
import 'support/http_recovery_fixture.dart';

void main() {
  test('recovery read and explicit POST share original Unix adapter and auth',
      () async {
    final dir = Directory('/tmp').createTempSync('k16-retry-unix-');
    final path = '${dir.path}/api.sock';
    final server = await HttpServer.bind(
        InternetAddress(path, type: InternetAddressType.unix), 0);
    final shapes = <String>[];
    final fixture = HttpRecoveryFixture();
    final subscription = server.listen((q) async {
      shapes.add('${q.method} ${q.uri.path}');
      expect(q.headers.value('X-Api-Token'), 'synthetic-unix-token');
      final text = await utf8.decoder.bind(q).join();
      final action = text.isEmpty ? null : jsonDecode(text) as Map;
      final data = q.uri.path == '/api/v1/dataset/identity'
          ? {'version': 1, 'datasetId': 'a' * 32, 'persistent': true}
          : {
              'scopeVersion': 1,
              'datasetId': 'a' * 32,
              'recovery': fixture.view(action: action)
            };
      q.response.headers.contentType = ContentType.json;
      q.response.write(jsonEncode({'code': 0, 'msg': '', 'data': data}));
      await q.response.close();
    });
    try {
      api.init('unix', path, 'synthetic-unix-token',
          taskBatchRoute: JournalRouteRef(
              product: 'kownload', identityProtocol: 1, routeScope: 'b' * 64));
      final snapshot = api.activeApiSnapshot!;
      final identity = (await HttpRecoveryApi.readIdentity(snapshot))!;
      final client = HttpRecoveryApi(
          snapshot: snapshot, identity: identity, taskId: fixture.taskId);
      expect((await client.query()).recovery!.episode, 'ep-a');
      expect(
          (await client.retry(HttpRetryAction('user-unix', 'ep-a', 1)))
              .recovery!
              .receipt!
              .state,
          'accepted');
      expect(shapes.length, 3);
      expect(shapes.where((s) => s.startsWith('POST')).length, 1);
    } finally {
      await subscription.cancel();
      await server.close(force: true);
      dir.deleteSync(recursive: true);
    }
  });
}
