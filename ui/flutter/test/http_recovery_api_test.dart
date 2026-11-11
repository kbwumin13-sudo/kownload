import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/http_recovery_api.dart';
import 'package:gopeed/api/model/http_recovery.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'support/http_recovery_fixture.dart';

void main() {
  final f = HttpRecoveryFixture();
  final action = HttpRetryAction('user-fixed', 'ep-a', 1);
  HttpRecoveryApi client() => HttpRecoveryApi(
      snapshot: api.activeApiSnapshot!,
      identity:
          DatasetIdentity(version: 1, datasetId: f.dataset, persistent: true),
      taskId: f.taskId);
  setUpAll(f.start);
  setUp(f.reset);
  tearDownAll(f.close);
  test('fixed scope auth and exact explicit request and read-only tuple query',
      () async {
    final c = client();
    expect(
        (await HttpRecoveryApi.readIdentity(c.snapshot))!.datasetId, f.dataset);
    final state = (await c.query()).recovery!;
    expect(state.episode, 'ep-a');
    expect(state.generation, 7);
    expect((await c.retry(action)).recovery!.receipt!.state, 'accepted');
    expect(
        (await c.query(action: action)).recovery!.receipt!.state, 'accepted');
    expect(f.posts, 1);
    final post = f.requests.singleWhere((r) => r['method'] == 'POST');
    expect(post['body'], action.toJson());
    expect((f.requests.last['uri'] as Uri).queryParameters, action.toQuery());
    expect(f.requests.every((r) => r['token'] == 'fixture-scoped-token'), true);
  });
  for (final mode in [
    '409',
    '503',
    'html',
    'redirect',
    'badVersion',
    'wrongDataset',
    'lostAck'
  ]) {
    test('$mode is not an accepted reset and never retries or falls back',
        () async {
      f.replyMode = mode;
      final c = client();
      final result = await c.retry(action);
      expect(result.recovery, isNull);
      expect(f.posts, 1);
      expect(f.requests.length, 1);
      if (const ['409', '503'].contains(mode)) {
        expect(result.statusCode, int.parse(mode));
        expect(result.businessCode, 1000);
      }
    });
  }
  for (final mode in ['stale', 'not_recorded']) {
    test('$mode is query only and preserves original action', () async {
      f.replyMode = mode;
      final result = await client().query(action: action);
      expect(result.recovery!.receipt!.state, mode);
      expect(result.recovery!.receipt!.queryRequired, mode == 'not_recorded');
      expect(f.posts, 0);
    });
  }
  test('legacy and held have no invented managed tuple', () async {
    for (final mode in ['legacy', 'held']) {
      f.replyMode = mode;
      final r = (await client().query()).recovery!;
      expect(r.management, mode);
      expect(r.retryAllowed, false);
      expect(r.episode, isNull);
      expect(r.generation, isNull);
    }
  });
  test('raw whitelist rejects bad method/path/body/query before any IO',
      () async {
    final root = '/api/v1/datasets/${f.dataset}/tasks/${f.taskId}/recovery/v1';
    final s = api.activeApiSnapshot!;
    for (final path in [
      'https://other.invalid$root',
      '$root/../retry',
      '$root\n',
      '$root?x=1'
    ]) {
      await expectLater(
          api.httpRecoveryRequest(s, 'GET', path), throwsArgumentError);
    }
    await expectLater(
        api.httpRecoveryRequest(s, 'PUT', root), throwsArgumentError);
    await expectLater(
        api.httpRecoveryRequest(s, 'POST', root, data: action.toJson()),
        throwsArgumentError);
    for (final bad in [
      {'actionId': 'only'},
      {...action.toQuery(), 'extra': 'no'},
      {...action.toQuery(), 'expectedSourceRevision': '01'},
      {...action.toQuery(), 'expectedSourceRevision': '9007199254740992'}
    ]) {
      await expectLater(
          api.httpRecoveryRequest(s, 'GET', root, queryParameters: bad),
          throwsArgumentError);
    }
    for (final bad in [
      {...action.toJson(), 'extra': 1},
      {...action.toJson(), 'decision': 'continue'},
      {...action.toJson(), 'version': 2},
      {...action.toJson(), 'expectedSourceRevision': 0}
    ]) {
      await expectLater(
          api.httpRecoveryRequest(s, 'POST', '$root/retry', data: bad),
          throwsArgumentError);
    }
    expect(f.requests, isEmpty);
  });
  test('old API snapshot cannot send, changed actual dataset fails read check',
      () async {
    final c = client();
    f.dataset = 'c' * 32;
    expect(await c.sameDataset(), false);
    f.init('d');
    final before = f.requests.length;
    await expectLater(c.retry(action), throwsStateError);
    expect(f.requests.length, before);
  });
  test('strict model rejects unsafe revisions and receipt association', () {
    for (final rev in [0, 1.0, 9007199254740992]) {
      expect(
          () => HttpRetryAction.fromJson(
              {...action.toJson(), 'expectedSourceRevision': rev}),
          throwsFormatException);
    }
    final valid = f.view(action: action.toJson());
    (valid['receipt'] as Map)['actionId'] = 'other';
    expect(() => HttpRecovery.parse(valid, f.taskId, action: action),
        throwsFormatException);
    expect(() => HttpRecovery.parse(f.view(action: action.toJson()), f.taskId),
        throwsFormatException);
    final legacy = f.view(management: 'legacy')..['episode'] = 'guessed';
    expect(() => HttpRecovery.parse(legacy, f.taskId), throwsFormatException);
  });
}
