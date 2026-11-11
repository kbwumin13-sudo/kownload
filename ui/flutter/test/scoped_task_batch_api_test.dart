import 'dart:io';
import 'dart:convert';
import 'dart:isolate';
import 'package:gopeed/database/task_batch_journal.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/create_task_batch.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'package:gopeed/api/model/task_batch_scope.dart';
import 'package:gopeed/api/task_batch_api.dart';
import 'support/scoped_task_batch_fixture.dart';

Future<void> queryScopedUnix(List<Object> args) async {
  final port = args[0] as SendPort;
  try {
    api.init('unix', args[1] as String, 'fixture-unix-token',
        taskBatchRoute: JournalRouteRef(
            product: 'kownload', identityProtocol: 1, routeScope: 'b' * 64));
    final snapshot = api.activeApiSnapshot!;
    final identity = await ScopedTaskBatchApi.readIdentity(snapshot);
    final reply =
        await ScopedTaskBatchApi(snapshot: snapshot, identity: identity).query(
            TaskBatchOperationRef(
                namespace: TaskBatchOperationRef.namespaceV1,
                operationId: 'unix-op'));
    Isolate.exit(port, {
      'status': reply.envelope.statusCode,
      'matched': reply.hasMatchingSnapshot,
      'query': reply.requiresQuery,
      'network': snapshot.network
    });
  } catch (error) {
    Isolate.exit(port, {'error': error.runtimeType.toString()});
  }
}

void main() {
  final f = ScopedBatchFixture();
  final op = TaskBatchOperationRef(
      namespace: TaskBatchOperationRef.namespaceV1, operationId: 'example:op');
  CreateTaskBatch batch() => CreateTaskBatch(reqs: [
        CreateTaskBatchItem(req: Request(url: 'https://fixture.invalid/one'))
      ]);
  ScopedTaskBatchApi bound() => ScopedTaskBatchApi(
      snapshot: api.activeApiSnapshot!,
      identity:
          DatasetIdentity(version: 1, datasetId: f.dataset, persistent: true));
  setUpAll(f.start);
  setUp(f.reset);
  tearDownAll(f.close);
  test('shared auth, canonical scoped wire, no legacy fallback', () async {
    expect(
        (await ScopedTaskBatchApi.readIdentity(api.activeApiSnapshot!))
            .datasetId,
        f.dataset);
    expect((await bound().query(op)).receipt!.state, 'unknown');
    final result = await bound().create(op, batch());
    expect(result.createdTaskIds, hasLength(2));
    expect(result.requiresQuery, isFalse);
    await bound().query(op);
    await bound().cancel(op);
    await bound().catalog();
    expect(f.posts, 2);
    expect(
        f.requests.every((r) => r['token'] == 'fixture-scoped-token'), isTrue);
    expect(
        f.requests
            .where((r) => (r['uri'] as Uri).path == '/api/v1/tasks/batch'),
        isEmpty);
    final create = f.requests.firstWhere((r) => r['method'] == 'POST');
    expect(create['body']['version'], 1);
    expect(create['body']['operationId'], 'example:op');
    expect(create['body'], isNot(contains('datasetId')));
    expect((f.requests[1]['uri'] as Uri).toString(), contains('example%3Aop'));
  });
  test(
      'same-domain partial retains HTTP/code and IDs; presence missing is unknown',
      () async {
    f.mode = 'partial';
    final r = await bound().create(op, batch());
    expect(r.envelope.statusCode, 503);
    expect(r.envelope.code, 1000);
    expect(r.createdTaskIds, ['example:op-task-0']);
    expect(r.requiresQuery, isTrue);
    expect(r.receipt!.members.first.taskPresent, isNull);
  });
  test('invalid wrapper or inner op never contributes IDs', () {
    for (final change in ['version', 'dataset', 'namespace', 'operation']) {
      final w = f.wrapper(f.receipt(op.operationId));
      if (change == 'version') w['scopeVersion'] = 2;
      if (change == 'dataset') w['datasetId'] = 'c' * 32;
      if (change == 'namespace') w['namespace'] = 'other';
      if (change == 'operation') w['operation'] = f.receipt('other');
      final r = ScopedTaskBatchReply(
          datasetId: f.dataset,
          operation: op,
          action: TaskBatchAction.query,
          statusCode: 200,
          body: {'code': 0, 'msg': '', 'data': w});
      expect(r.createdTaskIds, isEmpty);
      expect(r.requiresQuery, isTrue);
    }
    final r = ScopedTaskBatchReply(
        datasetId: f.dataset,
        operation: op,
        action: TaskBatchAction.create,
        statusCode: 409,
        body: {'code': 1000, 'msg': 'dataset_mismatch', 'data': null});
    expect(r.scopeVerified, isFalse);
    expect(r.createdTaskIds, isEmpty);
  });
  test(
      'unsupported status and contradictory mismatch wrapper are never authoritative IDs',
      () {
    for (final status in [404, 405, 501, 409]) {
      final reply = ScopedTaskBatchReply(
          datasetId: f.dataset,
          operation: op,
          action: TaskBatchAction.create,
          statusCode: status,
          body: {
            'code': 1000,
            'msg': status == 409 ? 'dataset_mismatch' : 'unsupported',
            'data': f.wrapper(f.receipt(op.operationId))
          });
      expect(reply.createdTaskIds, isEmpty);
      expect(reply.requiresQuery, isTrue);
    }
  });
  for (final mode in ['lostAck', 'html']) {
    test('$mode after POST stays unknown; only explicit query follows',
        () async {
      f.mode = mode;
      final r = await bound().create(op, batch());
      expect(r.requiresQuery, isTrue);
      expect(r.createdTaskIds, isEmpty);
      expect(f.posts, 1);
      f.mode = 'success';
      expect((await bound().query(op)).createdTaskIds, hasLength(2));
      expect(f.posts, 1);
    });
  }
  for (final status in [301, 302, 303, 307, 308]) {
    test('three scoped actions refuse $status redirect replay', () async {
      f.override = (q) async {
        q.response.statusCode = status;
        q.response.headers.set('location', '/must-not-follow');
        await q.response.close();
      };
      final c = bound();
      for (final r in [
        await c.create(op, batch()),
        await c.query(op),
        await c.cancel(op)
      ]) {
        expect(r.requiresQuery, isTrue);
        expect(r.createdTaskIds, isEmpty);
      }
      expect(f.requests, hasLength(3));
      expect(
          f.requests.any((r) => (r['uri'] as Uri).path == '/must-not-follow'),
          isFalse);
    });
  }
  test('whitelist rejects path/method/query with zero sends', () async {
    final snap = api.activeApiSnapshot!;
    final p = '/api/v1/datasets/${f.dataset}/task-batches/v1/operations';
    for (final path in [
      'https://example.invalid/',
      '$p?x=y',
      '$p\n',
      '$p/../other',
      '$p/a%2Fb',
      '$p/%61',
      '$p/a/cancel/extra',
      '/api/v1/tasks/batch',
      '/api/v1/datasets/${'A' * 32}/task-batches/v1/operations'
    ]) {
      await expectLater(
          api.scopedTaskBatchRequest(snap, 'POST', path, data: {'version': 1}),
          throwsArgumentError);
    }
    await expectLater(
        api.scopedTaskBatchRequest(
            snap, 'GET', '/api/v1/task-batches/operations',
            queryParameters: {'limit': 0}),
        throwsArgumentError);
    await expectLater(
        api.scopedTaskBatchRequest(
            snap, 'GET', '/api/v1/task-batches/operations',
            queryParameters: {'auth': 'fixture'}),
        throwsArgumentError);
    await expectLater(
        api.scopedTaskBatchRequest(snap, 'POST', '/api/v1/dataset/identity',
            data: {}),
        throwsArgumentError);
    expect(f.requests, isEmpty);
  });
  test('snapshot reflects actual singleton and rejects stale generation',
      () async {
    final old = api.activeApiSnapshot!;
    f.init('c');
    expect(old.isCurrent, isFalse);
    await expectLater(
        api.scopedTaskBatchRequest(old, 'GET', '/api/v1/dataset/identity'),
        throwsStateError);
    api.init('tcp', '127.0.0.1:1', 'different-fixture',
        taskBatchRoute: f.route());
    expect(api.activeApiSnapshot!.address, f.address);
    expect(api.activeApiSnapshot!.taskBatchRoute, isNull);
    await expectLater(
        api.scopedTaskBatchRequest(
            api.activeApiSnapshot!, 'GET', '/api/v1/dataset/identity'),
        throwsStateError);
    expect(f.requests, isEmpty);
  });
  test(
      'scoped identity and query reuse the real Unix adapter and authentication',
      () async {
    final dir = Directory('/tmp').createTempSync('scoped-unix-');
    final path = '${dir.path}/api.sock';
    final server = await HttpServer.bind(
        InternetAddress(path, type: InternetAddressType.unix), 0);
    final requests = <String>[];
    final subscription = server.listen((req) async {
      req.response.persistentConnection = false;
      expect(req.headers.value('X-Api-Token'), 'fixture-unix-token');
      requests.add('${req.method} ${req.uri.path}');
      final data = req.uri.path == '/api/v1/dataset/identity'
          ? {'version': 1, 'datasetId': f.dataset, 'persistent': true}
          : f.wrapper(f.receipt('unix-op', state: 'unknown'));
      req.response.headers.contentType = ContentType.json;
      req.response.write(jsonEncode({'code': 0, 'msg': '', 'data': data}));
      await req.response.close();
    });
    final port = ReceivePort();
    try {
      await Isolate.spawn(queryScopedUnix, [port.sendPort, path]);
      expect(await port.first.timeout(const Duration(seconds: 10)),
          {'status': 200, 'matched': true, 'query': true, 'network': 'unix'});
      expect(requests, [
        'GET /api/v1/dataset/identity',
        'GET /api/v1/datasets/${f.dataset}/task-batches/v1/operations/unix-op'
      ]);
    } finally {
      port.close();
      await subscription.cancel();
      await server.close(force: true);
      dir.deleteSync(recursive: true);
    }
  });
  test('Mem identity cannot enable recoverable submission', () async {
    f.persistent = false;
    await expectLater(ScopedTaskBatchApi.readIdentity(api.activeApiSnapshot!),
        throwsA(isA<TaskBatchCatalogException>()));
    expect(f.posts, 0);
  });
}
