import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:package_info_plus/package_info_plus.dart';
import 'package:gopeed/api/api.dart' as api;
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/app/views/app_view.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/modules/setting/controllers/setting_controller.dart';
import 'package:gopeed/app/modules/task/views/task_view.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/identity/app_identity.dart';
import 'package:gopeed/identity/browser_connection.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_shell.dart';
import 'package:gopeed/util/package_info.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _Settings extends SettingController {
  @override
  void fetchLatestVersion() {}
}

class _Box extends Fake implements Box {
  final _storedValues = <dynamic, dynamic>{
    MacOnboarding.completedKey: 'true',
  };
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      _storedValues[key] ?? defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {
    _storedValues[key] = value;
  }
}

class _HttpClients extends HttpOverrides {
  final clients = <HttpClient>[];
  @override
  HttpClient createHttpClient(SecurityContext? context) {
    final client = super.createHttpClient(context);
    clients.add(client);
    return client;
  }
}

BrowserConnectionReport _report(String browser) => BrowserConnectionReport(
    browser,
    const ManifestEvidence(AppIdentity.hostName,
        present: false,
        schemaValid: false,
        allowedOrigin: false,
        owner: 'missing',
        executable: 'unavailable'),
    const ManifestEvidence(AppIdentity.legacyHostName,
        present: false,
        schemaValid: false,
        allowedOrigin: false,
        owner: 'missing',
        executable: 'unavailable'));

Future<void> _pump(WidgetTester tester) async {
  for (var i = 0; i < 20; i++) {
    await tester
        .runAsync(() => Future<void>.delayed(const Duration(milliseconds: 5)));
    await tester.pump(const Duration(milliseconds: 25));
  }
}

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  testWidgets(
    'empty macOS task list opens the create panel through the default router',
    (tester) async {
      if (!Platform.isMacOS) return;
      final oldOverrides = HttpOverrides.current;
      final oldInspect = BrowserConnection.inspect;
      final clients = _HttpClients();
      HttpOverrides.global = clients;
      final requests = <(String, String)>[];
      final panelCalls = <MethodCall>[];
      final server = (await tester
          .runAsync(() => HttpServer.bind(InternetAddress.loopbackIPv4, 0)))!;
      server.listen((request) async {
        requests.add((request.method, request.uri.path));
        request.response.headers.contentType = ContentType.json;
        request.response.statusCode = request.method == 'GET' ? 200 : 405;
        request.response.write(jsonEncode({
          'code': request.method == 'GET' ? 0 : 1000,
          'msg': '',
          'data': request.uri.path == '/api/v1/tasks' ? [] : null,
        }));
        await request.response.close();
      });
      const channels = [
        MacPanel.channel,
        MethodChannel('gopeed/macos_appearance'),
        MethodChannel('window_manager'),
      ];
      try {
        await tester.binding.setSurfaceSize(const Size(900, 732));
        for (final channel in channels) {
          TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
              .setMockMethodCallHandler(channel, (call) async {
            if (channel == MacPanel.channel) panelCalls.add(call);
            if (call.method == 'getState') {
              return {
                'mode': MacPanel.presentation.value,
                'width': 900,
                'height': 700
              };
            }
            if (call.method == 'getShowInDock') return false;
            return null;
          });
        }
        packageInfo = PackageInfo(
            appName: 'kownload',
            packageName: 'fixture',
            version: '1.9.3',
            buildNumber: '41');
        Database.instance.box = _Box();
        BrowserConnection.inspect = (browser) async => _report(browser);
        final app = _App();
        app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
        app.downloaderConfig.value.extra.locale = 'zh_CN';
        app.downloaderConfig.value.extra.themeMode = 'system';
        Get.put<AppController>(app);
        Get.put<SettingController>(_Settings());
        api.init('tcp', '127.0.0.1:${server.port}', '');

        // Keep AppPages and its default GetDelegate intact. The native host uses
        // this same settings entry; only the following taps navigate the user.
        await tester.pumpWidget(const AppView());
        await _pump(tester);
        await MacPanel.showSettings();
        await _pump(tester);
        expect(find.byType(MacShell), findsOneWidget);
        await tester.tap(find.text('下载列表').first);
        await _pump(tester);
        expect(Get.rootDelegate.currentConfiguration?.uri.path, Routes.TASK);
        expect(find.byType(TaskView), findsOneWidget);
        expect(find.text('暂无任务'), findsOneWidget);
        expect(find.text('创建任务'), findsOneWidget);

        await tester.tap(find.text('创建任务'));
        await _pump(tester);
        expect(find.byType(CreateView), findsOneWidget);
        expect(
            find.byKey(const ValueKey('panel-download-input')), findsOneWidget);
        expect(find.text('更多选项'), findsOneWidget);
        expect(MacPanel.presentation.value, 'panel');
        expect(Get.rootDelegate.currentConfiguration?.uri.path, Routes.TASK);
        expect(
            panelCalls.any((call) =>
                call.method == 'setPresentation' &&
                call.arguments['mode'] == 'panel' &&
                call.arguments['show'] == true),
            isTrue);

        await tester.tap(find.text('更多选项'));
        await _pump(tester);
        expect(
            find.byKey(const ValueKey('http-expected-sha256')), findsOneWidget);
        await tester.tap(find.text('取消').last);
        await _pump(tester);
        expect(
            find.byKey(const ValueKey('panel-download-input')), findsOneWidget);
        expect(requests, isNotEmpty);
        expect(requests.every((request) => request.$1 == 'GET'), isTrue);
        expect(tester.takeException(), isNull);
      } finally {
        await tester.pumpWidget(const SizedBox.shrink());
        await tester.pump();
        Get.reset();
        Get.routerDelegate = null;
        Get.routeInformationParser = null;
        MacPanel.cancelAutoHide();
        for (final client in clients.clients) {
          client.close(force: true);
        }
        await tester.runAsync(() => server.close(force: true));
        BrowserConnection.inspect = oldInspect;
        HttpOverrides.global = oldOverrides;
        for (final channel in channels) {
          TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
              .setMockMethodCallHandler(channel, null);
        }
        await tester.binding.setSurfaceSize(null);
        await tester.pump(const Duration(seconds: 2));
      }
    },
    variant: TargetPlatformVariant.only(TargetPlatform.macOS),
  );
}
