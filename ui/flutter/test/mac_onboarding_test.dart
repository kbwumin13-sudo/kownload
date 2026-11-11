import 'dart:async';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/onboarding_checks.dart';
import 'package:gopeed/macos/mac_widgets.dart';
import 'package:gopeed/macos/mac_theme.dart';

import 'support/onboarding_check_fixture.dart';

class _Box extends Fake implements Box {
  final stored = <dynamic, dynamic>{};
  bool failCompletion = false;
  int writes = 0;

  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      stored[key] ?? defaultValue;

  @override
  Future<void> put(dynamic key, dynamic value) async {
    writes++;
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

void main() {
  late _Box box;
  late _App app;
  final nativeCalls = <MethodCall>[];

  setUp(() async {
    Get.testMode = true;
    MacOnboarding.checksOverride = FixtureOnboardingChecks();
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
    MacOnboarding.checksOverride = null;
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
    await checkOnboardingDirectory(tester);
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
    await checkOnboardingDirectory(tester);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('请选择下载保存位置。'), findsOneWidget);
    expect(app.savedDirectories, isEmpty);
    await tester.enterText(directoryField, '/retry');
    app.failSave = true;
    await checkOnboardingDirectory(tester);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('保存失败，请重试。'), findsOneWidget);
    expect(app.downloaderConfig.value.downloadDir, '/original');
    expect(box.stored[MacOnboarding.completedKey], isNull);
    app.failSave = false;
    await checkOnboardingDirectory(tester);
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
    await checkOnboardingDirectory(tester);
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
    await checkOnboardingDirectory(tester);
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
    // The chooser is kownload's own AppKit panel, reached over the panel channel.
    Object? chosen =
        PlatformException(code: 'fixture', message: 'fixture picker failure');
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      nativeCalls.add(call);
      if (call.method != 'chooseDirectory') return null;
      if (chosen is PlatformException) throw chosen;
      return chosen;
    });
    final launch = await mount(tester);
    final completed = launch();
    await tester.pumpAndSettle();
    await tester.tap(find.text('选择…'));
    await tester.pumpAndSettle();
    expect(find.text('无法选择文件夹，请重试或输入保存位置。'), findsOneWidget);
    chosen = '/picked';
    await tester.tap(find.text('选择…'));
    await tester.pumpAndSettle();
    final choices =
        nativeCalls.where((call) => call.method == 'chooseDirectory').toList();
    expect(choices, hasLength(2));
    expect(choices.last.arguments,
        {'title': '选择下载保存位置', 'initialDirectory': '/original'});
    expect(
        tester.widget<TextField>(directoryField).controller!.text, '/picked');
    await checkOnboardingDirectory(tester);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await completed, isTrue);
    expect(app.savedDirectories, ['/picked']);
    expect(box.stored[MacOnboarding.completedKey], isTrue);
  });

  testWidgets(
      'browser setup has real instructions and a missing-folder fallback',
      (tester) async {
    final folder = Directory('/tmp').createTempSync('kownload-guide-');
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
    await tester.ensureVisible(find.text('设置浏览器下载…'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('设置浏览器下载…'));
    await tester.pumpAndSettle();
    expect(find.text('设置浏览器下载'), findsOneWidget);
    expect(find.textContaining('chrome://extensions'), findsOneWidget);
    expect(find.textContaining('edge://extensions'), findsOneWidget);
    expect(find.textContaining('加载已解压的扩展程序'), findsOneWidget);
    expect(find.textContaining('回到视频或网盘网页并刷新'), findsOneWidget);
    expect(find.textContaining('浏览器运行时连接尚未验证'), findsOneWidget);
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
    final folder = Directory('/tmp').createTempSync('kownload-helper-');
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
    await tester.ensureVisible(find.text('设置浏览器下载…'));
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
    await checkOnboardingDirectory(tester);
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

  testWidgets('current directory must be explicitly checked before completing',
      (tester) async {
    final launch = await mount(tester);
    final result = launch();
    await tester.pumpAndSettle();
    final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
    expect(checks.directories, isEmpty);
    expect(checks.tools, isEmpty);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(find.text('请先检查此文件夹的写入权限，再完成教程。'), findsOneWidget);
    expect(app.savedDirectories, isEmpty);
    await checkOnboardingDirectory(tester);
    expect(checks.directories, ['/original']);
    await tester.enterText(directoryField, '/different');
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(app.savedDirectories, isEmpty);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(await result, isFalse);
    expect(box.writes, 0);
  });

  for (final state in [
    DirectoryCheckState.missing,
    DirectoryCheckState.denied,
    DirectoryCheckState.cleanupFailed
  ]) {
    testWidgets('directory $state feedback keeps original settings and marker',
        (tester) async {
      final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
      checks.directoryState = state;
      final launch = await mount(tester);
      final result = launch();
      await tester.pumpAndSettle();
      await checkOnboardingDirectory(tester);
      expect(find.byKey(const ValueKey('onboarding-directory-result')),
          findsOneWidget);
      await tester.tap(find.text('完成并继续'));
      await tester.pumpAndSettle();
      expect(app.savedDirectories, isEmpty);
      expect(box.writes, 0);
      expect(app.downloaderConfig.value.downloadDir, '/original');
      await tester.tap(find.text('取消'));
      await tester.pumpAndSettle();
      expect(await result, isFalse);
    });
  }

  testWidgets(
      'late directory check cannot stamp a changed or reverted directory',
      (tester) async {
    final pending = Completer<DirectoryCheck>();
    final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
    checks.directoryResult = (_) => pending.future;
    final launch = await mount(tester);
    final result = launch();
    await tester.pumpAndSettle();
    final button = find.byKey(const ValueKey('onboarding-check-directory'));
    await tester.ensureVisible(button);
    await tester.tap(button);
    await tester.pump();
    final input = tester.widget<TextField>(directoryField).controller!;
    input.text = '/different';
    input.text = '/original';
    pending.complete(
        const DirectoryCheck('/original', DirectoryCheckState.writable));
    await tester.pumpAndSettle();
    expect(find.byKey(const ValueKey('onboarding-directory-result')),
        findsNothing);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(app.savedDirectories, isEmpty);
    expect(find.text('请先检查此文件夹的写入权限，再完成教程。'), findsOneWidget);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(await result, isFalse);
  });

  testWidgets('late check after dialog closes cannot mark completion or save',
      (tester) async {
    final pending = Completer<DirectoryCheck>();
    final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
    checks.directoryResult = (_) => pending.future;
    final launch = await mount(tester);
    final result = launch();
    await tester.pumpAndSettle();
    final button = find.byKey(const ValueKey('onboarding-check-directory'));
    await tester.ensureVisible(button);
    await tester.tap(button);
    await tester.pump();
    Navigator.of(
            tester.element(find.byKey(const ValueKey('onboarding-content'))))
        .pop(false);
    await tester.pumpAndSettle();
    expect(await result, isFalse);
    pending.complete(
        const DirectoryCheck('/original', DirectoryCheckState.writable));
    await tester.pumpAndSettle();
    expect(app.savedDirectories, isEmpty);
    expect(box.writes, 0);
    expect(tester.takeException(), isNull);
  });

  testWidgets(
      'completed v1 upgrade never auto-opens and unchanged manual review writes nothing',
      (tester) async {
    box.stored[MacOnboarding.completedKey] = true;
    final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
    final launch = await mount(tester);
    expect(await launch(), isTrue);
    await tester.pumpAndSettle();
    expect(find.byType(Dialog), findsNothing);
    expect(checks.directories, isEmpty);
    final review = launch(force: true);
    await tester.pumpAndSettle();
    expect(find.textContaining('本次为手动复查'), findsOneWidget);
    await checkOnboardingDirectory(tester);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await review, isTrue);
    expect(box.stored[MacOnboarding.completedKey], isTrue);
    expect(box.writes, 0);
    expect(app.savedDirectories, isEmpty);
    expect(app.downloaderConfig.value.downloadDir, '/original');
    expect(checks.tools, isEmpty);
  });

  testWidgets(
      'optional tools run only a clicked absolute path; Gatekeeper help keeps GUI instructions',
      (tester) async {
    final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
    app.downloaderConfig.value.protocolConfig.media.ffmpegPath =
        '/fixture/ffmpeg';
    app.downloaderConfig.value.protocolConfig.media.jsRuntimePath = 'node';
    final launch = await mount(tester);
    final result = launch();
    await tester.pumpAndSettle();
    final tools = find.byKey(const ValueKey('onboarding-tools'));
    await tester.ensureVisible(tools);
    await tester.tap(find.text('媒体工具（可选）'));
    await tester.pumpAndSettle();
    expect(checks.tools, isEmpty);
    final node = find.byKey(const ValueKey('onboarding-tool-node'));
    expect(tester.widget<MacButton>(node).onPressed, isNull);
    final ffmpeg = find.byKey(const ValueKey('onboarding-tool-ffmpeg'));
    await tester.ensureVisible(ffmpeg);
    await tester.pumpAndSettle();
    await tester.tap(ffmpeg);
    await tester.pumpAndSettle();
    expect(checks.tools, [(MediaTool.ffmpeg, '/fixture/ffmpeg')]);
    expect(find.text('fixture version only'), findsOneWidget);
    final help = find.text('macOS 首次打开帮助');
    await tester.ensureVisible(help);
    await tester.pumpAndSettle();
    await tester.tap(help);
    await tester.pumpAndSettle();
    expect(find.textContaining('尚未经过 Apple 公证'), findsOneWidget);
    expect(find.textContaining('仍要打开'), findsOneWidget);
    expect(find.textContaining('https://support.apple.com/zh-cn/102445'),
        findsOneWidget);
    expect(app.savedDirectories, isEmpty);
    expect(box.writes, 0);
    await tester.tap(find.text('取消'));
    await tester.pumpAndSettle();
    expect(await result, isFalse);
    expect(nativeCalls.where((call) => call.method == 'chooseFiles'), isEmpty);
    expect(tester.takeException(), isNull);
  });

  testWidgets('optional tool failure does not block ordinary download setup',
      (tester) async {
    final checks = MacOnboarding.checksOverride! as FixtureOnboardingChecks;
    checks.toolResult =
        const ToolCheck(ToolCheckState.failed, 'fixture tool unavailable');
    app.downloaderConfig.value.protocolConfig.media.ffmpegPath =
        '/fixture/ffmpeg';
    final launch = await mount(tester);
    final result = launch();
    await tester.pumpAndSettle();
    expect(checks.tools, isEmpty);
    await tester.ensureVisible(find.text('媒体工具（可选）'));
    await tester.tap(find.text('媒体工具（可选）'));
    await tester.pumpAndSettle();
    final button = find.byKey(const ValueKey('onboarding-tool-ffmpeg'));
    await tester.ensureVisible(button);
    await tester.pumpAndSettle();
    await tester.tap(button);
    await tester.pumpAndSettle();
    expect(find.text('fixture tool unavailable'), findsOneWidget);
    await checkOnboardingDirectory(tester);
    await tester.tap(find.text('完成并继续'));
    await tester.pumpAndSettle();
    expect(await result, isTrue);
    expect(app.savedDirectories, ['/original']);
    expect(box.stored[MacOnboarding.completedKey], isTrue);
    expect(checks.tools, hasLength(1));
    expect(tester.takeException(), isNull);
  });
}
