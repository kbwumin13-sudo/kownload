import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/macos/mac_panel.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  final calls = <MethodCall>[];
  setUp(() {
    calls.clear();
    MacPanel.cancelAutoHide();
    MacPanel.presentation.value = 'panel';
    MacPanel.visible.value = false;
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      calls.add(call);
      return call.method == 'registerHotKey'
          ? {'registered': true, 'status': 0}
          : null;
    });
  });
  tearDown(() {
    MacPanel.cancelAutoHide();
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null);
  });
  testWidgets('submission waits for confirmation and new input cancels hiding',
      (tester) async {
    MacPanel.visible.value = true;
    MacPanel.submitted();
    expect(MacPanel.notice.value, '已加入下载');
    await tester.pump(const Duration(milliseconds: 600));
    expect(MacPanel.visible.value, isTrue);
    MacPanel.cancelAutoHide();
    await tester.pump(const Duration(seconds: 1));
    expect(calls.where((c) => c.method == 'hide'), isEmpty);
    MacPanel.submitted();
    await tester.pump(const Duration(milliseconds: 800));
    await tester.pump();
    expect(MacPanel.visible.value, isFalse);
    expect(calls.last.method, 'hide');
  });
  testWidgets('browser result stays open and visible updates do not flash',
      (tester) async {
    MacPanel.submitted();
    await MacPanel.showBrowserResult(['a', 'b'], null);
    expect(MacPanel.browserResult.value!.taskIds, ['a', 'b']);
    await tester.pump(const Duration(seconds: 3));
    expect(MacPanel.visible.value, isTrue);
    expect(calls.where((c) => c.method == 'hide'), isEmpty);
    await MacPanel.showBrowserResult([], '失败原因');
    expect(MacPanel.browserResult.value!.error, '失败原因');
    expect(calls.where((c) => c.method == 'setPresentation'), hasLength(1));
    await MacPanel.hide();
    expect(MacPanel.browserResult.value, isNull);
  });
  test('conflicting shortcut preserves the working label', () async {
    MacPanel.hotKeyLabel.value = '⌃⌥D';
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel,
            (_) async => {'registered': false, 'status': -9878});
    expect(
        await MacPanel.changeHotKey(keyCode: 49, modifiers: 2048, label: '⌥空格'),
        isFalse);
    expect(MacPanel.hotKeyLabel.value, '⌃⌥D');
    expect(MacPanel.hotKeyError.value, isNotNull);
  });
  test('panel toggle hides without destroying presentation state', () async {
    await MacPanel.showPanel();
    await MacPanel.toggle();
    expect(MacPanel.visible.value, isFalse);
    expect(MacPanel.presentation.value, 'panel');
    expect(calls.map((c) => c.method),
        containsAllInOrder(['setPresentation', 'hide']));
  });
  test('nested native pickers release protection after failure', () async {
    await expectLater(MacPanel.transient(() async {
      await MacPanel.transient(() async {});
      throw StateError('cancelled');
    }), throwsStateError);
    expect(
        calls
            .where((c) => c.method == 'setTransient')
            .map((c) => c.arguments['active']),
        [true, true, true, false]);
  });
}
