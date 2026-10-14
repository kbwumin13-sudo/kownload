import 'package:dio/dio.dart' hide Options;
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/app/modules/create/views/netdisk_view.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/util/netdisk_browser.dart';

const mockSource =
    NetdiskSource(provider: 'quark', url: 'https://pan.quark.cn/s/share');
const mockFile = NetdiskFile(id: 'file', name: '报告.pdf', size: 1024);
const mockFolder = NetdiskFile(id: 'folder', name: '资料', isDir: true);

Task mockExistingTask({String sessionId = 'lost-after-restart'}) => Task(
    id: 'original-task',
    name: mockFile.name,
    meta: Meta(
        req: Request(url: mockSource.url, protocol: 'netdisk', extra: {
          'provider': mockSource.provider,
          // Transferred copies keep the original page but have no ShareID.
          'source': mockSource.toJson(),
          'file': mockFile.toJson(),
          if (sessionId.isNotEmpty) 'sessionId': sessionId,
          'accountId': 'original-account',
          'batchId': 'original-batch',
        }),
        opts: Options(path: '/original-downloads')),
    status: Status.error,
    uploading: false,
    progress: Progress(
        used: 3, speed: 0, downloaded: 512, uploadSpeed: 0, uploaded: 0),
    createdAt: DateTime.utc(2026),
    updatedAt: DateTime.utc(2026));

class MockBrowserTool extends NetdiskBrowserTool {
  final launches = <String>[];
  MockBrowserTool() : super(home: '/fake', isMacOS: true);
  @override
  Future<List<NetdiskBrowser>> browsers() async => [
        const NetdiskBrowser(
            name: 'Google Chrome',
            application: '/fake/Chrome.app',
            localStatePath: '/fake/Local State',
            profiles: [
              NetdiskBrowserProfile('Default', '学习'),
              NetdiskBrowserProfile('Profile 1', '工作')
            ])
      ];
  @override
  Future<void> launch(NetdiskBrowser browser, NetdiskBrowserProfile profile,
      NetdiskSource source,
      {String? requestId}) async {
    launches.add('${browser.name}:${profile.directory}:${source.url}');
  }
}

class MockNetdiskTransport {
  final calls = <(String, String, dynamic)>[];
  Future<Response?> Function(String, String, dynamic)? handler;
  bool? result;
  Task? task;
  late final NetdiskApi api = NetdiskApi(forward: forward);
  Response envelope(dynamic data, {int code = 0, String msg = ''}) => Response(
      requestOptions: RequestOptions(path: '/mock'),
      data: {'code': code, 'msg': msg, 'data': data});
  Future<Response> forward(String path,
      {required String method,
      dynamic data,
      Map<String, dynamic>? queryParameters}) async {
    calls.add((path, method, data));
    final custom = await handler?.call(path, method, data);
    if (custom != null) return custom;
    if (task != null && path.endsWith('/tasks/${task!.id}')) {
      return envelope(task!.toJson());
    }
    if (task != null && path.endsWith('/tasks/${task!.id}/continue')) {
      return envelope(null);
    }
    if (path.endsWith('/providers')) {
      return envelope([
        {
          'id': 'quark',
          'name': '夸克网盘',
          'hosts': ['pan.quark.cn'],
          'apiHosts': ['drive-pc.quark.cn']
        }
      ]);
    }
    if (path.endsWith('/browser/request')) {
      return envelope(
          {'id': 'request', 'state': 'waiting', 'source': data['source']});
    }
    if (path.contains('/browser/requests/')) {
      if (method == 'DELETE') return envelope(null);
      return envelope(
          {'id': 'request', 'state': 'waiting', 'source': mockSource.toJson()});
    }
    if (path.endsWith('/list')) {
      return envelope({
        'source': data['source'],
        'sessionId': data['sessionId'],
        'files': [mockFile.toJson(), mockFolder.toJson()],
        'nextPage': ''
      });
    }
    if (path.endsWith('/prepare')) {
      return envelope({
        'batchId': 'batch',
        'reqs': [
          for (final file in data['files'])
            {
              'req': {
                'protocol': 'netdisk',
                'url': mockSource.url,
                'extra': {
                  'provider': 'quark',
                  'source': data['source'],
                  'file': file,
                  'sessionId': data['sessionId'],
                  'batchId': 'batch'
                }
              },
              'opts': data['opts']
            }
        ]
      });
    }
    if (path.endsWith('/tasks/batch')) {
      return envelope(
          [for (var i = 0; i < data['reqs'].length; i++) 'task-$i']);
    }
    throw StateError('unexpected mock request: $method $path');
  }

  List<(String, String, dynamic)> at(String suffix) =>
      calls.where((c) => c.$1.endsWith(suffix)).toList();
}

Future<void> mountNetdisk(WidgetTester tester, MockNetdiskTransport transport,
    {NetdiskInput input =
        const NetdiskInput(mockSource, sessionId: 'old', files: [mockFile]),
    MockBrowserTool? browser,
    Future<void> Function()? onResume,
    Duration pollInterval = const Duration(milliseconds: 100)}) async {
  await tester.binding.setSurfaceSize(const Size(800, 600));
  addTearDown(() => tester.binding.setSurfaceSize(null));
  await tester.pumpWidget(MaterialApp(
      theme: macTheme(Brightness.light),
      home: Builder(
        builder: (context) => Scaffold(
            body: TextButton(
                child: const Text('打开'),
                onPressed: () async {
                  transport.result = await showDialog<bool>(
                      context: context,
                      barrierDismissible: false,
                      builder: (_) => NetdiskView(
                          input: input,
                          options: Options(path: '/downloads'),
                          api: transport.api,
                          browserTool: browser ?? MockBrowserTool(),
                          onResume: onResume,
                          pollInterval: pollInterval));
                })),
      )));
  await tester.tap(find.text('打开'));
  await tester.pumpAndSettle();
}
