import 'dart:async';
import 'dart:io';

import 'package:dio/dio.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/app/modules/create/views/netdisk_view.dart';
import 'package:gopeed/util/netdisk_browser.dart';

import 'netdisk_test_support.dart';

class ReloadableBrowserTool extends MockBrowserTool {
  final Future<List<NetdiskBrowser>> Function() discover;
  ReloadableBrowserTool(this.discover);
  @override
  Future<List<NetdiskBrowser>> browsers() => discover();
}

void main() {
  testWidgets(
      'Default profiles load while the provider request is still pending',
      (tester) async {
    final service = Completer<Response>();
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async =>
        path.endsWith('/providers') ? service.future : null;
    final tool = NetdiskBrowserTool(
        home: '/Users/example',
        isMacOS: true,
        appExists: (path) async => path == '/Applications/Google Chrome.app',
        readText: (_) async =>
            '{"profile":{"info_cache":{"Default":{"name":"Chrome 默认资料"}}}}');
    await mountNetdisk(tester, transport,
        browser: ReloadableBrowserTool(tool.browsers),
        input: const NetdiskInput(mockSource));
    expect(service.isCompleted, isFalse);
    expect(find.text('Chrome 默认资料'), findsOneWidget);
    expect(find.textContaining('未找到 Chrome'), findsNothing);
    service
        .completeError(const NetdiskException('无法连接下载服务', 'connection_error'));
    await tester.pumpAndSettle();
    expect(find.text('Chrome 默认资料'), findsOneWidget);
    expect(find.textContaining('无法连接下载服务'), findsOneWidget);
  });

  testWidgets('permission and connection errors remain usable together',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async {
      if (path.endsWith('/providers')) {
        throw const NetdiskException('无法连接下载服务，请稍后重试', 'connection_error');
      }
      return null;
    };
    final tool = NetdiskBrowserTool(
        home: '/Users/example',
        isMacOS: true,
        appExists: (path) async => path == '/Applications/Google Chrome.app',
        readText: (path) async => throw FileSystemException(
            'read', path, const OSError('Operation not permitted', 1)));
    await mountNetdisk(tester, transport,
        browser: ReloadableBrowserTool(tool.browsers),
        input: const NetdiskInput(mockSource));
    expect(find.textContaining('errno 1'), findsOneWidget);
    expect(find.textContaining('无法连接下载服务'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'permission error includes the real path, allows manual choice and retry',
      (tester) async {
    var failed = true;
    final realTool = NetdiskBrowserTool(
        home: '/Users/example',
        isMacOS: true,
        appExists: (path) async => path == '/Applications/Google Chrome.app',
        readText: (path) async {
          if (failed) {
            throw FileSystemException(
                'read', path, const OSError('Operation not permitted', 1));
          }
          return '{"profile":{"info_cache":{"Default":{"name":"实际资料"}}}}';
        });
    await mountNetdisk(tester, MockNetdiskTransport(),
        browser: ReloadableBrowserTool(realTool.browsers),
        input: const NetdiskInput(mockSource));
    expect(
        find.textContaining(
            '/Users/example/Library/Application Support/Google/Chrome/Local State'),
        findsOneWidget);
    expect(find.textContaining('errno 1'), findsOneWidget);
    expect(find.textContaining('未找到 Chrome'), findsNothing);
    expect(
        tester
            .widget<TextButton>(find.widgetWithText(TextButton, '选择资料文件'))
            .onPressed,
        isNotNull);
    expect(find.text('Default'), findsNothing);
    failed = false;
    await tester.tap(find.text('重新读取资料'));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('netdisk-profile-error')), findsNothing);
    expect(find.text('实际资料'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });

  testWidgets('empty browser profiles have a visible diagnostic',
      (tester) async {
    await mountNetdisk(tester, MockNetdiskTransport(),
        browser: ReloadableBrowserTool(() async => [
              const NetdiskBrowser(
                  name: 'Google Chrome',
                  application: '/Applications/Google Chrome.app',
                  localStatePath: '/empty/Local State',
                  profiles: [])
            ]));
    expect(find.textContaining('没有可用的个人资料'), findsOneWidget);
    expect(find.textContaining('/empty/Local State'), findsOneWidget);
    expect(find.text('学习'), findsNothing);
  });

  testWidgets('unsupported service and unsupported provider are explicit',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async {
      if (path.endsWith('/providers')) {
        throw const NetdiskException(
            '当前下载服务不支持网盘功能，请更新下载服务后重试。', 'unsupported_service');
      }
      return null;
    };
    await mountNetdisk(tester, transport);
    expect(find.textContaining('不支持网盘功能'), findsOneWidget);
    transport.handler = (path, method, data) async =>
        path.endsWith('/providers') ? transport.envelope([]) : null;
    await tester.tap(find.text('重试服务信息'));
    await tester.pumpAndSettle();
    expect(find.textContaining('不支持夸克网盘'), findsOneWidget);
  });

  testWidgets(
      'connection failure is shown accurately and service retry recovers',
      (tester) async {
    final transport = MockNetdiskTransport();
    var failed = true;
    transport.handler = (path, method, data) async {
      if (path.endsWith('/providers') && failed) {
        throw const NetdiskException('无法连接下载服务，请稍后重试', 'connection_error');
      }
      return null;
    };
    await mountNetdisk(tester, transport);
    expect(find.textContaining('无法连接下载服务'), findsOneWidget);
    expect(find.textContaining('已更新'), findsNothing);
    expect(find.text('报告.pdf'), findsOneWidget);
    failed = false;
    await tester.tap(find.text('重试服务信息'));
    await tester.pumpAndSettle();
    expect(find.textContaining('无法连接下载服务'), findsNothing);
    expect(transport.at('/providers'), hasLength(2));
  });

  testWidgets(
      'other service errors retain their message without upgrade advice',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async {
      if (path.endsWith('/providers')) {
        throw const NetdiskException('服务拒绝访问（HTTP 403）', 'http_error');
      }
      return null;
    };
    await mountNetdisk(tester, transport);
    expect(find.textContaining('HTTP 403'), findsOneWidget);
    expect(find.textContaining('已更新'), findsNothing);
  });

  testWidgets('browser discovery failure is visible and can be retried',
      (tester) async {
    var failed = true;
    final tool = ReloadableBrowserTool(() async {
      if (failed) throw const NetdiskException('HOME 路径不可用', 'invalid_home');
      return MockBrowserTool().browsers();
    });
    await mountNetdisk(tester, MockNetdiskTransport(), browser: tool);
    expect(find.textContaining('HOME 路径不可用'), findsOneWidget);
    expect(find.textContaining('未找到 Chrome'), findsNothing);
    failed = false;
    await tester.tap(find.text('重新读取资料'));
    await tester.pumpAndSettle();
    expect(find.textContaining('HOME 路径不可用'), findsNothing);
    expect(find.text('学习'), findsOneWidget);
  });

  testWidgets('resume-only mode never lists an old session or selects files',
      (tester) async {
    final transport = MockNetdiskTransport();
    var resumed = 0;
    await mountNetdisk(tester, transport,
        input: const NetdiskInput(mockSource, sessionId: 'expired'),
        onResume: () async {
      resumed++;
    });
    expect(find.text('夸克网盘 · 恢复下载'), findsOneWidget);
    expect(find.byType(CupertinoCheckbox), findsNothing);
    expect(find.byTooltip('刷新列表'), findsNothing);
    expect(find.text('确认下载'), findsNothing);
    expect(transport.at('/list'), isEmpty);
    expect(resumed, 0);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(resumed, 0);
    expect(transport.at('/prepare'), isEmpty);
    expect(transport.at('/tasks/batch'), isEmpty);
  });

  testWidgets('cancelled resume ignores a late browser capture',
      (tester) async {
    final transport = MockNetdiskTransport();
    final status = Completer<Response>();
    var resumed = 0;
    transport.handler = (path, method, data) async =>
        path.contains('/browser/requests/') && method == 'GET'
            ? status.future
            : null;
    await mountNetdisk(tester, transport, onResume: () async {
      resumed++;
    });
    await tester.tap(find.text('重新同步登录'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 150));
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    status.complete(transport.envelope({
      'id': 'request',
      'source': mockSource.toJson(),
      'state': 'ready',
      'sessionId': 'late'
    }));
    await tester.pump(const Duration(seconds: 1));
    expect(resumed, 0);
    expect(transport.at('/prepare'), isEmpty);
    expect(transport.at('/tasks/batch'), isEmpty);
    expect(transport.at('/list'), isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'captured file and folder show directly and prepare only after confirmation',
      (tester) async {
    final transport = MockNetdiskTransport();
    await mountNetdisk(tester, transport,
        input: const NetdiskInput(mockSource,
            sessionId: 'captured', files: [mockFile, mockFolder]));
    expect(find.text('报告.pdf'), findsOneWidget);
    expect(find.text('资料'), findsOneWidget);
    expect(find.text('夸克网盘 · 确认下载'), findsOneWidget);
    expect(find.text('quark'), findsNothing);
    expect(find.text('drive-pc.quark.cn'), findsNothing);
    expect(transport.at('/list'), isEmpty);
    expect(transport.at('/prepare'), isEmpty);
    expect(transport.at('/tasks/batch'), isEmpty);
    await tester.tap(find.text('报告.pdf')); // Download the selected folder only.
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(transport.at('/prepare').single.$3['files'], [mockFolder.toJson()]);
    expect(transport.at('/prepare').single.$3['opts']['path'], '/downloads');
    expect(transport.at('/tasks/batch'), hasLength(1));
    expect(transport.result, isTrue);
    expect(find.byType(NetdiskView), findsNothing);
    expect(tester.takeException(), isNull);
  });

  testWidgets('pagination and directory navigation retain chosen files',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async {
      if (!path.endsWith('/list')) return null;
      final parent = data['source']['parentId'];
      final files = parent == 'folder'
          ? [const NetdiskFile(id: 'inside', name: '目录内.txt').toJson()]
          : data['cursor'] == 'page2'
              ? [const NetdiskFile(id: 'more', name: '第二页.txt').toJson()]
              : [mockFile.toJson(), mockFolder.toJson()];
      return transport.envelope({
        'source': data['source'],
        'sessionId': 'session',
        'files': files,
        'nextPage': parent == null && data['cursor'] == null ? 'page2' : ''
      });
    };
    await mountNetdisk(tester, transport,
        input: const NetdiskInput(mockSource, sessionId: 'session'));
    await tester.tap(find.text('报告.pdf'));
    await tester.tap(find.text('加载更多'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('第二页.txt'));
    await tester.tap(find.byTooltip('打开 资料'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('目录内.txt'));
    await tester.tap(find.byTooltip('上级目录'));
    await tester.pumpAndSettle();
    await tester.tap(find.byTooltip('刷新列表'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    final files = transport.at('/prepare').single.$3['files'] as List;
    expect(files.map((f) => f['id']), ['file', 'more', 'inside']);
    expect(transport.at('/list').map((c) => c.$3['source']['parentId']),
        [null, null, 'folder', null, null]);
    expect(tester.takeException(), isNull);
  });

  testWidgets('expired login shows recapture action and never submits a batch',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async => path.endsWith('/prepare')
        ? transport
            .envelope({'reasonCode': 'login_required'}, code: 1, msg: '登录已失效')
        : null;
    await mountNetdisk(tester, transport);
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(find.textContaining('登录已失效'), findsOneWidget);
    expect(find.text('重新同步登录'), findsOneWidget);
    expect(transport.at('/tasks/batch'), isEmpty);
    expect(transport.result, isNull);
  });

  testWidgets(
      'partial batch remains visible with no completion or duplicate submission',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler =
        (path, method, data) async => path.endsWith('/tasks/batch')
            ? transport.envelope({
                'reasonCode': 'partial_batch',
                'taskIds': ['one']
              }, code: 1, msg: '部分任务创建失败')
            : null;
    await mountNetdisk(tester, transport);
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(find.textContaining('已创建 1 个任务'), findsOneWidget);
    expect(find.byType(NetdiskView), findsOneWidget);
    expect(transport.result, isNull);
    await tester.tap(find.text('确认下载'), warnIfMissed: false);
    await tester.pump();
    expect(transport.at('/tasks/batch'), hasLength(1));
    expect(tester.takeException(), isNull);
  });

  testWidgets('cancel ends dialog polling and ignores a late capture result',
      (tester) async {
    final transport = MockNetdiskTransport();
    final status = Completer<Response>();
    transport.handler = (path, method, data) async {
      if (method == 'GET' && path.contains('/browser/requests/')) {
        return status.future;
      }
      return null;
    };
    await mountNetdisk(tester, transport,
        input: const NetdiskInput(mockSource));
    await tester.tap(find.text('打开页面并同步'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 150));
    expect(
        transport.at('/browser/requests/request').where((c) => c.$2 == 'GET'),
        hasLength(1));
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    status.complete(transport.envelope({
      'id': 'request',
      'source': mockSource.toJson(),
      'state': 'ready',
      'sessionId': 'late'
    }));
    await tester.pump(const Duration(seconds: 1));
    expect(transport.at('/list'), isEmpty);
    expect(
        transport
            .at('/browser/requests/request')
            .where((c) => c.$2 == 'DELETE'),
        hasLength(1));
    expect(
        transport.at('/browser/requests/request').where((c) => c.$2 == 'GET'),
        hasLength(1));
    expect(transport.at('/prepare'), isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'closing during request creation cancels the request once it arrives',
      (tester) async {
    final transport = MockNetdiskTransport();
    final request = Completer<Response>();
    final browser = MockBrowserTool();
    transport.handler = (path, method, data) async =>
        path.endsWith('/browser/request') ? request.future : null;
    await mountNetdisk(tester, transport,
        input: const NetdiskInput(mockSource), browser: browser);
    await tester.tap(find.text('打开页面并同步'));
    await tester.pump();
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    request.complete(transport.envelope(
        {'id': 'request', 'state': 'waiting', 'source': mockSource.toJson()}));
    await tester.pump();
    expect(
        transport
            .at('/browser/requests/request')
            .where((c) => c.$2 == 'DELETE'),
        hasLength(1));
    expect(browser.launches, isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'recapture binds a new session and refreshes the current selection',
      (tester) async {
    final transport = MockNetdiskTransport();
    transport.handler = (path, method, data) async =>
        path.contains('/browser/requests/') && method == 'GET'
            ? transport.envelope({
                'id': 'request',
                'source': mockSource.toJson(),
                'state': 'ready',
                'sessionId': 'new'
              })
            : null;
    final browser = MockBrowserTool();
    await mountNetdisk(tester, transport, browser: browser);
    await tester.tap(find.text('重新同步登录'));
    await tester.pump();
    await tester.pump(const Duration(milliseconds: 150));
    await tester.pumpAndSettle();
    expect(transport.at('/list').single.$3['sessionId'], 'new');
    expect(browser.launches.single, contains('Default'));
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(transport.at('/prepare').single.$3['sessionId'], 'new');
    expect(transport.at('/prepare').single.$3['files'], [mockFile.toJson()]);
    expect(transport.result, isTrue);
  });

  testWidgets('empty selection cannot prepare, and save directory is required',
      (tester) async {
    final transport = MockNetdiskTransport();
    await mountNetdisk(tester, transport);
    await tester.tap(find.text('报告.pdf'));
    await tester.pump();
    expect(
        tester.widget<CupertinoCheckbox>(find.byType(CupertinoCheckbox)).value,
        isFalse);
    await tester.tap(find.text('确认下载'));
    await tester.pump();
    expect(transport.at('/prepare'), isEmpty);
    await tester.tap(find.text('报告.pdf'));
    await tester.pump();
    await tester.enterText(find.byType(TextFormField), '');
    await tester.pump();
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(find.text('请选择保存目录'), findsOneWidget);
    expect(transport.at('/prepare'), isEmpty);
  });
}
