import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/modules/setting/views/setting_view.dart';
import 'package:gopeed/macos/mac_panel.dart';

void main() {
  test(
      'Carbon chords use physical letters, punctuation, function and keypad codes',
      () {
    final chord = MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.keyD,
        control: true, option: true);
    expect(chord!.keyCode, 2);
    expect(chord.modifiers, 6144);
    expect(chord.label, '⌃⌥D');
    expect(
        MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.keyQ,
                command: true, shift: true)!
            .keyCode,
        12);
    expect(
        MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.comma,
                command: true)!
            .keyCode,
        43);
    expect(
        MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.f12, option: true)!
            .keyCode,
        111);
    expect(
        MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.numpadEnter,
                control: true)!
            .keyCode,
        76);
    expect(
        MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.controlLeft,
            control: true),
        isNull);
    expect(MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.keyD), isNull);
    expect(
        MacHotKeyChord.fromPhysicalKey(PhysicalKeyboardKey.mediaPlayPause,
            option: true),
        isNull);
  });

  testWidgets(
      'recorder validates modifiers, registers physical key and shows conflicts',
      (tester) async {
    final calls = <MethodCall>[];
    var succeeds = false;
    tester.binding.defaultBinaryMessenger.setMockMethodCallHandler(
      MacPanel.channel,
      (call) async {
        calls.add(call);
        return call.method == 'registerHotKey'
            ? {'registered': succeeds}
            : null;
      },
    );
    addTearDown(() {
      tester.binding.defaultBinaryMessenger
          .setMockMethodCallHandler(MacPanel.channel, null);
      MacPanel.hotKeyLabel.value = '⌃⌥D';
      MacPanel.hotKeyError.value = null;
    });
    MacPanel.hotKeyLabel.value = '⌃⌥D';
    await tester
        .pumpWidget(const MaterialApp(home: Scaffold(body: MacHotKeyEditor())));
    await tester.tap(find.text('⌃⌥D'));
    await tester.pumpAndSettle();
    await tester.sendKeyEvent(LogicalKeyboardKey.keyQ);
    await tester.pump();
    expect(find.text('请按至少一个修饰键与受支持的实体按键。'), findsOneWidget);
    expect(
        tester
            .widget<TextButton>(find.widgetWithText(TextButton, '保存'))
            .onPressed,
        isNull);
    await tester.sendKeyDownEvent(LogicalKeyboardKey.controlLeft);
    await tester.sendKeyDownEvent(LogicalKeyboardKey.altLeft);
    // A remapped logical character still records the physical D key.
    await tester.sendKeyEvent(LogicalKeyboardKey.keyX,
        physicalKey: PhysicalKeyboardKey.keyD);
    await tester.sendKeyUpEvent(LogicalKeyboardKey.altLeft);
    await tester.sendKeyUpEvent(LogicalKeyboardKey.controlLeft);
    await tester.pump();
    await tester.tap(find.text('保存'));
    await tester.pumpAndSettle();
    final register = calls.firstWhere((c) => c.method == 'registerHotKey');
    expect(register.arguments, {'keyCode': 2, 'modifiers': 6144});
    expect(find.byType(AlertDialog), findsOneWidget);
    expect(find.text('快捷键被占用或无法注册，请更换组合。'), findsWidgets);
    succeeds = true;
    await tester.tap(find.text('保存'));
    await tester.pumpAndSettle();
    expect(find.byType(AlertDialog), findsNothing);
    expect(MacPanel.hotKeyError.value, isNull);
    expect(MacPanel.hotKeyLabel.value, '⌃⌥D');
    expect(tester.takeException(), isNull);
  });

  testWidgets('Escape cancels recording without registering a shortcut',
      (tester) async {
    var registrations = 0;
    tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, (call) async {
      if (call.method == 'registerHotKey') registrations++;
      return null;
    });
    addTearDown(() => tester.binding.defaultBinaryMessenger
        .setMockMethodCallHandler(MacPanel.channel, null));
    await tester
        .pumpWidget(const MaterialApp(home: Scaffold(body: MacHotKeyEditor())));
    await tester.tap(find.text(MacPanel.hotKeyLabel.value));
    await tester.pumpAndSettle();
    await tester.sendKeyEvent(LogicalKeyboardKey.escape);
    await tester.pumpAndSettle();
    expect(find.byType(AlertDialog), findsNothing);
    expect(registrations, 0);
  });
}
