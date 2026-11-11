import 'package:flutter/material.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/macos/onboarding_checks.dart';

class FixtureOnboardingChecks extends OnboardingChecks {
  final directories = <String>[];
  final tools = <(MediaTool, String)>[];
  ToolCheck toolResult =
      const ToolCheck(ToolCheckState.succeeded, 'fixture version only');
  DirectoryCheckState directoryState = DirectoryCheckState.writable;
  Future<DirectoryCheck> Function(String)? directoryResult;

  @override
  Future<DirectoryCheck> checkDirectory(String input) async {
    final directory = input.trim();
    directories.add(directory);
    if (directoryResult != null) return directoryResult!(directory);
    return DirectoryCheck(directory,
        directory.isEmpty ? DirectoryCheckState.empty : directoryState);
  }

  @override
  Future<ToolCheck> checkTool(MediaTool tool, String executable) async {
    tools.add((tool, executable));
    return toolResult;
  }
}

Future<void> checkOnboardingDirectory(WidgetTester tester,
    {bool settle = true}) async {
  final button = find.byKey(const ValueKey('onboarding-check-directory'));
  await tester.ensureVisible(button);
  if (settle) {
    await tester.pumpAndSettle();
  } else {
    await tester.pump(const Duration(milliseconds: 300));
  }
  await tester.tap(button);
  if (settle) {
    await tester.pumpAndSettle();
  } else {
    await tester.pump(const Duration(milliseconds: 300));
  }
  expect(find.byKey(const ValueKey('onboarding-directory-result')),
      findsOneWidget);
}
