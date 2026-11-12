import 'dart:async';
import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/app/modules/create/views/create_path_intent.dart';
import 'package:gopeed/app/modules/create/views/netdisk_view.dart';
import 'package:gopeed/app/views/directory_selector.dart';
import 'package:gopeed/macos/mac_panel.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'netdisk_test_support.dart';

void main() {
  testWidgets(
      'prepared path keeps provenance and only queries the retained batch after unknown',
      (tester) async {
    final transport = MockNetdiskTransport();
    final picked = Completer<String?>();
    var choosing = false, submissions = 0;
    NetdiskPrepared? original;
    final received = <PathIntent>[];
    await tester.binding.setSurfaceSize(const Size(900, 850));
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      if (call.method == 'chooseDirectory') {
        choosing = true;
        return picked.future;
      }
      return null;
    });
    addTearDown(() async {
      if (!picked.isCompleted) picked.complete(null);
      await tester.pumpWidget(const SizedBox.shrink());
      await tester.binding.setSurfaceSize(null);
      tester.binding.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      Get.reset();
    });
    await tester.pumpWidget(GetMaterialApp(
        theme: macTheme(Brightness.light),
        home: Builder(
            builder: (context) => Scaffold(
                body: TextButton(
                    child: const Text('open-path-dialog'),
                    onPressed: () => showDialog<bool>(
                        context: context,
                        builder: (_) => NetdiskView(
                            input: const NetdiskInput(mockSource,
                                sessionId: 'owned-session', files: [mockFile]),
                            options: Options(path: '/tmp/original-root'),
                            pathIntent: const PathIntent(
                                path: '/tmp/original-root',
                                origin: PathOrigin.defaultPath),
                            api: transport.api,
                            browserTool: MockBrowserTool(),
                            submitPrepared: (prepared, intent) async {
                              submissions++;
                              original ??= prepared;
                              expect(identical(prepared, original), true);
                              received.add(intent);
                              throw const NetdiskException(
                                  'synthetic unknown ACK', 'query_required');
                            })))))));
    await tester.tap(find.text('open-path-dialog'));
    await tester.pumpAndSettle();
    final selector = find.byType(DirectorySelector);
    final folder = find.descendant(
        of: selector, matching: find.byIcon(CupertinoIcons.folder));
    await tester.ensureVisible(folder);
    await tester.pumpAndSettle();
    await tester.tap(folder);
    await tester.pump();
    expect(choosing, true);
    final input =
        find.descendant(of: selector, matching: find.byType(TextFormField));
    await tester.enterText(input, '/tmp/confirmed-root');
    await tester.pump();
    picked.complete('/tmp/stale-root');
    await tester.pumpAndSettle();
    expect(tester.widget<DirectorySelector>(selector).controller.text,
        '/tmp/confirmed-root');
    await tester.tap(find.text('确认下载'));
    await tester.pumpAndSettle();
    expect(submissions, 1);
    expect(received.single.path, '/tmp/confirmed-root');
    expect(received.single.origin, PathOrigin.userSelected);
    expect(transport.at('/prepare'), hasLength(1));
    expect(original!.batchId, 'batch');
    expect(original!.reqs.single['opts']['path'], '/tmp/confirmed-root');
    expect(original!.reqs.single['req']['extra']['sessionId'], 'owned-session');
    expect(transport.at('/tasks/batch'), isEmpty);
    await tester.tap(find.byKey(const ValueKey('netdisk-query-original')));
    await tester.pumpAndSettle();
    expect(submissions, 2);
    expect(transport.at('/prepare'), hasLength(1));
    expect(transport.at('/tasks/batch'), isEmpty);
    expect(transport.at('/cleanup'), isEmpty);
  });
}
