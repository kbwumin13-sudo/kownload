import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/identity/bootstrap.dart';
import 'package:gopeed/identity/bootstrap_error_page.dart';

Future<void> _mount(WidgetTester tester, IdentityBootstrapException error,
    VoidCallback onQuit) async {
  await tester.binding.setSurfaceSize(const Size(720, 900));
  addTearDown(() => tester.binding.setSurfaceSize(null));
  await tester.pumpWidget(
      MaterialApp(home: BootstrapErrorPage(error: error, onQuit: onQuit)));
}

void main() {
  const keep = '保留新数据，今后忽略旧目录的变化';
  testWidgets('the persistent choice is stored once, then the app can quit',
      (tester) async {
    var calls = 0, quits = 0;
    await _mount(
        tester,
        IdentityBootstrapException('legacy changed',
            ignoreLegacyChanges: () async {
          calls++;
          return true;
        }),
        () => quits++);
    expect(find.text('legacy changed'), findsOneWidget);
    await tester.tap(find.text(keep));
    await tester.pumpAndSettle();
    expect(calls, 1);
    expect(find.textContaining('已记住'), findsOneWidget);
    expect(tester.widget<OutlinedButton>(find.byType(OutlinedButton)).onPressed,
        isNull);
    await tester.tap(find.text('退出'));
    expect(quits, 1);
  });
  testWidgets('a failed choice can be retried and offers no quit shortcut',
      (tester) async {
    var calls = 0;
    await _mount(
        tester,
        IdentityBootstrapException('legacy changed',
            ignoreLegacyChanges: () async {
          calls++;
          return false;
        }),
        () {});
    await tester.tap(find.text(keep));
    await tester.pumpAndSettle();
    expect(find.textContaining('没有保存'), findsOneWidget);
    expect(find.text('退出'), findsNothing);
    await tester.tap(find.text(keep));
    await tester.pumpAndSettle();
    expect(calls, 2);
  });
  testWidgets('other bootstrap errors show only their text', (tester) async {
    await _mount(
        tester, const IdentityBootstrapException('please quit'), () {});
    expect(find.text('please quit'), findsOneWidget);
    expect(find.text(keep), findsNothing);
  });
}
