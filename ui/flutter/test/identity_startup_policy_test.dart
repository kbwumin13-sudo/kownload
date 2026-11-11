import 'dart:convert';
import 'dart:io';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/bootstrap.dart';
import 'package:gopeed/identity/startup_policy.dart';
import 'package:gopeed/identity/startup_review_notice.dart';
import 'package:gopeed/core/common/start_config.dart';

void main() {
  test('old markers hold automatic execution without silently authorizing it',
      () {
    for (final imported in [true, false]) {
      final policy = IdentityStartupPolicy.fromMarker(
          jsonEncode({'version': 1, 'imported': imported}));
      expect(policy.mode, 'imported-review');
      expect(policy.reason, 'legacy_marker');
      expect(policy.requiresReview, true);
    }
  });

  test('only supported explicit provenance admits normal startup', () {
    final policy = IdentityStartupPolicy.fromMarker(jsonEncode({
      'version': 1,
      'imported': false,
      'startupPolicy': {'version': 1, 'mode': 'normal', 'reason': 'fresh'}
    }));
    expect(policy.requiresReview, false);
    for (final entry in [
      {'version': 2, 'mode': 'normal', 'reason': 'fresh'},
      {'version': 1.0, 'mode': 'normal', 'reason': 'fresh'},
      {'version': 1, 'mode': 'future', 'reason': 'fresh'},
      {'version': 1, 'mode': 'normal', 'reason': 'unproven'},
      {'version': 1, 'mode': 'normal', 'reason': 'fresh'}
    ]) {
      expect(
          () => IdentityStartupPolicy.fromMarker(jsonEncode(
              {'version': 1, 'imported': true, 'startupPolicy': entry})),
          throwsFormatException);
    }
  });

  test('imported review never invokes startup resume and preserves user choice',
      () async {
    var resumed = 0;
    const savedAutoStart = true;
    final held =
        IdentityStartupPolicy.fromMarker('{"version":1,"imported":true}');
    final didResume = await resumeIdentityTasks(
        policy: held,
        autoStartTasks: savedAutoStart,
        isolated: false,
        resume: () async {
          resumed++;
        });
    expect(didResume, false);
    expect(resumed, 0);
    expect(savedAutoStart, true);
    expect(
        await resumeIdentityTasks(
            policy: const IdentityStartupPolicy.normal(),
            autoStartTasks: true,
            isolated: false,
            resume: () async {
              resumed++;
            }),
        true);
    expect(resumed, 1);
    await resumeIdentityTasks(
        policy: const IdentityStartupPolicy.normal(),
        autoStartTasks: false,
        isolated: false,
        resume: () async {
          resumed++;
        });
    await resumeIdentityTasks(
        policy: const IdentityStartupPolicy.normal(),
        autoStartTasks: true,
        isolated: true,
        resume: () async {
          resumed++;
        });
    expect(resumed, 1);
  });

  test(
      'real bootstrap reads admission after import without touching Hive bytes',
      () async {
    final dir = await Directory.systemTemp.createTemp('s0-bootstrap-');
    try {
      final hive = File('${dir.path}/new/database.hive');
      await hive.parent.create();
      await hive.writeAsString('unchanged private fixture');
      final policy = await runIdentityImport(
          source: '${dir.path}/old',
          target: '${dir.path}/new',
          run: (_) async {
            await File('${dir.path}/new/.kownload-identity.json')
                .writeAsString('{"version":1,"imported":true}');
            return ProcessResult(0, 0, '{"ok":true}', '');
          });
      expect(policy.mode, 'imported-review');
      expect(await hive.readAsString(), 'unchanged private fixture');
    } finally {
      await dir.delete(recursive: true);
    }
  });

  test('startup policy is serialized only as an ephemeral engine option', () {
    final cfg = StartConfig.fromJson({
      'network': 'unix',
      'address': '/tmp/fixture.sock',
      'storage': 'bolt',
      'storageDir': '/tmp/fixture',
      'refreshInterval': 0,
      'apiToken': ''
    });
    expect(cfg.startupTaskPolicy, 'normal');
    cfg.startupTaskPolicy = 'imported-review';
    expect(cfg.toJson()['startupTaskPolicy'], 'imported-review');
  });

  testWidgets('review is visible and dismissing it does not block a new action',
      (tester) async {
    var created = 0;
    await tester.pumpWidget(MaterialApp(
        home: Builder(
            builder: (context) => Scaffold(
                    body: Column(children: [
                  TextButton(
                      onPressed: () => showDialog<void>(
                          context: context,
                          builder: (_) => IdentityStartupReviewNotice(
                              policy: IdentityStartupPolicy.fromMarker(
                                  '{"version":1}'))),
                      child: const Text('show review')),
                  TextButton(
                      onPressed: () {
                        created++;
                      },
                      child: const Text('new action'))
                ])))));
    await tester.tap(find.text('show review'));
    await tester.pumpAndSettle();
    expect(find.textContaining('没有自动继续'), findsOneWidget);
    expect(find.textContaining('原自动开始设置保持'), findsOneWidget);
    await tester.tap(find.text('查看任务'));
    await tester.pumpAndSettle();
    await tester.tap(find.text('new action'));
    await tester.pump();
    expect(created, 1);
  });
}
