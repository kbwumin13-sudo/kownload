import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resource.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/app/modules/task/views/task_view.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/macos/mac_appearance.dart';

Task makeTask(String id, String name, String url, Status status) => Task(
      id: id,
      name: name,
      meta: Meta(
        req: Request(url: url),
        opts: Options(),
      )..res = Resource(files: const []),
      status: status,
      uploading: false,
      progress: Progress(
        used: 0,
        speed: 0,
        downloaded: 0,
        uploadSpeed: 0,
        uploaded: 0,
      ),
      createdAt: DateTime.utc(2026),
      updatedAt: DateTime.utc(2026),
    );

void main() {
  final tasks = [
    makeTask('run', 'Archive.ZIP', 'https://example.com/one', Status.running),
    makeTask('pause', 'Video', 'https://example.com/TWO', Status.pause),
    makeTask('ready', 'Queued', 'https://example.com/four', Status.ready),
    makeTask('wait', 'Waiting', 'https://example.com/five', Status.wait),
    makeTask('error', 'Failed', 'https://example.com/six', Status.error),
    makeTask('done', 'Report', 'https://example.com/three', Status.done),
  ];

  test('macOS segments keep active, completed and all tasks distinct', () {
    expect(filterMacTasks(tasks, 0, '').map((task) => task.id), [
      'run',
      'pause',
      'ready',
      'wait',
      'error',
    ]);
    expect(filterMacTasks(tasks, 1, '').map((task) => task.id), ['done']);
    expect(filterMacTasks(tasks, 2, '').map((task) => task.id), [
      'run',
      'pause',
      'ready',
      'wait',
      'error',
      'done',
    ]);
  });

  test('macOS local search matches name and URL without case sensitivity', () {
    expect(filterMacTasks(tasks, 2, ' archive ').map((task) => task.id), [
      'run',
    ]);
    expect(filterMacTasks(tasks, 0, 'two').map((task) => task.id), ['pause']);
    expect(filterMacTasks(tasks, 1, 'archive'), isEmpty);
  });

  test('macOS selection keeps an anchor across ranges and keyboard moves', () {
    final list = _FakeMacTaskListController(tasks);
    final ids = tasks.map((task) => task.id).toList();
    list.selectRow(ids, 'pause');
    list.selectRow(ids, 'wait', shift: true);
    expect(list.selectedTaskIds, ['pause', 'ready', 'wait']);
    list.selectRow(ids, 'run', toggle: true);
    expect(list.selectedTaskIds, ['pause', 'ready', 'wait', 'run']);
    list.selectRow(ids, 'ready', shift: true);
    expect(list.selectedTaskIds, ['run', 'pause', 'ready']);
    expect(list.moveSelection(ids, direction: 1, shift: true), 'wait');
    expect(list.selectedTaskIds, ['run', 'pause', 'ready', 'wait']);
    expect(list.moveSelection(ids, direction: 2), 'done');
    expect(list.selectedTaskIds, ['done']);
    list.selectVisible(ids.sublist(0, 3));
    expect(list.selectedTaskIds, ['run', 'pause', 'ready']);
    list.reconcileSelection(['pause', 'ready']);
    expect(list.selectedTaskIds, ['pause', 'ready']);
    expect(list.selectionAnchor, isNull);
  });

  testWidgets(
    'macOS task page filters, focuses search, selects visible rows and toggles inspector',
    (tester) async {
      if (!Platform.isMacOS) return;
      final list = _FakeMacTaskListController(tasks);
      Get.put<AppController>(_FakeAppController());
      Get.put<TaskController>(TaskController());
      Get.put<MacTaskListController>(list);
      addTearDown(Get.reset);
      final inspectorStates = <Map<dynamic, dynamic>>[];
      const channel = MethodChannel('gopeed/macos_appearance');
      tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
        channel,
        (call) async {
          if (call.method == 'inspector') {
            inspectorStates.add(call.arguments as Map<dynamic, dynamic>);
          }
          return null;
        },
      );
      addTearDown(() => tester.binding.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, null));

      await tester.pumpWidget(
        const GetMaterialApp(home: Scaffold(body: TaskView())),
      );
      await tester.pump();

      expect(find.text('Archive.ZIP'), findsOneWidget);
      expect(find.text('Report'), findsNothing);
      list.error.value = 'offline';
      await tester.pump();
      expect(find.text('Archive.ZIP'), findsOneWidget);
      expect(find.text('刷新失败，正在重试'), findsOneWidget);
      list.error.value = null;
      await tester.pump();
      await tester.sendKeyDownEvent(LogicalKeyboardKey.metaLeft);
      await tester.sendKeyEvent(LogicalKeyboardKey.keyF);
      await tester.sendKeyUpEvent(LogicalKeyboardKey.metaLeft);
      await tester.pump();
      expect(
        tester
            .widget<CupertinoTextField>(find.byType(CupertinoTextField))
            .focusNode!
            .hasFocus,
        isTrue,
      );

      await tester.enterText(find.byType(CupertinoTextField), 'archive');
      await tester.pump();
      expect(find.text('Archive.ZIP'), findsOneWidget);
      expect(find.text('Video'), findsNothing);
      await tester.tap(find.text('Archive.ZIP'));
      await tester.pump(const Duration(milliseconds: 350));
      expect(find.text('https://example.com/one'), findsNothing);
      expect(inspectorStates.last, {'visible': false, 'enabled': true});
      MacAppearance.inspectorRequests.value++;
      await tester.pump();
      expect(find.text('https://example.com/one'), findsOneWidget);
      expect(inspectorStates.last, {'visible': true, 'enabled': true});
      await tester.sendKeyEvent(LogicalKeyboardKey.escape);
      await tester.pump();
      expect(find.text('https://example.com/one'), findsNothing);
      expect(inspectorStates.last, {'visible': false, 'enabled': true});
      MacAppearance.inspectorRequests.value++;
      await tester.pump();
      expect(find.text('https://example.com/one'), findsOneWidget);
      await tester.sendKeyDownEvent(LogicalKeyboardKey.metaLeft);
      await tester.sendKeyEvent(LogicalKeyboardKey.keyA);
      await tester.sendKeyUpEvent(LogicalKeyboardKey.metaLeft);
      await tester.pump();
      expect(list.selectedTaskIds, ['run']);

      tester
          .widget<CupertinoSlidingSegmentedControl<int>>(
            find.byType(CupertinoSlidingSegmentedControl<int>),
          )
          .onValueChanged(1);
      await tester.pump();
      expect(find.text('Report'), findsNothing);
      expect(Get.find<TaskController>().selectTask.value, isNull);
      expect(list.selectedTaskIds, isEmpty);
      expect(find.text('https://example.com/one'), findsNothing);
      await tester.enterText(find.byType(CupertinoTextField), '');
      await tester.pump();
      expect(find.text('Report'), findsOneWidget);

      tester
          .widget<CupertinoSlidingSegmentedControl<int>>(
            find.byType(CupertinoSlidingSegmentedControl<int>),
          )
          .onValueChanged(2);
      await tester.pump();
      await tester.tap(find.text('Video'));
      await tester.pump(const Duration(milliseconds: 350));
      await tester.sendKeyDownEvent(LogicalKeyboardKey.shiftLeft);
      await tester.tap(find.text('Waiting'));
      await tester.pump(const Duration(milliseconds: 350));
      await tester.sendKeyUpEvent(LogicalKeyboardKey.shiftLeft);
      expect(list.selectedTaskIds, ['pause', 'ready', 'wait']);
      await tester.sendKeyDownEvent(LogicalKeyboardKey.metaLeft);
      await tester.tap(find.text('Archive.ZIP'));
      await tester.sendKeyUpEvent(LogicalKeyboardKey.metaLeft);
      await tester.pump(const Duration(milliseconds: 350));
      expect(list.selectedTaskIds, ['pause', 'ready', 'wait', 'run']);
      await tester.sendKeyEvent(LogicalKeyboardKey.arrowDown);
      expect(list.selectedTaskIds, ['pause']);
      await tester.sendKeyDownEvent(LogicalKeyboardKey.shiftLeft);
      await tester.sendKeyEvent(LogicalKeyboardKey.arrowDown);
      await tester.sendKeyUpEvent(LogicalKeyboardKey.shiftLeft);
      expect(list.selectedTaskIds, ['pause', 'ready']);
      await tester.sendKeyEvent(LogicalKeyboardKey.home);
      expect(list.selectedTaskIds, ['run']);
      await tester.sendKeyEvent(LogicalKeyboardKey.end);
      expect(list.selectedTaskIds, ['done']);
      await tester.sendKeyDownEvent(LogicalKeyboardKey.metaLeft);
      await tester.sendKeyEvent(LogicalKeyboardKey.keyA);
      await tester.sendKeyUpEvent(LogicalKeyboardKey.metaLeft);
      expect(list.selectedTaskIds, tasks.map((task) => task.id).toList());

      MacAppearance.inspectorRequests.value++;
      await tester.pump();
      expect(find.text('https://example.com/three'), findsOneWidget);
      final handle = find.byWidgetPredicate((widget) =>
          widget is MouseRegion &&
          widget.cursor == SystemMouseCursors.resizeLeftRight);
      final initialLeft = tester.getTopLeft(handle).dx;
      await tester.drag(handle, const Offset(-100, 0));
      await tester.pump();
      expect(tester.getTopLeft(handle).dx, lessThan(initialLeft));
      await tester.binding.setSurfaceSize(const Size(600, 700));
      addTearDown(() => tester.binding.setSurfaceSize(null));
      await tester.pump();
      expect(tester.takeException(), isNull);
      expect(tester.getTopLeft(handle).dx, greaterThanOrEqualTo(300));

      list.selectedTaskIds([]);
      await tester.tap(find.byType(CupertinoTextField));
      await tester.sendKeyDownEvent(LogicalKeyboardKey.metaLeft);
      await tester.sendKeyEvent(LogicalKeyboardKey.keyA);
      await tester.sendKeyUpEvent(LogicalKeyboardKey.metaLeft);
      expect(list.selectedTaskIds, isEmpty);
    },
  );
}

class _FakeAppController extends AppController {
  @override
  void onReady() {}

  @override
  void onClose() {}
}

class _FakeMacTaskListController extends MacTaskListController {
  final List<Task> initialTasks;
  _FakeMacTaskListController(this.initialTasks);

  @override
  // The fake deliberately replaces the real controller's network timer.
  // ignore: must_call_super
  void onInit() {
    tasks.assignAll(initialTasks);
    isLoading.value = false;
  }

  @override
  void onClose() {}
}
