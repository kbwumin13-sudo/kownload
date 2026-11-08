import 'dart:io';

import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/views/directory_selector.dart';
import 'package:gopeed/macos/mac_panel.dart';

// Folder and file choosers on macOS are kownload's own AppKit panels, reached
// over the panel channel, so they come forward even without a Dock icon.
void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  final calls = <MethodCall>[];
  Object? answer;

  setUp(() {
    calls.clear();
    answer = null;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      calls.add(call);
      return call.method.startsWith('choose') ? answer : null;
    });
  });

  tearDown(() {
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });

  Future<int Function()> mount(
      WidgetTester tester, TextEditingController controller) async {
    var edits = 0;
    await tester.pumpWidget(MaterialApp(
        home: Scaffold(
            body: DirectorySelector(
                controller: controller,
                inlineActions: true,
                onEditComplete: () => edits++))));
    return () => edits;
  }

  testWidgets('the folder button opens the chooser at the current folder',
      (tester) async {
    if (!Platform.isMacOS) return;
    final controller = TextEditingController(text: '/Users/me/Downloads');
    final edits = await mount(tester, controller);
    answer = '/Volumes/Data/Inbox';
    await tester.tap(find.byIcon(CupertinoIcons.folder));
    await tester.pumpAndSettle();
    expect(calls.map((call) => call.method), ['chooseDirectory']);
    expect(calls.single.arguments, {'initialDirectory': '/Users/me/Downloads'});
    expect(controller.text, '/Volumes/Data/Inbox');
    expect(edits(), 1);
  });

  testWidgets('cancelling the chooser leaves the folder and the form as they were',
      (tester) async {
    if (!Platform.isMacOS) return;
    final controller = TextEditingController(text: '/Users/me/Downloads');
    final edits = await mount(tester, controller);
    await tester.tap(find.byIcon(CupertinoIcons.folder));
    await tester.pumpAndSettle();
    expect(calls.single.method, 'chooseDirectory');
    expect(controller.text, '/Users/me/Downloads');
    expect(edits(), 0);
  });

  test('file choosers pass their filter and return the chosen paths', () async {
    answer = ['/Users/me/Downloads/a.torrent'];
    expect(await MacPanel.chooseFiles(extensions: const ['torrent']),
        ['/Users/me/Downloads/a.torrent']);
    expect(calls.single.method, 'chooseFiles');
    expect(calls.single.arguments, {
      'extensions': ['torrent'],
      'multiple': false
    });
    answer = null;
    expect(await MacPanel.chooseFiles(), isNull);
    expect(calls.last.arguments, {'multiple': false});
  });
}
