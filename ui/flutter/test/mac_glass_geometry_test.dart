import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/macos/mac_appearance.dart';
import 'package:gopeed/macos/mac_glass.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  const channel = MethodChannel('gopeed/macos_appearance');

  testWidgets('native glass follows a moved cached child without rebuilding it',
      (tester) async {
    final updates = <List<dynamic>>[];
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
      if (call.method == 'glassRegions')
        updates.add(call.arguments as List<dynamic>);
      return null;
    });
    MacAppearance.state.value = const MacAppearanceState(supportsGlass: true);
    final offset = ValueNotifier(Offset.zero);
    addTearDown(() {
      offset.dispose();
      MacAppearance.state.value = const MacAppearanceState();
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, null);
    });
    await tester.pumpWidget(MaterialApp(
      home: Align(
        alignment: Alignment.topLeft,
        child: ValueListenableBuilder<Offset>(
          valueListenable: offset,
          child: const RepaintBoundary(
            child: MacGlassSurface(child: SizedBox(width: 120, height: 38)),
          ),
          builder: (_, value, child) =>
              Transform.translate(offset: value, child: child),
        ),
      ),
    ));
    await tester.pump();
    final before = updates.last.single as Map;
    offset.value = const Offset(208, 44);
    await tester.pump();
    await tester.pump();
    final after = updates.last.single as Map;
    expect(after['x'], before['x'] + 208);
    expect(after['y'], before['y'] + 44);
    expect(after['width'], before['width']);
    expect(after['height'], before['height']);
  });

  testWidgets('native glass is clipped by its scrolling viewport',
      (tester) async {
    final updates = <List<dynamic>>[];
    TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
        .setMockMethodCallHandler(channel, (call) async {
      if (call.method == 'glassRegions')
        updates.add(call.arguments as List<dynamic>);
      return null;
    });
    MacAppearance.state.value = const MacAppearanceState(supportsGlass: true);
    final scroll = ScrollController();
    addTearDown(() {
      scroll.dispose();
      MacAppearance.state.value = const MacAppearanceState();
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger
          .setMockMethodCallHandler(channel, null);
    });
    await tester.pumpWidget(MaterialApp(
      home: Padding(
        padding: const EdgeInsets.only(top: 100),
        child: ListView(controller: scroll, children: const [
          MacGlassSurface(child: SizedBox(height: 38)),
          SizedBox(height: 1200),
        ]),
      ),
    ));
    await tester.pump();
    final height = (updates.last.single as Map)['height'] as num;
    scroll.jumpTo(20);
    await tester.pump();
    await tester.pump();
    final partial = updates.last.single as Map;
    expect(partial['y'], 80);
    expect(partial['clipY'], 100);
    expect(partial['clipHeight'], closeTo(height - 20, .01));
    scroll.jumpTo(120);
    await tester.pump();
    await tester.pump();
    expect(updates.last, isEmpty);
  });
}
