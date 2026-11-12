import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/api/http_path_api.dart';
import 'package:gopeed/api/model/http_path_create.dart';
import 'package:gopeed/api/model/create_task_batch.dart';
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/api/model/path_policy_config.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task_batch_catalog.dart';
import 'package:gopeed/api/model/task_batch_operation.dart';
import 'support/scoped_task_batch_fixture.dart';

void main() {
  final f = ScopedBatchFixture();
  final op = TaskBatchOperationRef(
      namespace: TaskBatchOperationRef.namespaceV1, operationId: 'path:one');
  CreateTaskBatch batch() => CreateTaskBatch(reqs: [
        CreateTaskBatchItem(
            req: Request(url: 'https://fixture.invalid/one.bin'),
            opts: Options(path: '/tmp/fixture', name: 'one.bin'))
      ]);
  HttpPathCreateRequest request(CreateTaskBatch b, {String? sha}) =>
      HttpPathCreateRequest(b,
          pathIntent:
              HttpPathIntent(path: '/tmp/fixture', origin: 'user_selected'),
          expectedSHA256: sha);
  setUpAll(f.start);
  setUp(f.reset);
  tearDownAll(f.close);

  test(
      'settings default stays absent and unsupported values survive unrelated edits',
      () {
    final config = DownloaderConfig();
    expect(config.extra.toJson().containsKey('pathPolicyV1'), false);
    for (final raw in [
      null,
      7,
      {'version': 2, 'enabled': true, 'rules': []},
      {
        'version': 1,
        'enabled': false,
        'rules': [],
        'future': {'x': 1}
      }
    ]) {
      final json = config.toJson();
      json['extra']['pathPolicyV1'] = raw;
      final decoded = DownloaderConfig.fromJson(json);
      decoded.maxRunning = 7;
      expect(decoded.extra.pathPolicyV1!.editable, false);
      expect(decoded.toJson()['extra']['pathPolicyV1'], raw);
    }
    final enabled = PathPolicyConfig.disabled().changed(enabled: true, rules: [
      PathPolicyRule.empty('one').changed(extension: '.bin', root: '/tmp/bin')
    ]);
    expect(enabled.toJson(), {
      'version': 1,
      'enabled': true,
      'rules': [
        {
          'id': 'one',
          'kind': 'extension',
          'match': {'extension': '.bin'},
          'root': '/tmp/bin'
        }
      ]
    });
  });

  test(
      'typed intent is owned and raw path mismatch is rejected before transport',
      () {
    final b = batch();
    final original = jsonEncode(b.toJson());
    final frozen = request(b, sha: 'a' * 64);
    expect(jsonEncode(b.toJson()), original);
    expect(frozen.requiredCapabilities,
        [httpPathCapability, 'http-sha256-completion/v1']);
    b.reqs!.single.opts!.path = '/tmp/changed';
    expect(frozen.toJson(op.operationId)['request']['opts']['path'],
        '/tmp/fixture');
    expect(() => request(b), throwsArgumentError);
    expect(() => HttpPathIntent(path: '/tmp/fixture', origin: 'rule'),
        throwsArgumentError);
    expect(f.requests, isEmpty);
  });

  test(
      'protocol capability stays isolated and prepared directory bytes are owned',
      () async {
    final prepared = <Map<String, dynamic>>[
      for (final name in ['a.bin', 'b.bin'])
        {
          'req': {
            'url': 'https://fixture.invalid/share',
            'protocol': 'netdisk',
            'extra': {
              'batchId': 'original-batch',
              'sessionId': 'original-session',
              'accountId': 'original-account',
              'deferred': true,
              'file': {'path': 'Folder/Sub', 'name': name}
            }
          },
          'opts': {'path': '/tmp/fixture', 'name': name},
        }
    ];
    final raw = jsonEncode(prepared);
    final r = HttpPathCreateRequest.netdisk(
        reqs: prepared,
        batchId: 'original-batch',
        pathIntent: HttpPathIntent(path: '/tmp/fixture', origin: 'default'));
    prepared.first['req']['extra']['sessionId'] = 'changed-after-snapshot';
    var wrongKind = true, floatCode = false;
    f.override = (q) async {
      if (q.uri.path.endsWith('/capability')) {
        q.response.headers.contentType = ContentType.json;
        q.response.write(jsonEncode({
          'code': floatCode ? 0.0 : 0,
          'data': {
            'scopeVersion': 1,
            'datasetId': f.dataset,
            'kind': wrongKind ? httpPathCreateKind : r.variant.kind,
            'requestVersion': 1,
            'supported': true,
            'requiredCapabilities': r.requiredCapabilities,
            'methods': {
              'create': 'POST',
              'query': 'GET',
              'cancel': 'POST',
              'preview': 'POST'
            },
          }
        }));
        await q.response.close();
      } else {
        await f.respond(q, {
          ...f.wrapper(f.receipt(op.operationId)),
          'kind': r.variant.kind,
          'requiredCapabilities': r.requiredCapabilities
        });
      }
    };
    final bound = HttpPathApi(
        snapshot: api.activeApiSnapshot!,
        identity:
            DatasetIdentity(version: 1, datasetId: f.dataset, persistent: true),
        request: r);
    expect(await bound.supported(), false);
    wrongKind = false;
    floatCode = true;
    expect(await bound.supported(), false);
    floatCode = false;
    expect(await bound.supported(), true);
    await bound.create(op, r.batch);
    final post = f.requests.singleWhere((q) => q['method'] == 'POST');
    expect(post['uri'].path, endsWith('/netdisk-path/v1/operations'));
    final body = post['body'] as Map;
    expect(body['request'], isNull);
    expect(jsonEncode(body['reqs']), raw);
    expect(body['batchId'], 'original-batch');
    expect(body['requiredCapabilities'], ['netdisk-path-freeze/v1']);
    final count = f.requests.length;
    await expectLater(
        api.scopedTaskBatchRequest(api.activeApiSnapshot!, 'GET',
            '/api/v1/datasets/${f.dataset}/netdisk-path/v1/capability',
            queryParameters: {'integrity': 'sha256'}),
        throwsArgumentError);
    expect(
        () => HttpPathCreateRequest(
            CreateTaskBatch(reqs: [
              CreateTaskBatchItem(
                  req: Request(
                      url: 'magnet:?xt=urn:btih:fixture', protocol: 'bt'),
                  opts: Options(path: '/tmp/fixture'))
            ]),
            pathIntent: HttpPathIntent(path: '/tmp/fixture', origin: 'default'),
            variant: PathCreateVariant.bt,
            expectedSHA256: 'a' * 64),
        throwsArgumentError);
    expect(f.requests.length, count);
  });

  test(
      'actual client binds capability preview and operation, preserves 409 history without replay',
      () async {
    final b = batch();
    final r = request(b, sha: 'a' * 64);
    var old = false, known = false;
    f.override = (q) async {
      if (q.uri.path.endsWith('/capability')) {
        if (old) {
          q.response.statusCode = 404;
          q.response.write('<html>old</html>');
          await q.response.close();
          return;
        }
        await f.respond(q, {
          'scopeVersion': 1,
          'datasetId': f.dataset,
          'kind': httpPathCreateKind,
          'requestVersion': 1,
          'supported': true,
          'requiredCapabilities': r.requiredCapabilities,
          'methods': {
            'create': 'POST',
            'query': 'GET',
            'cancel': 'POST',
            'preview': 'POST'
          }
        });
      } else if (q.uri.path.endsWith('/preview')) {
        await f.respond(q, {
          'version': 1,
          'decision': {
            'version': 1,
            'root': '/tmp/fixture',
            'origin': 'user_selected',
            'ruleId': '',
            'ruleVersion': null
          },
          'name': 'one.bin',
          'layoutHash': 'b' * 64,
          'layout': {
            'version': 1,
            'parentRoot': '/tmp/fixture',
            'members': [
              {'index': 0, 'relativePath': 'one.bin', 'selected': true}
            ]
          }
        });
      } else {
        if (q.method == 'POST') known = true;
        await f.respond(
            q,
            {
              ...f.wrapper(known
                  ? f.receipt(op.operationId, count: 1)
                  : f.receipt(op.operationId, state: 'unknown')),
              'kind': httpPathCreateKind,
              'requiredCapabilities': known ? r.requiredCapabilities : []
            },
            status: q.method == 'POST' ? 409 : 200,
            code: q.method == 'POST' ? 1000 : 0,
            msg: q.method == 'POST' ? 'operation_conflict' : '');
      }
    };
    final bound = HttpPathApi(
        snapshot: api.activeApiSnapshot!,
        identity:
            DatasetIdentity(version: 1, datasetId: f.dataset, persistent: true),
        request: r);
    expect(await bound.supported(), true);
    expect(f.requests.last['uri'].queryParameters, {'integrity': 'sha256'});
    expect((await bound.query(op)).receipt!.state, 'unknown');
    expect(
        HttpPathReply(
            datasetId: f.dataset,
            operation: op,
            action: TaskBatchAction.query,
            requiredCapabilities: r.requiredCapabilities,
            statusCode: 503,
            body: {
              'code': 1000,
              'data': {
                ...f.wrapper(f.receipt(op.operationId, state: 'unknown')),
                'kind': httpPathCreateKind,
                'requiredCapabilities': []
              }
            }).scopeVerified,
        false);
    expect((await bound.preview(op)).root, '/tmp/fixture');
    expect((await bound.create(op, b)).requiresQuery, true);
    expect((await bound.query(op)).receipt!.operationId, op.operationId);
    expect(
        f.requests.where((q) =>
            q['method'] == 'POST' && q['uri'].path.endsWith('/operations')),
        hasLength(1));
    expect(f.requests.every((q) => q['token'] == 'fixture-scoped-token'), true);
    final post =
        f.requests.firstWhere((q) => q['uri'].path.endsWith('/operations'));
    expect(post['body']['pathIntent'],
        {'version': 1, 'path': '/tmp/fixture', 'origin': 'user_selected'});
    expect(f.requests.every((q) => q['uri'].path.contains('/http-path/v1/')),
        true);
    old = true;
    expect(await bound.supported(), false);
    expect(
        f.requests.where((q) =>
            q['method'] == 'POST' && q['uri'].path.endsWith('/operations')),
        hasLength(1));
  });
}
