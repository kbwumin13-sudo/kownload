import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/macos/mac_theme.dart';
import 'package:gopeed/macos/mac_glass.dart';
import 'package:gopeed/macos/mac_widgets.dart';

void main() {
  testWidgets('empty, search, error and extension states have distinct symbols', (tester) async {
    for (final pair in <IconData, String>{
      CupertinoIcons.arrow_down_to_line: 'tray.and.arrow.down',
      CupertinoIcons.search: 'magnifyingglass',
      CupertinoIcons.exclamationmark_circle: 'exclamationmark.triangle',
      CupertinoIcons.square_stack: 'puzzlepiece.extension',
    }.entries) {
      await tester.pumpWidget(MaterialApp(home: MacEmptyState(icon: pair.key, title: 'State')));
      expect(tester.widget<MacSymbol>(find.byType(MacSymbol)).name, pair.value);
    }
  });
  for (final brightness in Brightness.values) {
    testWidgets('compact toolbar and empty state fit 800x600 in $brightness',
        (tester) async {
      tester.view.physicalSize = const Size(800, 600);
      tester.view.devicePixelRatio = 1;
      addTearDown(tester.view.resetPhysicalSize);
      addTearDown(tester.view.resetDevicePixelRatio);
      final search = TextEditingController();
      addTearDown(search.dispose);
      var added = false;
      await tester.pumpWidget(MaterialApp(
          theme: macTheme(brightness),
          home: Row(children: [
            const SizedBox(width: 208),
            Expanded(
                child: MacPage(
                    title: '下载任务',
                    actions: [
                      SizedBox(
                          width: 140,
                          child: MacSearchField(controller: search)),
                      MacButton(
                          primary: true,
                          icon: CupertinoIcons.add,
                          onPressed: () => added = true,
                          child: const Text('新建')),
                    ],
                    child: const MacEmptyState(
                        icon: CupertinoIcons.arrow_down_to_line,
                        title: '还没有下载任务',
                        message: '添加链接或拖入种子文件。'))),
          ])));
      expect(tester.takeException(), isNull);
      expect(tester.getSize(find.byType(MacSearchField)).height,
          tester.getSize(find.byType(MacButton)).height);
      await tester.tap(find.text('新建'));
      expect(added, isTrue);
      await tester.enterText(
          find.byType(CupertinoTextField), 'very long download search query');
      expect(search.text, 'very long download search query');
      expect(tester.takeException(), isNull);
    });
  }
}
