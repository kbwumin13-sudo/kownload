import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/api/model/create_task.dart';
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/create/controllers/create_controller.dart';
import 'package:gopeed/app/modules/create/views/create_path_intent.dart';
import 'package:gopeed/app/modules/create/views/create_view.dart';
import 'package:gopeed/app/views/directory_selector.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/i18n/message.dart' as i18n;
import 'package:gopeed/macos/mac_onboarding.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_widgets.dart';
import 'package:gopeed/util/util.dart';

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
  @override
  void completeBrowserConfirmation(String id,
      {List<String> taskIds = const [], bool cancelled = false}) {}
}

class _Box extends Fake implements Box {
  final _values = <dynamic, dynamic>{MacOnboarding.completedKey: true};
  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      _values[key] ?? defaultValue;
  @override
  Future<void> put(dynamic key, dynamic value) async {
    _values[key] = value;
  }
}

class _NoNetwork extends HttpOverrides {
  int attempts = 0;
  @override
  HttpClient createHttpClient(SecurityContext? context) {
    attempts++;
    throw StateError('unexpected network request');
  }
}

void main() {
  late Directory root;
  late _App app;
  late GlobalKey<CreateViewState> key;
  late _NoNetwork network;
  HttpOverrides? oldOverrides;
  final creates = <String>[];
  final gates = <Completer<String?>>[];
  Object? pickerAnswer;
  var pickerCalls = 0, edits = 0;
  String getDefault() => '${root.path}/default';
  String getCategory() => '${root.path}/category';

  Future<void> mount(WidgetTester tester, {CreateTask? incoming}) async {
    root = Directory('/tmp').createTempSync('k16-path-intent-');
    await tester.runAsync(() => Util.initStorageDir(previewStorage: root.path));
    oldOverrides = HttpOverrides.current;
    network = _NoNetwork();
    HttpOverrides.global = network;
    Database.instance.box = _Box();
    app = _App();
    app.downloaderConfig.value.downloadDir = getDefault();
    app.downloaderConfig.value.protocolConfig.http.connections = 1;
    app.downloaderConfig.value.extra.defaultDirectDownload = true;
    app.downloaderConfig.value.extra.downloadCategories = [
      DownloadCategory(name: 'fixture category', path: getCategory())
    ];
    Get.put<AppController>(app);
    Get.put(CreateController());
    MacPanel.presentation.value = 'panel';
    MacPanel.incoming.value = incoming;
    MacPanel.taskResult.value = null;
    key = GlobalKey<CreateViewState>();
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      if (call.method == 'chooseDirectory') {
        pickerCalls++;
        final answer = pickerAnswer;
        return answer is Completer<String?> ? await answer.future : answer;
      }
      return null;
    });
    await tester.binding.setSurfaceSize(const Size(900, 1400));
    await tester.pumpWidget(GetMaterialApp.router(
        theme: macTheme(Brightness.light),
        translations: i18n.messages,
        locale: const Locale('zh', 'CN'),
        getPages: [
          GetPage(
              name: '/',
              page: () => CreateView(
                  key: key,
                  taskCreator: (request) async {
                    creates.add(jsonEncode(request.toJson()));
                    return 'fixture-${creates.length}';
                  }))
        ]));
    await tester.pumpAndSettle();
  }

  Future<void> expand(WidgetTester tester) async {
    await tester.tap(find.text('更多选项'));
    await tester.pumpAndSettle();
  }

  Future<void> pick(WidgetTester tester) async {
    await tester.tap(find.descendant(
        of: find.byType(DirectorySelector), matching: find.byType(IconButton)));
    await tester.pump();
  }

  void widgetTest(String name, Future<void> Function(WidgetTester) body) {
    testWidgets(name, (tester) async {
      if (!Platform.isMacOS) return;
      try {
        await body(tester);
        expect(network.attempts, 0);
        expect(tester.takeException(), isNull);
      } finally {
        for (final gate in gates) {
          if (!gate.isCompleted) gate.complete(null);
        }
        await tester.pumpWidget(const SizedBox.shrink());
        await tester.pump();
        MacPanel.cancelAutoHide();
        MacPanel.incoming.value = null;
        MacPanel.taskResult.value = null;
        Get.reset();
        Get.routerDelegate = null;
        Get.routeInformationParser = null;
        tester.binding.defaultBinaryMessenger
            .setMockMethodCallHandler(MacPanel.channel, null);
        HttpOverrides.global = oldOverrides;
        await tester.binding.setSurfaceSize(null);
        await tester.runAsync(() => Util.initStorageDir());
        root.deleteSync(recursive: true);
      }
    });
  }

  setUp(() {
    creates.clear();
    gates.clear();
    pickerAnswer = null;
    pickerCalls = 0;
    edits = 0;
  });

  widgetTest(
      'path intent default and same-path picker confirmation stay distinct',
      (tester) async {
    await mount(tester);
    expect(key.currentState!.pathIntent.origin, PathOrigin.defaultPath);
    expect(key.currentState!.pathIntent.path, getDefault());
    await expand(tester);
    await pick(tester);
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.origin, PathOrigin.defaultPath);
    pickerAnswer = getDefault();
    await pick(tester);
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.origin, PathOrigin.userSelected);
    final snapshot = key.currentState!.pathIntent;
    app.downloaderConfig.value.downloadDir = '${root.path}/new-default';
    app.downloaderConfig.refresh();
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.path, snapshot.path);
    expect(snapshot.origin, PathOrigin.userSelected);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.origin, PathOrigin.userSelected);
    expect(creates, isEmpty);
  });

  widgetTest('path intent category rejects an older pending picker revision',
      (tester) async {
    await mount(tester);
    await expand(tester);
    final gate = Completer<String?>();
    gates.add(gate);
    pickerAnswer = gate;
    await pick(tester);
    await tester.tap(find.text('fixture category'));
    await tester.pump();
    gate.complete('${root.path}/late-picker');
    await tester.pumpAndSettle();
    final selector =
        tester.widget<DirectorySelector>(find.byType(DirectorySelector));
    expect(selector.controller.text, getCategory());
    expect(key.currentState!.pathIntent.path, getCategory());
    expect(key.currentState!.pathIntent.origin, PathOrigin.manualCategory);
    expect(creates, isEmpty);
  });

  widgetTest('path intent new draft rejects late picker on the same controller',
      (tester) async {
    await mount(tester);
    await expand(tester);
    final controller = tester
        .widget<DirectorySelector>(find.byType(DirectorySelector))
        .controller;
    final gate = Completer<String?>();
    gates.add(gate);
    pickerAnswer = gate;
    await pick(tester);
    final incoming = '${root.path}/incoming';
    await MacPanel.showPanel(
        task: CreateTask(
            req: Request(url: 'https://fixture.invalid/new'),
            opts: Options(path: incoming)));
    await tester.pump();
    expect(
        tester
            .widget<DirectorySelector>(find.byType(DirectorySelector))
            .controller,
        same(controller));
    gate.complete('${root.path}/old-draft-picker');
    await tester.pumpAndSettle();
    expect(controller.text, incoming);
    expect(key.currentState!.pathIntent.path, incoming);
    expect(key.currentState!.pathIntent.origin, PathOrigin.unknown);
    app.downloaderConfig.value.downloadDir = '${root.path}/later-default';
    app.downloaderConfig.refresh();
    await tester.pumpAndSettle();
    expect(controller.text, incoming);
    expect(creates, isEmpty);
  });

  widgetTest('path intent follows real browser cancel and ordinary reset',
      (tester) async {
    await mount(tester);
    await MacPanel.showPanel(
        task: CreateTask(
            req: Request(url: 'https://fixture.invalid/cancel'),
            opts: Options(path: getDefault()))
          ..browserOperationId = 'fixture-cancel');
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.origin, PathOrigin.unknown);
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.origin, PathOrigin.defaultPath);
    expect(key.currentState!.pathIntent.path, getDefault());
    expect(creates, isEmpty);
  });

  widgetTest('path intent is absent from equal legacy create payloads',
      (tester) async {
    await mount(tester);
    const url = 'https://fixture.invalid/payload.bin';
    await tester.enterText(
        find.byKey(const ValueKey('panel-download-input')), url);
    await tester.pumpAndSettle();
    final start = find.descendant(
        of: find.byTooltip('开始下载'), matching: find.byType(MacButton));
    expect(tester.widget<MacButton>(start).onPressed, isNotNull);
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pumpAndSettle();
    MacPanel.cancelAutoHide();
    expect(creates, hasLength(1),
        reason: tester.widgetList<Text>(find.byType(Text))
            .map((text) => text.data).whereType<String>().join('\n'));
    expect(key.currentState!.pathIntent.origin, PathOrigin.defaultPath);
    await expand(tester);
    pickerAnswer = getDefault();
    await pick(tester);
    await tester.pumpAndSettle();
    expect(key.currentState!.pathIntent.origin, PathOrigin.userSelected);
    await tester.tap(find.text('取消').last);
    await tester.pumpAndSettle();
    await tester.enterText(
        find.byKey(const ValueKey('panel-download-input')), url);
    await tester.pumpAndSettle();
    expect(tester.widget<MacButton>(start).onPressed, isNotNull);
    await tester.tap(find.byTooltip('开始下载'));
    await tester.pumpAndSettle();
    MacPanel.cancelAutoHide();
    expect(creates, hasLength(2));
    expect(creates.last, creates.first);
    expect(
        creates.first,
        jsonEncode(CreateTask(
                req: Request(url: url),
                opts: Options(
                    path: getDefault(), extra: OptsExtraHttp(connections: 1)))
            .toJson()));
  });

  widgetTest(
      'directory picker without provenance callback keeps legacy edit behavior',
      (tester) async {
    await mount(tester);
    final controller = TextEditingController(text: getDefault());
    addTearDown(controller.dispose);
    await tester.pumpWidget(MaterialApp(
        home: Scaffold(
            body: DirectorySelector(
                controller: controller,
                inlineActions: true,
                onEditComplete: () => edits++))));
    pickerAnswer = '${root.path}/legacy-picker';
    await pick(tester);
    await tester.pumpAndSettle();
    expect(controller.text, pickerAnswer);
    expect(edits, 1);
    pickerAnswer = null;
    await pick(tester);
    await tester.pumpAndSettle();
    expect(controller.text, '${root.path}/legacy-picker');
    expect(edits, 1);
    expect(pickerCalls, 2);
  });
}
