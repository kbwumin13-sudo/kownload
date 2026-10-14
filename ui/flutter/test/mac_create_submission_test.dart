import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _Box extends Fake implements Box {
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) => defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {}
}

void main() {
  testWidgets(
      'successful capture clears authentication for the next site and advanced errors stay visible',
      (tester) async {
    if (!Platform.isMacOS) return;
    final requests = <Map<String, dynamic>>[];
    var reject = false;
    Future<String> submitTask(CreateTask request) async {
      requests.add(jsonDecode(jsonEncode(request.toJson())));
      if (reject) throw StateError('测试下载失败');
      return 'created';
    }

    Database.instance.box = _Box();
    final app = _App();
    app.downloaderConfig.value.downloadDir = '/tmp/kownload-test';
    app.downloaderConfig.value.extra.defaultDirectDownload = true;
    Get.put<AppController>(app);
    Get.put(CreateController());
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = CreateTask(
        req: Request(
            url: 'https://site-a.example/file',
            proxy: RequestProxy(
                mode: RequestProxyMode.custom,
                scheme: 'socks5',
                host: '127.0.0.1:1080'),
            extra: {
          'header': {
            'Cookie': 'fixture-secret',
            'Referer': 'https://site-a.example/'
          }
        }));
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(640, 760));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      MacPanel.cancelAutoHide();
      MacPanel.incoming.value = null;
      Get.reset();
      Get.routerDelegate = null;
      Get.routeInformationParser = null;
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(GetMaterialApp.router(
        theme: macTheme(Brightness.light),
        getPages: [
          GetPage(name: '/', page: () => CreateView(taskCreator: submitTask))
        ]));
    await tester.pumpAndSettle();
    Future<void> submit(Finder button) async {
      final expected = requests.length + 1;
      await tester.tap(button);
      await tester.pumpAndSettle();
      expect(requests.length, expected);
      await tester.pumpAndSettle();
      MacPanel.cancelAutoHide();
    }

    await submit(find.byTooltip('开始下载'));
    expect(
        requests.single['req']['extra']['header']['Cookie'], 'fixture-secret');
    expect(requests.single['req']['proxy']['scheme'], 'socks5');
    expect(requests.single['req']['proxy']['host'], '127.0.0.1:1080');
    await tester.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://site-b.example/file');
    await submit(find.byTooltip('开始下载'));
    expect(jsonEncode(requests.last), isNot(contains('fixture-secret')));
    expect(jsonEncode(requests.last), isNot(contains('site-a.example')));
    await tester.enterText(find.byKey(const ValueKey('panel-download-input')),
        'https://site-c.example/file');
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
    reject = true;
    await submit(find.text('confirm'));
    expect(find.textContaining('测试下载失败'), findsOneWidget);
    expect(tester.takeException(), isNull);
  });
}
