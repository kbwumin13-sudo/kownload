import 'dart:async';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/create_task_batch.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/app/modules/task/views/http_completion_status.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'package:gopeed/api/verified_http_api.dart';
import 'support/scoped_task_batch_fixture.dart';

void main() {
  final f = ScopedBatchFixture();
  final op = TaskBatchOperationRef(
      namespace: TaskBatchOperationRef.namespaceV1, operationId: 'sha:op');
  VerifiedHttpApi bound() => VerifiedHttpApi(
      snapshot: api.activeApiSnapshot!,
      identity:
          DatasetIdentity(version: 1, datasetId: f.dataset, persistent: true),
      expectedSHA256: 'a' * 64);
  CreateTaskBatch payload(
          {String url = 'https://fixture.invalid/one', Object? extra}) =>
      CreateTaskBatch(reqs: [
        CreateTaskBatchItem(
            req: Request(url: url),
            opts: Options(name: 'one.bin', path: '/tmp/fixture', extra: extra))
      ]);
  setUpAll(f.start);
  setUp(() {
    f.reset();
    f.verified = true;
  });
  tearDownAll(f.close);
  test('exact capability and guarded wire use original authenticated client',
      () async {
    final c = bound();
    expect(await c.supported(), true);
    expect((await c.query(op)).receipt!.state, 'unknown');
    final reply = await c.create(op, payload(extra: OptsExtraHttp()));
    expect(reply.createdTaskIds, ['sha:op-task-0']);
    expect(reply.requiresQuery, false);
    await c.query(op);
    await c.cancel(op);
    expect(f.posts, 2);
    expect(f.requests.every((r) => r['token'] == 'fixture-scoped-token'), true);
    final body =
        f.requests.firstWhere((r) => r['method'] == 'POST')['body'] as Map;
    expect(body.keys.toSet(), {
      'version',
      'requiredCapability',
      'operationId',
      'request',
      'integrity'
    });
    expect(body['version'], 1);
    expect(body['operationId'], op.operationId);
    expect(body['requiredCapability'], verifiedHttpCapability);
    expect(
        body['integrity'], {'algorithm': 'sha256', 'expectedSHA256': 'a' * 64});
    expect(body['request'].keys.toSet(), {'req', 'opts'});
    expect(body['request']['opts']['extra']['autoExtract'], isNull);
    expect(body['request']['opts']['extra']['autoTorrent'], isNull);
    expect(
        f.requests
            .every((r) => (r['uri'] as Uri).path.contains('verified-http')),
        true);
    expect((f.requests[1]['uri'] as Uri).toString(), contains('sha%3Aop'));
  });
  for (final bad in [
    'html',
    'version',
    'scopeVersion',
    'projectionVersion',
    'dataset',
    'capability',
    'supported',
    'methods'
  ]) {
    test('preflight $bad is unsupported and sends no POST', () async {
      f.override = (q) async {
        if (bad == 'html') {
          q.response.statusCode = 404;
          q.response.write('<html>old</html>');
          await q.response.close();
          return;
        }
        final d = <String, dynamic>{
          'scopeVersion': 1,
          'datasetId': f.dataset,
          'capability': verifiedHttpCapability,
          'requestVersion': 1,
          'projectionVersion': 1,
          'supported': true,
          'methods': {'create': 'POST', 'query': 'GET', 'cancel': 'POST'}
        };
        if (bad == 'version') d['requestVersion'] = 2;
        if (bad == 'scopeVersion') d['scopeVersion'] = 1.0;
        if (bad == 'projectionVersion') d.remove('projectionVersion');
        if (bad == 'dataset') d['datasetId'] = 'c' * 32;
        if (bad == 'capability') d['capability'] = 'other';
        if (bad == 'supported') d['supported'] = false;
        if (bad == 'methods') d['methods'] = {'create': 'POST', 'query': 'GET'};
        await f.respond(q, d);
      };
      expect(await bound().supported(), false);
      expect(f.posts, 0);
    });
  }
  test('bad/multi/non-direct/unsupported options fail before IO', () async {
    for (final bad in ['', 'A' * 64, 'a' * 63, 'a' * 64 + '\n']) {
      expect(
          () => VerifiedHttpApi(
              snapshot: api.activeApiSnapshot!,
              identity: bound().identity,
              expectedSHA256: bad),
          throwsArgumentError);
    }
    final c = bound();
    for (final value in [
      payload(url: 'magnet:?x'),
      payload(url: 'https:///missing'),
      CreateTaskBatch(reqs: [...payload().reqs!, ...payload().reqs!]),
      CreateTaskBatch(reqs: [
        CreateTaskBatchItem(
            req: Request(url: 'https://fixture.invalid/one'),
            opts: Options(selectFiles: [0, 1]))
      ]),
      payload(extra: OptsExtraHttp(autoTorrent: true)),
      payload(extra: OptsExtraHttp(autoExtract: true))
    ]) {
      expect(() => c.create(op, value), throwsArgumentError);
    }
    expect(f.requests, isEmpty);
  });
  for (final mode in ['partial', 'lostAck', 'html', 'wrongDomain']) {
    test('$mode cannot replay POST and query stays on original prefix',
        () async {
      f.mode = mode;
      final c = bound();
      final reply = await c.create(op, payload());
      expect(reply.requiresQuery, true);
      expect(reply.createdTaskIds,
          mode == 'partial' ? ['sha:op-task-0'] : isEmpty);
      f.mode = 'success';
      final query = await c.query(op);
      expect(query.createdTaskIds, ['sha:op-task-0']);
      expect(f.posts, 1);
    });
  }
  test('S2-only or altered wrapper never contributes verified IDs', () async {
    for (final change in ['capability', 'dataset', 'version', 'op']) {
      final w = f.wrapper(f.receipt(op.operationId, count: 1));
      if (change == 'capability') w.remove('capability');
      if (change == 'dataset') w['datasetId'] = 'c' * 32;
      if (change == 'version') w['scopeVersion'] = 2;
      if (change == 'op') w['operation'] = f.receipt('other', count: 1);
      f.override = (q) => f.respond(q, w);
      final result = await bound().query(op);
      expect(result.createdTaskIds, isEmpty);
      expect(result.requiresQuery, true);
    }
  });
  for (final status in [301, 302, 303, 307, 308]) {
    test('capability/actions refuse $status without redirect or replay',
        () async {
      f.override = (q) async {
        q.response.statusCode = status;
        q.response.headers.set('location', '/not-followed');
        await q.response.close();
      };
      final c = bound();
      expect(await c.supported(), false);
      for (final reply in [
        await c.create(op, payload()),
        await c.query(op),
        await c.cancel(op)
      ]) {
        expect(reply.createdTaskIds, isEmpty);
        expect(reply.requiresQuery, true);
      }
      expect(f.requests.length, 4);
      expect(f.requests.any((r) => (r['uri'] as Uri).path == '/not-followed'),
          false);
    });
  }
  test(
      'fixed routes reject illegal method, data, query and traversal with zero IO',
      () async {
    final snapshot = api.activeApiSnapshot!;
    final root = '/api/v1/datasets/${f.dataset}/verified-http/v1';
    for (final path in [
      'http://external.invalid/',
      '$root/capability/x',
      '$root/operations/../x',
      '$root/operations/a%2Fb',
      '$root/operations/%61',
      '$root/operations/a/cancel/extra',
      '$root/capability\n'
    ]) {
      await expectLater(
          api.scopedTaskBatchRequest(snapshot, 'POST', path, data: {}),
          throwsArgumentError);
    }
    await expectLater(
        api.scopedTaskBatchRequest(snapshot, 'POST', '$root/capability',
            data: {}),
        throwsArgumentError);
    await expectLater(
        api.scopedTaskBatchRequest(snapshot, 'GET', '$root/capability',
            queryParameters: {'x': 1}),
        throwsArgumentError);
    await expectLater(
        api.scopedTaskBatchRequest(snapshot, 'GET', '$root/operations'),
        throwsArgumentError);
    await expectLater(
        api.scopedTaskBatchRequest(
            snapshot, 'PUT', '$root/operations/a/cancel'),
        throwsArgumentError);
    expect(f.requests, isEmpty);
  });
  test(
      'actual Task list validates dataset, preserves old JSON and uses no new request for legacy Task',
      () async {
    final task = Task(
        id: 'task',
        name: 'file.bin',
        meta: Meta(
            req: Request(url: 'https://fixture.invalid/file'), opts: Options()),
        status: Status.done,
        uploading: false,
        progress: Progress(
            used: 0, speed: 0, downloaded: 1, uploadSpeed: 0, uploaded: 0),
        createdAt: DateTime.utc(2026),
        updatedAt: DateTime.utc(2026))
      ..protocol = Protocol.http;
    final json = task.toJson();
    var decorated = true;
    f.override = (q) async {
      if (q.uri.path == '/api/v1/tasks') {
        await f.respond(q, [
          {
            ...json,
            if (decorated)
              'completionView': {
                'version': 1,
                'scopeVersion': 1,
                'datasetId': 'a' * 32,
                'taskId': 'task',
                'completionId': 'completion',
                'available': true,
                'phase': 'committed',
                'recordedMatch': true,
                'errorCategory': null
              }
          }
        ]);
      } else {
        await f.handle(q);
      }
    };
    final result = (await api.getTasks([])).single;
    expect(result.completionView!.available, true);
    expect(
        HttpCompletionView.fromTaskProjection(result.completionView!)
            .labelFor(result.status),
        '校验并保存完成');
    expect(result.toJson(), json);
    expect(result.toJson().containsKey('completionView'), false);
    expect(f.requests.length, 2);
    f.dataset = 'c' * 32;
    final wrong = (await api.getTasks([])).single;
    expect(wrong.completionView!.available, false);
    expect(
        HttpCompletionView.fromTaskProjection(wrong.completionView!)
            .labelFor(wrong.status),
        '校验完成状态未确认');
    decorated = false;
    final before = f.requests.length;
    final legacy = (await api.getTasks([])).single;
    expect(legacy.completionView, isNull);
    expect(f.requests.length, before + 1);
    f.init('c');
    expect(result.completionView!.available, false);
  });
  test(
      'late old transport response cannot expose IDs after active generation changes',
      () async {
    final gate = Completer<void>();
    f.postGate = gate;
    final pending = bound().create(op, payload());
    while (f.posts == 0) {
      await Future<void>.delayed(const Duration(milliseconds: 1));
    }
    f.init('c');
    gate.complete();
    expect((await pending).createdTaskIds, isEmpty);
    expect(f.posts, 1);
  });
}
