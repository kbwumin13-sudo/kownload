import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/modules/create/views/http_checksum_input.dart';

void main() {
  test('empty input remains an ordinary download', () {
    for (final raw in ['', '  ', '\n\t']) {
      expect(normalizedExpectedSha256(raw), isNull);
    }
  });
  test('full ASCII SHA256 accepts surrounding whitespace and normalizes case',
      () {
    final value = 'aB09' * 16;
    expect(normalizedExpectedSha256(' \t$value\n'), value.toLowerCase());
  });
  for (final (index, value) in [
    'a' * 63,
    'a' * 65,
    'g' * 64,
    'sha256:${'a' * 64}',
    '${'a' * 32} ${'a' * 32}',
    '${'a' * 32}\n${'a' * 32}',
    'ａ' * 64,
    '${'a' * 32}\u200b${'a' * 32}',
  ].indexed) {
    test('invalid digest case $index is rejected without echoing the input',
        () {
      try {
        normalizedExpectedSha256(value);
        fail('digest accepted');
      } on FormatException catch (error) {
        expect(error.message, '请输入完整的 64 位十六进制 SHA-256。');
        expect(error.message, isNot(contains(value)));
      }
    });
  }
  testWidgets(
      'optional field validates the entire input and allows explicit clearing',
      (tester) async {
    final controller = TextEditingController();
    addTearDown(controller.dispose);
    final edits = <String>[];
    await tester.pumpWidget(MaterialApp(
        home: Scaffold(
            body: HttpChecksumInput(
      controller: controller,
      onChanged: edits.add,
    ))));
    final field = find.byKey(const ValueKey('http-expected-sha256'));
    await tester.enterText(field, 'a' * 65);
    await tester.pump();
    expect(controller.text, hasLength(65));
    expect(find.text('请输入完整的 64 位十六进制 SHA-256。'), findsOneWidget);
    await tester.enterText(field, 'A' * 64);
    await tester.pump();
    expect(find.text('请输入完整的 64 位十六进制 SHA-256。'), findsNothing);
    expect(edits.last, 'A' * 64);
    await tester.enterText(field, '');
    await tester.pump();
    expect(normalizedExpectedSha256(controller.text), isNull);
    expect(edits.last, '');
  });
}
