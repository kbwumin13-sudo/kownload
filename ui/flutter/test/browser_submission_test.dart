import 'dart:convert';
import 'dart:io';
import 'package:dio/dio.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/rpc/browser_submission.dart';
import 'package:gopeed/app/rpc/rpc.dart';

Response<dynamic> response(dynamic data, [int status = 200]) => Response(
    requestOptions: RequestOptions(path: '/api/v1/tasks'),
    statusCode: status,
    data: data);
Map<String, dynamic> created(String id) => {
      'code': 0,
      'data': 'task-one',
      'operationId': id,
      'submission': {
        'state': 'created',
        'taskIds': ['task-one']
      }
    };

class _RealHttp extends HttpOverrides {}

void main() {
  const body =
      '{"req":{"url":"https://fixture.test/file?signature=synthetic"}}';
  test(
      'App confirmation reaches created through the existing operation, without resubmitting',
      () async {
    var opened = 0;
    var sends = 0;
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async {
          sends++;
          throw StateError('unexpected second pipeline');
        },
        showCreate: (_) async {},
        log: (_) {},
        showConfirmation: (_, id, capture) async {
          expect(id, 'app-confirm');
          opened++;
        });
    await rpc.create(body, operationId: 'app-confirm', confirm: true);
    expect(rpc.confirmationStatus('app-confirm'),
        {'state': 'accepted', 'taskIds': []});
    rpc.completeConfirmation('app-confirm', taskIds: ['created-one']);
    final reply =
        await rpc.create(body, operationId: 'app-confirm', confirm: true);
    expect(reply.body['submission'], {
      'state': 'created',
      'taskIds': ['created-one']
    });
    expect(reply.body['data'], 'created-one');
    expect(rpc.confirmationStatus('app-confirm')['state'], 'created');
    rpc.completeConfirmation('app-confirm', cancelled: true);
    expect(rpc.confirmationStatus('app-confirm')['state'], 'created');
    expect(opened, 1);
    expect(sends, 0);
  });

  test('only explicit cancellation marks an accepted confirmation cancelled',
      () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {});
    expect(rpc.confirmationStatus('unknown')['state'], 'unknown');
    await rpc.create(body, operationId: 'cancel-one', confirm: true);
    expect(rpc.confirmationStatus('cancel-one')['state'], 'accepted');
    rpc.completeConfirmation('cancel-one', cancelled: true);
    expect(rpc.confirmationStatus('cancel-one'),
        {'state': 'cancelled', 'taskIds': []});
    await rpc.create(body, operationId: 'cancel-one', confirm: true);
    expect(rpc.confirmationStatus('cancel-one')['state'], 'cancelled');
  });

  test(
      'browser capture waits for a fresh token decision and preserves real browser state',
      () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {});
    await rpc.create(body,
        operationId: 'gated', confirm: true, browserCapture: true);
    var released = false;
    final gate = rpc.requestCommit('gated').then((value) {
      released = true;
      return value;
    });
    final waiting = rpc.confirmationStatus('gated');
    expect(waiting['commitRequested'], true);
    final token = waiting['confirmToken'] as String;
    rpc.decideConfirmation('gated', 'wrong-token', 'commit',
        {'state': 'in_progress', 'paused': true});
    await Future<void>.delayed(Duration.zero);
    expect(released, false);
    expect(
        () => rpc.decideConfirmation('gated', token, 'browser_completed', {
              'state': 'in_progress',
              'paused': true,
              'received': 10,
              'total': 10
            }),
        throwsFormatException);
    rpc.decideConfirmation('gated', token, 'commit',
        {'state': 'in_progress', 'paused': true, 'received': 10, 'total': 10});
    expect(await gate, true,
        reason: 'fully buffered is not the complete terminal state');
    expect(rpc.confirmationStatus('gated')['state'], 'accepted');
    expect(rpc.confirmationStatus('gated')['commitStarted'], true);
    rpc.completeConfirmation('gated', cancelled: true);
    expect(rpc.confirmationStatus('gated')['state'], 'accepted');
    rpc.completeConfirmation('gated', taskIds: ['final-task']);
    expect(
        rpc.decideConfirmation('gated', token, 'commit',
            {'state': 'in_progress', 'paused': true})['state'],
        'created');
  });

  test('a browser-complete gate never releases a second download', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {},
        log: (_) {});
    await rpc.create(body,
        operationId: 'complete-browser', confirm: true, browserCapture: true);
    final gate = rpc.requestCommit('complete-browser');
    final token =
        rpc.confirmationStatus('complete-browser')['confirmToken'] as String;
    rpc.decideConfirmation('complete-browser', token, 'browser_completed',
        {'state': 'complete', 'paused': true});
    expect(await gate, false);
    expect(rpc.confirmationStatus('complete-browser'),
        {'state': 'browser_completed', 'taskIds': []});
    expect(await rpc.requestCommit('complete-browser'), false);
  });

  test('browser results show once per silent receipt and once per batch',
      () async {
    final results = <List<String>>[];
    final errors = <String?>[];
    var batch = false;
    final rpc = BrowserSubmissionRpc(
      send: (_, id, __) async => response(batch
          ? {
              'code': 0,
              'data': ['a', 'b'],
              'submission': {
                'state': 'created',
                'taskIds': ['a', 'b']
              },
            }
          : created(id!)),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (ids, error) async {
        results.add(ids);
        errors.add(error);
      },
    );
    await rpc.create(body, operationId: 'silent-panel', confirm: false);
    await rpc.create(body, operationId: 'silent-panel', confirm: false);
    await rpc.create(body, operationId: 'confirm-panel', confirm: true);
    expect(results, [
      ['task-one']
    ]);
    batch = true;
    await rpc.forward({'method': 'POST', 'path': '/api/v1/tasks/batch'});
    expect(results, [
      ['task-one'],
      ['a', 'b']
    ]);
    expect(errors, [null, null]);
    await rpc.forward({'method': 'GET', 'path': '/api/v1/tasks'});
    expect(results, hasLength(2));
  });
  test('local failure cause is shown but not exposed in native receipts',
      () async {
    final errors = <String?>[];
    final rpc = BrowserSubmissionRpc(
      send: (_, __, ___) async => response({
        'code': 1000,
        'msg': 'unsupported protocol https://fixture.test/secret?token=hidden'
      }),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (ids, error) async {
        expect(ids, isEmpty);
        errors.add(error);
      },
    );
    final reply = await rpc.create(body, confirm: false);
    expect(errors.single, contains('unsupported protocol'));
    expect(errors.single, isNot(contains('hidden')));
    expect(jsonEncode(reply.body), isNot(contains('unsupported')));
    final disconnected = BrowserSubmissionRpc(
      send: (_, __, ___) async => throw StateError('socket unavailable'),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (ids, error) async {
        errors.add(error);
      },
    );
    await disconnected.create(body, confirm: false);
    expect(errors.last, contains('无法连接下载服务'));
  });
  test('panel failure cannot change a created receipt', () async {
    final rpc = BrowserSubmissionRpc(
      send: (_, id, __) async => response(created(id!)),
      showCreate: (_) async {},
      log: (_) {},
      showResult: (_, __) async => throw StateError('window unavailable'),
    );
    expect(
        (await rpc.create(body, operationId: 'panel-failure', confirm: false))
            .body['submission']['state'],
        'created');
  });
  test('accepted coalesces confirmation and logs domains only', () async {
    var opens = 0;
    final events = <Map<String, dynamic>>[];
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {
          opens++;
        },
        log: events.add);
    final a = await rpc.create(body, operationId: 'confirm-one', confirm: true);
    final b = await rpc.create(body, operationId: 'confirm-one', confirm: true);
    expect(opens, 1);
    expect(a.status, 202);
    expect(b.body, a.body);
    expect(a.body['submission'], {'state': 'accepted', 'taskIds': []});
    expect(events.first['domain'], 'fixture.test');
    expect(jsonEncode(events), isNot(contains('signature')));
  });
  test('a new operation reopens confirmation after an accepted one', () async {
    var opens = 0;
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (_) async {
          opens++;
        },
        log: (_) {});
    await rpc.create(body, operationId: 'click-one', confirm: true);
    await rpc.create(body, operationId: 'click-one', confirm: true);
    expect(opens, 1, reason: 'a resend of one click must not reopen the panel');
    final again =
        await rpc.create(body, operationId: 'click-two', confirm: true);
    expect(opens, 2);
    expect(again.status, 202);
    expect(again.body['submission'], {'state': 'accepted', 'taskIds': []});
  });
  test('a new operation runs again after a definitive failure', () async {
    final calls = <String?>[];
    var fixed = false;
    final rpc = BrowserSubmissionRpc(
        send: (_, id, __) async {
          calls.add(id);
          return fixed
              ? response(created(id!))
              : response({'code': 1000, 'msg': 'not allowed', 'data': null});
        },
        showCreate: (_) async => fail('unexpected'),
        log: (_) {});
    final failed =
        await rpc.create(body, operationId: 'click-one', confirm: false);
    expect(failed.body['error'], 'business_error');
    fixed = true;
    final done =
        await rpc.create(body, operationId: 'click-two', confirm: false);
    expect(done.body['submission']['state'], 'created');
    expect(calls, ['click-one', 'click-two']);
  });
  test('silent created retains data and uses ten seconds', () async {
    final rpc = BrowserSubmissionRpc(
        send: (value, id, budget) async {
          expect(budget, const Duration(seconds: 10));
          expect(value['path'], '/api/v1/tasks');
          return response(created(id!));
        },
        showCreate: (_) async => fail('unexpected'),
        log: (_) {});
    final result =
        await rpc.create(body, operationId: 'silent-one', confirm: false);
    expect(result.body['data'], 'task-one');
    expect(result.body['submission']['state'], 'created');
  });
  for (final mode in [
    'http',
    'business',
    'empty',
    'code',
    'submission',
    'wrong ID',
    'throw'
  ]) {
    test('$mode failure is explicit and redacted', () async {
      final rpc = BrowserSubmissionRpc(
          send: (_, id, __) async {
            switch (mode) {
              case 'http':
                return response({
                  'code': 0,
                  'msg': 'fixture-secret',
                  'data': 'fixture-secret'
                }, 500);
              case 'business':
                return response({
                  'code': 1000,
                  'msg': 'fixture-secret',
                  'data': 'fixture-secret'
                });
              case 'empty':
                return response('');
              case 'code':
                return response({'data': 'fixture-secret'});
              case 'submission':
                return response({'code': 0, 'data': 'task-one'});
              case 'wrong ID':
                return response(created('other'));
              default:
                throw StateError('fixture-secret');
            }
          },
          showCreate: (_) async {},
          log: (_) {});
      final r =
          await rpc.create(body, operationId: 'failed-one', confirm: false);
      expect(r.status, mode == 'http' || mode == 'throw' ? 500 : 200);
      expect(r.body['data'], isNull);
      expect(r.body['submission'], isNull);
      expect(jsonEncode(r.body), isNot(contains('fixture-secret')));
      expect(
          r.body['error'],
          mode == 'http'
              ? 'http_error'
              : mode == 'business'
                  ? 'business_error'
                  : mode == 'throw'
                      ? 'request_failed'
                      : 'invalid_response');
    });
  }
  test('forward keeps data and the sixty second budget', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, budget) async {
          expect(budget, const Duration(seconds: 60));
          return response({
            'code': 0,
            'data': {
              'nested': [1, 2]
            }
          });
        },
        showCreate: (_) async {},
        log: (_) {});
    final r = await rpc.forward({'method': 'GET', 'path': '/api/v1/config'},
        operationId: 'get-one');
    expect(r.body['data'], {
      'nested': [1, 2]
    });
    expect(r.body['operationId'], 'get-one');
    expect(
        (await rpc.forward(
                {'method': 'POST', 'path': 'https://foreign.test/api/v1/tasks'},
                operationId: 'get-one'))
            .status,
        400);
  });
  test('legacy base64 create and invalid ID', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => throw StateError('unexpected'),
        showCreate: (value) async {
          expect(value['opts']['name'], '合成测试');
        },
        log: (_) {});
    final encoded = base64Encode(utf8.encode('{"opts":{"name":"合成测试"}}'));
    expect((await rpc.create(jsonEncode(encoded), confirm: true)).status, 202);
    expect(
        (await rpc.create(body, operationId: 'bad/id', confirm: true)).status,
        400);
  });
  test(
      'the next browser capture reads canonical App mode without reloading the helper',
      () async {
    var enabled = true;
    var confirm = true;
    var opens = 0;
    var sends = 0;
    final rpc = BrowserSubmissionRpc(
      capturePreferences: (_, __) => {'enabled': enabled, 'confirm': confirm},
      send: (_, id, __) async {
        sends++;
        return response(created(id!));
      },
      showCreate: (_) async {
        opens++;
      },
      log: (_) {},
    );
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((r) => rpc.handle(RpcContext(r, r.response), create: true));
    final client = _RealHttp().createHttpClient(null);
    Future<int> capture(String id, bool silent) async {
      final request = await client
          .postUrl(Uri.parse('http://127.0.0.1:${server.port}/create'));
      request.headers.set(operationHeader, id);
      request.headers.set(
          'X-Kownload-Host-Meta',
          jsonEncode({
            'source': 'browser-capture',
            'clientId': 'chrome-one',
            'browser': 'chrome',
            'silent': silent,
          }));
      request.write(body);
      final result = await request.close();
      await result.drain<void>();
      return result.statusCode;
    }

    try {
      expect(await capture('canonical-confirm', true), 202);
      expect(opens, 1);
      expect(sends, 0);
      enabled = false;
      expect(await capture('canonical-off', true), 409);
      expect(sends, 0);
      enabled = true;
      confirm = false;
      expect(await capture('canonical-direct', false), 200);
      expect(sends, 1);
      expect(opens, 1);
    } finally {
      client.close(force: true);
      await server.close(force: true);
    }
  });

  test('HTTP RPC fixture preserves accepted and upstream 500', () async {
    final rpc = BrowserSubmissionRpc(
        send: (_, __, ___) async => response(
            {'code': 1000, 'msg': 'fixture-secret', 'data': 'fixture-secret'},
            500),
        showCreate: (_) async {},
        log: (_) {});
    final server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
    server.listen((r) =>
        rpc.handle(RpcContext(r, r.response), create: r.uri.path == '/create'));
    final client = _RealHttp().createHttpClient(null);
    try {
      for (final route in ['/create', '/forward']) {
        final request = await client
            .postUrl(Uri.parse('http://127.0.0.1:${server.port}$route'));
        request.headers.set(operationHeader, 'rpc-fixture');
        request.write(route == '/create'
            ? body
            : jsonEncode({
                'method': 'POST',
                'path': '/api/v1/tasks',
                'data': jsonDecode(body)
              }));
        final reply = await request.close();
        final data = jsonDecode(await utf8.decoder.bind(reply).join());
        expect(reply.statusCode, route == '/create' ? 202 : 500);
        expect(data['operationId'], 'rpc-fixture');
        expect(jsonEncode(data), isNot(contains('fixture-secret')));
      }
    } finally {
      client.close(force: true);
      await server.close(force: true);
    }
  });
  test('silent response loss retries REST with the original operation',
      () async {
    var calls = 0;
    final ids = <String?>[];
    final rpc = BrowserSubmissionRpc(
        send: (_, id, __) async {
          ids.add(id);
          if (calls++ == 0) throw StateError('simulated response loss');
          return response(created(id!));
        },
        showCreate: (_) async {},
        log: (_) {});
    expect(
        (await rpc.create(body, operationId: 'lost-reply', confirm: false))
            .body['error'],
        'request_failed');
    expect(
        (await rpc.create(body, operationId: 'lost-reply', confirm: false))
            .body['data'],
        'task-one');
    expect(ids, ['lost-reply', 'lost-reply']);
  });
}
