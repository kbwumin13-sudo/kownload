import 'dart:async';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task_stats.dart';
import 'package:gopeed/app/views/mac_task_failure.dart';

void main() {
  testWidgets(
      'failed row reads existing diagnostics once and omits signed URLs',
      (tester) async {
    var reads = 0;
    final result = Completer<TaskStats>();
    Future<TaskStats> read(String id) {
      reads++;
      return result.future;
    }

    Widget view() => MaterialApp(
        home: MacTaskFailure(
            taskId: 'a', style: const TextStyle(fontSize: 11), read: read));
    await tester.pumpWidget(view());
    expect(find.text('下载失败'), findsOneWidget);
    result.complete(const TaskStats(
        lastError:
            'HTTP 403 https://fixture.test/file?signature=synthetic Bearer fake-token'));
    await tester.pumpAndSettle();
    expect(find.text('下载失败 · HTTP 403 [下载地址] Bearer …'), findsOneWidget);
    await tester.pumpWidget(view());
    await tester.pumpAndSettle();
    expect(reads, 1);
    expect(find.textContaining('signature='), findsNothing);
    expect(find.textContaining('fake-token'), findsNothing);
  });
  for (final unavailable in [false, true]) {
    testWidgets(
        'missing diagnostics retain failure without invented cause; unavailable=$unavailable',
        (tester) async {
      await tester.pumpWidget(MaterialApp(
          home: MacTaskFailure(
              taskId: 'a',
              style: const TextStyle(),
              read: (_) async {
                if (unavailable) throw StateError('not available');
                return const TaskStats();
              })));
      await tester.pumpAndSettle();
      expect(find.text('下载失败'), findsOneWidget);
      expect(tester.takeException(), isNull);
    });
  }
  testWidgets('late diagnostics cannot label a different task', (tester) async {
    final old = Completer<TaskStats>(), next = Completer<TaskStats>();
    Future<TaskStats> read(String id) => id == 'a' ? old.future : next.future;
    Widget view(String id) => MaterialApp(
        home: MacTaskFailure(taskId: id, style: const TextStyle(), read: read));
    await tester.pumpWidget(view('a'));
    await tester.pumpWidget(view('b'));
    old.complete(const TaskStats(lastError: 'old reason'));
    await tester.pumpAndSettle();
    expect(find.textContaining('old reason'), findsNothing);
    next.complete(const TaskStats(lastError: 'new reason'));
    await tester.pumpAndSettle();
    expect(find.text('下载失败 · new reason'), findsOneWidget);
    final removed = Completer<TaskStats>();
    await tester.pumpWidget(MaterialApp(
        home: MacTaskFailure(
            taskId: 'removed',
            style: const TextStyle(),
            read: (_) => removed.future)));
    await tester.pumpWidget(const MaterialApp(home: Text('运行中')));
    removed.complete(const TaskStats(lastError: 'late after state change'));
    await tester.pumpAndSettle();
    expect(find.textContaining('late after state change'), findsNothing);
    expect(tester.takeException(), isNull);
  });
}
