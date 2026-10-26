import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';

class _RealHttp extends HttpOverrides {}

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
  @override
  Future<void> saveConfig() async {}
}

class _Box extends Fake implements Box {
  final stored = <dynamic, dynamic>{};
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      stored[key] ?? defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {
    stored[key] = value;
  }
}

void main() {
  testWidgets(
      'browser submission waits for setup without retaining an RPC that can time out',
      (tester) async {
    if (!Platform.isMacOS) return;
    Get.testMode = true;
    final box = _Box();
    Database.instance.box = box;
    final app = Get.put<AppController>(_App());
    app.downloaderConfig.value.downloadDir = '/tmp/downloads';
    MacPanel.presentation.value = 'panel';
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(640, 620));
    final received = <String>[];
    late HttpServer server;
    await tester.runAsync(() async {
      server = await HttpServer.bind(InternetAddress.loopbackIPv4, 0);
      server.listen((request) async {
        await request.drain<void>();
        received.add('${request.method} ${request.uri.path}');
        request.response.headers.contentType = ContentType.json;
        request.response.write(jsonEncode({'code': 0, 'data': 'created'}));
        await request.response.close();
      });
    });
    api.init('tcp', '127.0.0.1:${server.port}', '');
    Future<T> realHttp<T>(Future<T> Function() body) =>
        HttpOverrides.runZoned(body,
            createHttpClient: _RealHttp().createHttpClient);
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      await tester.runAsync(() => server.close(force: true));
      tester.binding.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      await tester.binding.setSurfaceSize(null);
      MacPanel.cancelAutoHide();
      Get.reset();
    });
    await tester.pumpWidget(GetMaterialApp(
        theme: macTheme(Brightness.light),
        home: const Scaffold(body: Text('panel'))));
    await tester.pumpAndSettle();
    // Browser inspection/status requests stay available before setup.
    await tester.runAsync(() => realHttp(() =>
        app.forwardBrowserRequest({'method': 'GET', 'path': '/api/v1/tasks'})));
    expect(received, ['GET /api/v1/tasks']);
    for (final endpoint in ['/api/v1/tasks', '/api/v1/tasks/batch']) {
      await expectLater(
          app.forwardBrowserRequest({
            'method': 'POST',
            'path': endpoint,
            'data': {
              'req': {'url': 'https://example.test/file'}
            }
          }),
          throwsA(isA<StateError>()));
    }
    await tester.pumpAndSettle();
    expect(find.text('下载新手教程'), findsOneWidget);
    expect(received, ['GET /api/v1/tasks']);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(MacOnboarding.completed, isFalse);
    await expectLater(
        app.forwardBrowserRequest({'method': 'POST', 'path': '/api/v1/tasks'}),
        throwsA(isA<StateError>()));
    await tester.pumpAndSettle();
    await tester.enterText(find.byKey(const ValueKey('onboarding-directory')),
        '/tmp/chosen-downloads');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(MacOnboarding.completed, isTrue);
    expect(app.downloaderConfig.value.downloadDir, '/tmp/chosen-downloads');
    expect(received, ['GET /api/v1/tasks']);
    final response =
        await tester.runAsync(() => realHttp(() => app.forwardBrowserRequest({
              'method': 'POST',
              'path': '/api/v1/tasks',
              'data': {
                'req': {'url': 'https://example.test/file'}
              }
            })));
    expect(response!.data['data'], 'created');
    expect(received, ['GET /api/v1/tasks', 'POST /api/v1/tasks']);
    expect(find.text('下载新手教程'), findsNothing);
    expect(tester.takeException(), isNull);
  });
}
