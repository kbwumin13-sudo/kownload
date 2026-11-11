import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:isolate';

import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/model/create_task_batch.dart';
import 'package:gopeed/api/model/request.dart' as model;
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'package:gopeed/api/task_batch_api.dart';

Map<String, dynamic> receipt(String id, {String state = 'settled'}) => {
      'version': 1,
      'operationId': id,
      'state': state,
      'members': state == 'unknown'
          ? []
          : [
              {'index': 0, 'taskId': '_stable', 'state': 'created'},
              {
                'index': 1,
                'taskId': '-failed',
                'state': 'failed',
                'error': 'cancelled'
              },
              if (state == 'pending')
                {'index': 2, 'taskId': 'reserved', 'state': 'pending'}
            ],
      'taskIds': state == 'unknown' ? [] : ['_stable'],
      'createdCount': state == 'unknown' ? 0 : 1,
      'failedCount': state == 'unknown' ? 0 : 1,
      'pendingCount': state == 'pending' ? 1 : 0,
    };

// A new isolate gives the shared API singleton a real Unix adapter, without
// changing production initialization or introducing a test-only reset hook.
Future<void> queryUnixBatch(List<Object> args) async {
  final replyPort = args[0] as SendPort;
  try {
    api.init('unix', args[1] as String, 'fixture-unix-token');
    final result = await TaskBatchApi().query(TaskBatchOperationRef(
        namespace: TaskBatchOperationRef.namespaceV1, operationId: 'unix-op'));
    Isolate.exit(replyPort, {
      'status': result.statusCode,
      'ids': result.createdTaskIds,
      'query': result.requiresQuery
    });
  } catch (error) {
    Isolate.exit(replyPort, {'error': error.runtimeType.toString()});
  }
}

void main() {
  const path = '/api/v1/task-batches/operations';
  late HttpServer server;
  late StreamSubscription<HttpRequest> subscription;
  late Future<void> Function(HttpRequest, dynamic) handler;
  final requests = <Map<String, dynamic>>[];
  final client = TaskBatchApi();
  final op = TaskBatchOperationRef(
      namespace: TaskBatchOperationRef.namespaceV1, operationId: 'op:one');
  CreateTaskBatch batch() => CreateTaskBatch(reqs: [
        CreateTaskBatchItem(
            req: model.Request(
                url: 'https://fixture.invalid/file?b=2&a=1',
                extra: {
              'future': [null, 9007199254740993]
            }))
      ]);
  Future<void> respond(HttpRequest request, Object? data,
      {int status = 200, int code = 0, String msg = ''}) async {
    request.response.statusCode = status;
    request.response.headers.contentType = ContentType.json;
    request.response
        .write(jsonEncode({'code': code, 'msg': msg, 'data': data}));
    await request.response.close();
  }

  setUpAll(() async {
    server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    api.init('tcp', '127.0.0.1:${server.port}', 'fixture-batch-token');
    subscription = server.listen((request) async {
      final text = await utf8.decoder.bind(request).join();
      final data = text.isEmpty ? null : jsonDecode(text);
      requests.add({
        'method': request.method,
        'uri': request.uri,
        'data': data,
        'token': request.headers.value('X-Api-Token')
      });
      await handler(request, data);
    });
  });
  setUp(() {
    requests.clear();
    handler = (request, _) => respond(request, receipt(op.operationId));
  });
  tearDownAll(() async {
    await subscription.cancel();
    await server.close(force: true);
  });

  test('create/query/cancel use shared authentication and exact frozen wire',
      () async {
    final input = batch();
    final creating = client.create(op, input);
    input.reqs!.first.req!.url = 'https://fixture.invalid/changed';
    final created = await creating;
    final queried = await client.query(op);
    final cancelled = await client.cancel(op);
    expect(created.createdTaskIds, ['_stable']);
    expect(queried.action, TaskBatchAction.query);
    expect(cancelled.action, TaskBatchAction.cancel);
    expect(cancelled.createdTaskIds, ['_stable']);
    expect(requests.map((r) => r['method']), ['POST', 'GET', 'POST']);
    expect((requests[0]['uri'] as Uri).path, path);
    expect((requests[1]['uri'] as Uri).pathSegments.last, op.operationId);
    expect((requests[2]['uri'] as Uri).pathSegments.last, 'cancel');
    expect(requests.every((r) => r['token'] == 'fixture-batch-token'), isTrue);
    final data = requests.first['data'] as Map;
    expect(data.keys.toSet(), {'version', 'operationId', 'reqs'});
    expect(data['version'], 1);
    expect(data['operationId'], op.operationId);
    expect(
        data['reqs'][0]['req']['url'], 'https://fixture.invalid/file?b=2&a=1');
    expect(data['reqs'][0]['req']['extra']['future'], [null, 9007199254740993]);
    expect(requests[1]['data'], isNull);
    expect(requests[2]['data'], isNull);
  });
  for (final status in [409, 503]) {
    test(
        'non-2xx $status preserves partial receipt instead of business exception',
        () async {
      handler = (request, _) => respond(
          request, receipt(op.operationId, state: 'pending'),
          status: status,
          code: 1000,
          msg: status == 409
              ? 'operation_conflict'
              : 'operation_unavailable_query_same_id');
      final result = await client.create(op, batch());
      expect(result.statusCode, status);
      expect(result.code, 1000);
      expect(result.message, isNotEmpty);
      expect(result.createdTaskIds, ['_stable']);
      expect(result.receipt!.pendingCount, 1);
      expect(result.requiresQuery, isTrue);
      expect(requests, hasLength(1));
    });
  }
  test('HTTP 200 business error also retains receipt', () async {
    handler = (request, _) => respond(request, receipt(op.operationId),
        code: 1000, msg: 'operation_conflict');
    final result = await client.query(op);
    expect(result.code, 1000);
    expect(result.createdTaskIds, ['_stable']);
    expect(result.requiresQuery, isTrue);
    expect(requests.single['method'], 'GET');
  });
  test('queryRequired and unknown responses never retry create or fall back',
      () async {
    handler = (request, _) => request.method == 'POST'
        ? respond(
            request,
            {
              'version': 1,
              'operationId': op.operationId,
              'queryRequired': true
            },
            status: 503,
            code: 1000)
        : respond(request, receipt(op.operationId, state: 'unknown'));
    expect((await client.create(op, batch())).requiresQuery, isTrue);
    expect(requests, hasLength(1));
    expect((await client.query(op)).requiresQuery, isTrue);
    expect(requests.map((r) => r['method']), ['POST', 'GET']);
  });
  test('future version and other-op data remain unconfirmed and preserved',
      () async {
    for (final data in [
      receipt(op.operationId)..['version'] = 99,
      receipt('another-operation')
    ]) {
      handler = (request, _) => respond(request, data);
      final result = await client.query(op);
      expect(result.createdTaskIds, isEmpty);
      expect(result.requiresQuery, isTrue);
      expect(result.receipt!.raw, data);
    }
    expect(requests.every((r) => r['method'] == 'GET'), isTrue);
  });
  test('lost ACK only permits explicit query and raw method keeps Dio failure',
      () async {
    handler = (request, _) async {
      if (request.method == 'POST') {
        final socket = await request.response.detachSocket(writeHeaders: false);
        socket.destroy();
      } else {
        await respond(request, receipt(op.operationId));
      }
    };
    final result = await client.create(op, batch());
    expect(result.transportFailure, isNotNull);
    expect(result.requiresQuery, isTrue);
    expect(result.receipt, isNull);
    expect(requests, hasLength(1));
    expect((await client.query(op)).createdTaskIds, ['_stable']);
    expect(requests.map((r) => r['method']), ['POST', 'GET']);
    await expectLater(
        api.taskBatchOperationRequest('POST', path, data: {'version': 1}),
        throwsA(isA<DioException>().having(
            (e) => e.response, 'original response-less error', isNull)));
  });
  test('all redirects remain single-request and cannot confirm IDs', () async {
    final target = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    var targetHits = 0;
    final listener = target.listen((request) async {
      targetHits++;
      await request.drain<void>();
      await respond(request, receipt(op.operationId));
    });
    try {
      for (final status in [301, 302, 303, 307, 308]) {
        handler = (request, _) async {
          request.response.headers.set(HttpHeaders.locationHeader,
              'http://127.0.0.1:${target.port}/api/v1/tasks/batch');
          await respond(request, receipt(op.operationId), status: status);
        };
        final result = await client.create(op, batch());
        expect(result.statusCode, status);
        expect(result.createdTaskIds, isEmpty);
        expect(result.requiresQuery, isTrue);
        expect(result.receipt!.taskIds, ['_stable']);
      }
      final queried = await client.query(op);
      expect(queried.requiresQuery, isTrue);
      expect(queried.createdTaskIds, isEmpty);
      expect(requests, hasLength(6));
      expect(requests.last['method'], 'GET');
      expect(targetHits, 0);
    } finally {
      await listener.cancel();
      await target.close(force: true);
    }
  });
  test('invalid method/path combinations send zero requests', () async {
    for (final entry in <(String, String, Map<String, dynamic>?)>[
      ('GET', path, null),
      ('DELETE', '$path/op', null),
      ('PATCH', path, null),
      ('POST', '$path/op', null),
      ('GET', '$path/op/cancel', null),
      ('GET', '$path/op', {'bad': true}),
      ('POST', '$path/op/cancel', {'bad': true}),
      ('POST', 'http://fixture.invalid$path', null),
      ('GET', '//fixture.invalid$path/op', null),
      ('GET', '$path/../tasks', null),
      ('GET', '$path/%2E%2E', null),
      ('GET', '$path/op%2Fother', null),
      ('GET', '$path/op?redirect=1', null),
      ('GET', '$path/op#fragment', null),
      ('GET', '$path/op%0A', null),
      ('GET', '$path/op/', null),
      ('get', '$path/op', null),
      ('POST', '/api/v1/tasks/batch', null),
    ]) {
      await expectLater(
          api.taskBatchOperationRequest(entry.$1, entry.$2, data: entry.$3),
          throwsArgumentError,
          reason: '${entry.$1} ${entry.$2}');
    }
    expect(requests, isEmpty);
  });
  for (final status in [404, 501]) {
    test('unsupported $status never falls back to legacy create', () async {
      handler = (request, _) => respond(request, null,
          status: status,
          code: 1000,
          msg: 'atomic_operation_storage_unsupported');
      final result = await client.create(op, batch());
      expect(result.statusCode, status);
      expect(result.message, 'atomic_operation_storage_unsupported');
      expect(result.createdTaskIds, isEmpty);
      expect((requests.single['uri'] as Uri).path, path);
    });
  }
  test('new query uses shared Unix routing and authentication', () async {
    final root = Directory('/tmp').createTempSync('task-batch-unix-');
    final socketPath = '${root.path}/api.sock';
    final unixServer = await HttpServer.bind(
        InternetAddress(socketPath, type: InternetAddressType.unix), 0);
    final seen = <String>[];
    final listener = unixServer.listen((request) async {
      await request.drain<void>();
      expect(request.headers.value('X-Api-Token'), 'fixture-unix-token');
      seen.add('${request.method} ${request.uri.path}');
      await respond(request, receipt('unix-op'));
    });
    final replyPort = ReceivePort();
    Isolate? isolate;
    try {
      isolate = await Isolate.spawn(
          queryUnixBatch, <Object>[replyPort.sendPort, socketPath]);
      final result = await replyPort.first.timeout(const Duration(seconds: 10));
      expect(result, {
        'status': 200,
        'ids': ['_stable'],
        'query': false
      });
      expect(seen, ['GET $path/unix-op']);
      expect(requests, isEmpty);
    } finally {
      replyPort.close();
      isolate?.kill(priority: Isolate.immediate);
      await listener.cancel();
      await unixServer.close(force: true);
      root.deleteSync(recursive: true);
    }
  });

  test('non-JSON reply stays unknown without automatic requests', () async {
    handler = (request, _) async {
      request.response.write('not a receipt');
      await request.response.close();
    };
    final result = await client.query(op);
    expect(result.rawBody, 'not a receipt');
    expect(result.requiresQuery, isTrue);
    expect(result.receipt, isNull);
    expect(requests.single['method'], 'GET');
  });
}
