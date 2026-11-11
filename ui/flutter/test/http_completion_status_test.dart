import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart' hide Progress;
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/http_completion_projection.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/resource.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/app/modules/task/controllers/task_controller.dart';
import 'package:gopeed/app/modules/task/views/http_completion_status.dart';
import 'package:gopeed/app/modules/task/views/task_view.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';

HttpCompletionView phase(Object? value,
        {Object? version = 1,
        Object? capability = HttpCompletionView.capability}) =>
    HttpCompletionView.fromProjection(
        version: version, capability: capability, phase: value);

Task sample(String id, Protocol protocol) => Task(
      id: id,
      name: '$id.bin',
      meta: Meta(
          req: Request(url: 'https://fixture.invalid/$id'), opts: Options())
        ..res = Resource(size: 32768, files: const []),
      status: Status.running,
      uploading: false,
      progress: Progress(
          used: 0, speed: 0, downloaded: 32768, uploadSpeed: 0, uploaded: 0),
      createdAt: DateTime.utc(2026),
      updatedAt: DateTime.utc(2026),
    )..protocol = protocol;

class _App extends AppController {
  @override
  void onReady() {}
  @override
  void onClose() {}
}

class _Tasks extends MacTaskListController {
  _Tasks(List<Task> initial) {
    tasks.assignAll(initial);
  }
  @override
  // ignore: must_call_super
  void onInit() {
    isLoading.value = false;
  }

  @override
  // ignore: must_call_super
  void onClose() {}
  @override
  Future<void> getTasksState() async {}
}

void main() {
  const labels = {
    'prepared': '尚未校验',
    'waiting_verify': '等待校验',
    'verifying': '正在校验 SHA-256',
    'verified': '校验通过，等待保存',
    'published_pending_commit': '正在保存完成状态',
  };
  for (final entry in labels.entries) {
    test('${entry.key} is distinct from done despite all bytes present', () {
      expect(phase(entry.key).labelFor(Status.running), entry.value);
      expect(phase(entry.key).labelFor(Status.done), '校验完成状态未确认');
    });
  }
  test('committed must agree with Task done before reporting completion', () {
    expect(phase('committed').labelFor(Status.done), '校验并保存完成');
    expect(phase('committed').labelFor(Status.running), '校验完成状态未确认');
  });
  for (final version in [null, 0, 2, '1', 1.0, true]) {
    test('unknown version $version cannot imply checksum success', () {
      expect(phase('committed', version: version).labelFor(Status.done),
          '校验完成状态未确认');
    });
  }
  test('unknown capability and phase remain unknown', () {
    expect(phase('committed', capability: null).labelFor(Status.done),
        '校验完成状态未确认');
    expect(phase('committed', capability: 'other/v1').labelFor(Status.done),
        '校验完成状态未确认');
    for (final value in [null, 'done', 'COMMITTED', 1, {}, []]) {
      expect(phase(value).labelFor(Status.done), '校验完成状态未确认');
    }
  });
  test('paused and failed tasks do not claim an active verification run', () {
    expect(phase('verifying').labelFor(Status.pause), '校验任务已暂停');
    expect(phase('verified').labelFor(Status.error), '校验任务失败');
  });
  testWidgets(
      'normal TaskView consumes bound response projection and keeps unknown completion visible',
      (tester) async {
    final http = sample('http', Protocol.http);
    final unknown = sample('unknown', Protocol.http)..status = Status.done;
    http.completionView = HttpCompletionProjection.fromJson({
      'version': 1,
      'scopeVersion': 1,
      'datasetId': 'a' * 32,
      'taskId': 'http',
      'completionId': 'completion',
      'available': true,
      'phase': 'verifying',
      'recordedMatch': null,
      'errorCategory': null
    }, taskId: 'http')
      ..bindToDataset('a' * 32, isCurrent: () => true);
    unknown.completionView =
        HttpCompletionProjection.fromJson(null, taskId: 'unknown');
    final list = _Tasks([http, unknown]);
    Get.put<AppController>(_App());
    Get.put(TaskController());
    Get.put<MacTaskListController>(list);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(1000, 850));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      Get.reset();
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      await tester.binding.setSurfaceSize(null);
    });
    await tester.pumpWidget(GetMaterialApp(
        theme: macTheme(Brightness.light), home: const TaskView()));
    await tester.pumpAndSettle();
    expect(find.textContaining('正在校验 SHA-256'), findsOneWidget);
    await tester.tap(find.widgetWithText(TextButton, '已完成'));
    await tester.pumpAndSettle();
    expect(find.textContaining('校验完成状态未确认'), findsOneWidget);
    expect(find.textContaining('校验并保存完成'), findsNothing);
  });
  testWidgets(
      'actual TaskView rows consume only explicit HTTP phase projections',
      (tester) async {
    final http = sample('http', Protocol.http);
    final media = sample('media', Protocol.media)
      ..progress.mediaPhase = 'merge';
    final bt = sample('bt', Protocol.bt);
    final list = _Tasks([http, media, bt]);
    Get.put<AppController>(_App());
    Get.put(TaskController());
    Get.put<MacTaskListController>(list);
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (_) async => null);
    await tester.binding.setSurfaceSize(const Size(1000, 850));
    addTearDown(() async {
      await tester.pumpWidget(const SizedBox.shrink());
      Get.reset();
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      await tester.binding.setSurfaceSize(null);
    });
    Future<void> mount(HttpCompletionView value) async {
      await tester.pumpWidget(GetMaterialApp(
          theme: macTheme(Brightness.light),
          home: TaskView(httpCompletions: {
            for (final task in [http, media, bt]) task.id: value,
          })));
      await tester.pumpAndSettle();
    }

    await mount(phase('verifying'));
    expect(find.textContaining('正在校验 SHA-256'), findsOneWidget);
    expect(find.textContaining('Merging'), findsOneWidget);
    expect(find.textContaining('校验并保存完成'), findsNothing);
    await mount(phase('published_pending_commit'));
    expect(find.textContaining('正在保存完成状态'), findsOneWidget);
    http.status = Status.done;
    list.tasks.refresh();
    await tester.pump();
    await tester.tap(find.widgetWithText(TextButton, '已完成'));
    await tester.pumpAndSettle();
    expect(find.textContaining('校验完成状态未确认'), findsOneWidget);
    await mount(phase('committed'));
    expect(find.textContaining('校验并保存完成'), findsOneWidget);
    await tester.pumpWidget(GetMaterialApp(
        theme: macTheme(Brightness.light), home: const TaskView()));
    await tester.pumpAndSettle();
    expect(find.textContaining('校验并保存完成'), findsNothing);
    expect(find.textContaining('downloaded'.tr), findsOneWidget);
  });
}
