import 'dart:async';

import 'package:dio/dio.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/app/modules/create/views/netdisk_view.dart';

import 'netdisk_test_support.dart';

void main() {
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
