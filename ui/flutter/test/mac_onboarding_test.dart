import 'dart:async';
import 'dart:io';

import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';

class _Box extends Fake implements Box {
  final stored = <dynamic, dynamic>{};
  bool failCompletion = false;

  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      stored[key] ?? defaultValue;

  @override
  Future<void> put(dynamic key, dynamic value) async {
    if (key == MacOnboarding.completedKey && failCompletion) {
      throw StateError('fixture storage failure');
    }
    stored[key] = value;
  }
}

class _App extends AppController {
  final savedDirectories = <String>[];
  bool failSave = false;
  Completer<void>? saving;

  @override
  void onReady() {}
  @override
  void onClose() {}

  @override
  Future<void> saveConfig() async {
    if (saving != null) await saving!.future;
    if (failSave) throw StateError('fixture config failure');
    savedDirectories.add(downloaderConfig.value.downloadDir);
  }
}

class _Picker extends FilePicker {
  String? selected;
  bool fail = false;
  @override
  Future<String?> getDirectoryPath({
    String? dialogTitle,
    bool lockParentWindow = false,
    String? initialDirectory,
  }) async {
    if (fail) throw StateError('fixture picker failure');
    return selected;
  }
}

void main() {
  late _Box box;
  late _App app;
  final nativeCalls = <MethodCall>[];

  setUp(() async {
    Get.testMode = true;
    box = _Box();
    Database.instance.box = box;
    app = _App();
    app.downloaderConfig.value.downloadDir = '/original';
    Get.put<AppController>(app);
    MacPanel.presentation.value = 'panel';
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      nativeCalls.add(call);
      return null;
    });
    await MacPanel.resize(260);
    nativeCalls.clear();
  });

  tearDown(() {
    MacOnboarding.helperDirectoryOverride = null;
    MacPanel.cancelAutoHide();
    Get.reset();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(
            const MethodChannel('plugins.flutter.io/url_launcher'), null);
  });

  Future<Future<bool> Function({bool force})> mount(WidgetTester tester) async {
    late BuildContext hostContext;
    await tester.binding.setSurfaceSize(const Size(640, 600));
    addTearDown(() => tester.binding.setSurfaceSize(null));
    await tester.pumpWidget(MaterialApp(
      theme: macTheme(Brightness.light),
      home: Builder(builder: (context) {
        hostContext = context;
        return const Scaffold(body: SizedBox.shrink());
      }),
    ));
    return ({bool force = false}) =>
        MacOnboarding.show(hostContext, force: force);
  }

  final directoryField = find.byKey(const ValueKey('onboarding-directory'));

  testWidgets(
      'first completion persists directory before marking; repeat skips',
      (tester) async {
    final launch = await mount(tester);
    final completed = launch();
    await tester.pumpAndSettle();
    expect(find.text('下载新手教程'), findsOneWidget);
    expect(box.stored[MacOnboarding.completedKey], isNull);
    expect(MacPanel.height, 520);
    expect(
        tester.getSize(find.byKey(const ValueKey('onboarding-content'))).width,
        lessThanOrEqualTo(480));
    await tester.enterText(directoryField, '  /chosen/downloads  ');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await completed, isTrue);
    expect(app.savedDirectories, ['/chosen/downloads']);
    expect(app.downloaderConfig.value.downloadDir, '/chosen/downloads');
    expect(box.stored[MacOnboarding.completedKey], isTrue);
    expect(MacPanel.height, 260);
    expect(await launch(), isTrue);
    await tester.pumpAndSettle();
    expect(find.byType(Dialog), findsNothing);
    expect(app.savedDirectories, hasLength(1));
    expect(
        nativeCalls
            .where((call) => call.method == 'setTransient')
            .map((call) => call.arguments['active']),
        [true, false]);
  });

  testWidgets('cancellation after editing leaves config and marker untouched',
      (tester) async {
    final launch = await mount(tester);
    final cancelled = launch();
    await tester.pumpAndSettle();
    await tester.enterText(directoryField, '/discarded');
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(await cancelled, isFalse);
    expect(app.savedDirectories, isEmpty);
    expect(app.downloaderConfig.value.downloadDir, '/original');
    expect(box.stored[MacOnboarding.completedKey], isNull);
  });

  testWidgets(
      'forced tutorial reopens and Escape cancels without resizing settings',
      (tester) async {
    box.stored[MacOnboarding.completedKey] = true;
    MacPanel.presentation.value = 'settings';
    final launch = await mount(tester);
    final cancelled = launch(force: true);
    await tester.pumpAndSettle();
    expect(find.text('下载新手教程'), findsOneWidget);
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pumpAndSettle();
    expect(await cancelled, isFalse);
    expect(box.stored[MacOnboarding.completedKey], isTrue);
    expect(nativeCalls.where((call) => call.method == 'resizePanel'), isEmpty);
  });

  testWidgets(
      'empty directory cannot complete; save failure stays open for retry',
      (tester) async {
    final launch = await mount(tester);
    final completed = launch();
    await tester.pumpAndSettle();
    await tester.enterText(directoryField, ' ');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('请选择下载保存位置。'), findsOneWidget);
    expect(app.savedDirectories, isEmpty);
    await tester.enterText(directoryField, '/retry');
    app.failSave = true;
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('保存失败，请重试。'), findsOneWidget);
    expect(app.downloaderConfig.value.downloadDir, '/original');
    expect(box.stored[MacOnboarding.completedKey], isNull);
    app.failSave = false;
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await completed, isTrue);
    expect(app.savedDirectories, ['/retry']);
  });

  testWidgets('completion write failure restores saved directory before cancel',
      (tester) async {
    final launch = await mount(tester);
    final cancelled = launch();
    await tester.pumpAndSettle();
    box.failCompletion = true;
    await tester.enterText(directoryField, '/not-committed');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('保存失败，请重试。'), findsOneWidget);
    expect(app.savedDirectories, ['/not-committed', '/original']);
    expect(app.downloaderConfig.value.downloadDir, '/original');
    expect(box.stored[MacOnboarding.completedKey], isNull);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(await cancelled, isFalse);
  });

  testWidgets(
      'overlapping requests share one dialog and save cannot double-submit',
      (tester) async {
    final launch = await mount(tester);
    final first = launch();
    final second = launch();
    await tester.pumpAndSettle();
    expect(find.byType(Dialog), findsOneWidget);
    app.saving = Completer<void>();
    await tester.tap(find.text('完成并继续'));
    await tester.pump();
    await tester.sendKeyEvent(LogicalKeyboardKey.enter);
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pump();
    expect(find.byType(Dialog), findsOneWidget);
    expect(box.stored[MacOnboarding.completedKey], isNull);
    app.saving!.complete();
    await tester.pumpAndSettle();
    expect(await first, isTrue);
    expect(await second, isTrue);
    expect(app.savedDirectories, ['/original']);
  });

  testWidgets(
      'native directory picker persists selection and reports failures nearby',
      (tester) async {
    final picker = _Picker()..fail = true;
    FilePicker.platform = picker;
    final launch = await mount(tester);
    final completed = launch();
    await tester.pumpAndSettle();
    await tester.tap(find.text('选择…'));
    await tester.pumpAndSettle();
    expect(find.text('无法选择文件夹，请重试或输入保存位置。'), findsOneWidget);
    picker
      ..fail = false
      ..selected = '/picked';
    await tester.tap(find.text('选择…'));
    await tester.pumpAndSettle();
    expect(
        tester.widget<TextField>(directoryField).controller!.text, '/picked');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await completed, isTrue);
    expect(app.savedDirectories, ['/picked']);
    expect(box.stored[MacOnboarding.completedKey], isTrue);
  });

  testWidgets(
      'browser setup has real instructions and a missing-folder fallback',
      (tester) async {
    final folder = Directory.systemTemp.createTempSync('kownload-guide-');
    addTearDown(() => folder.delete(recursive: true));
    MacOnboarding.helperDirectoryOverride = '${folder.path}/missing';
    final launches = <MethodCall>[];
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(
            const MethodChannel('plugins.flutter.io/url_launcher'),
            (call) async {
      launches.add(call);
      return true;
    });
    final launch = await mount(tester);
    final cancelled = launch();
    await tester.pumpAndSettle();
    await tester.tap(find.text('设置浏览器下载…'));
    await tester.pumpAndSettle();
    expect(find.text('设置浏览器下载'), findsOneWidget);
    expect(find.textContaining('chrome://extensions'), findsOneWidget);
    expect(find.textContaining('edge://extensions'), findsOneWidget);
    expect(find.textContaining('加载已解压的扩展程序'), findsOneWidget);
    expect(find.textContaining('回到视频或网盘网页并刷新'), findsOneWidget);
    await tester.tap(find.text('打开助手文件夹'));
    await tester.pumpAndSettle();
    expect(find.textContaining('此应用中没有附带下载助手'), findsOneWidget);
    expect(launches, isEmpty);
    await tester.tap(find.text('使用文档'));
    await tester.pumpAndSettle();
    expect(launches.single.method, 'launch');
    expect(launches.single.arguments['url'],
        'https://github.com/kbwumin13-sudo/kownload/blob/main/browser-media-extension/README.md');
    expect(box.stored[MacOnboarding.completedKey], isNull);
    expect(app.savedDirectories, isEmpty);
    await tester.tap(find.text('返回教程'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(await cancelled, isFalse);
  });

  testWidgets(
      'bundled browser helper opens externally without recording installation',
      (tester) async {
    final folder = Directory.systemTemp.createTempSync('kownload-helper-');
    addTearDown(() => folder.delete(recursive: true));
    MacOnboarding.helperDirectoryOverride = folder.path;
    final launches = <MethodCall>[];
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(
            const MethodChannel('plugins.flutter.io/url_launcher'),
            (call) async {
      launches.add(call);
      return true;
    });
    final launch = await mount(tester);
    final completed = launch();
    await tester.pumpAndSettle();
    await tester.tap(find.text('设置浏览器下载…'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('打开助手文件夹'));
    await tester.pumpAndSettle();
    expect(launches.single.arguments['url'],
        Uri.directory(folder.path).toString());
    expect(launches.single.arguments['useSafariVC'], isFalse);
    expect(launches.single.arguments['useWebView'], isFalse);
    expect(box.stored[MacOnboarding.completedKey], isNull);
    expect(app.savedDirectories, isEmpty);
    // Closing the optional instructions is separate from completing the tutorial.
    await tester.tap(find.text('返回教程'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await completed, isTrue);
    expect(app.savedDirectories, ['/original']);
    expect(box.stored[MacOnboarding.completedKey], isTrue);
    expect(
        nativeCalls
            .where((call) => call.method == 'setTransient')
            .map((call) => call.arguments['active']),
        [true, true, true, true, true, false]);
  });
}
