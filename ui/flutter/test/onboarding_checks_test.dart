import 'dart:async';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/macos/onboarding_checks.dart';

class _VersionProcess extends Fake implements Process {
  final output = StreamController<List<int>>();
  final errors = StreamController<List<int>>();
  final exit = Completer<int>();
  int kills = 0;
  _VersionProcess({List<int> bytes = const [], int? code = 0}) {
    if (code != null) {
      scheduleMicrotask(() async {
        output.add(bytes);
        await output.close();
        await errors.close();
        if (!exit.isCompleted) exit.complete(code);
      });
    }
  }
  @override
  Stream<List<int>> get stdout => output.stream;
  @override
  Stream<List<int>> get stderr => errors.stream;
  @override
  Future<int> get exitCode => exit.future;
  @override
  bool kill([ProcessSignal signal = ProcessSignal.sigterm]) {
    kills++;
    if (!exit.isCompleted) exit.complete(-9);
    unawaited(output.close());
    unawaited(errors.close());
    return true;
  }
}

void main() {
  late Directory root;
  setUp(() async {
    root = await Directory('/tmp').createTemp('kownload-onboarding-');
  });
  tearDown(() async => root.delete(recursive: true));

  test('directory rejects empty, relative, missing and non-directory paths',
      () async {
    final checks = OnboardingChecks();
    expect((await checks.checkDirectory(' ')).state, DirectoryCheckState.empty);
    expect((await checks.checkDirectory('relative')).state,
        DirectoryCheckState.relative);
    expect((await checks.checkDirectory('${root.path}/absent')).state,
        DirectoryCheckState.missing);
    final file = await File('${root.path}/user-file').writeAsString('keep');
    expect((await checks.checkDirectory(file.path)).state,
        DirectoryCheckState.notDirectory);
    expect(await file.readAsString(), 'keep');
    expect(await Directory('${root.path}/absent').exists(), isFalse);
  });

  test(
      'directory writes only its unique probe and leaves existing files unchanged',
      () async {
    final user = await File('${root.path}/probe').writeAsString('user bytes');
    final previous = await root.list().map((value) => value.path).toList();
    final result = await OnboardingChecks().checkDirectory('  ${root.path}  ');
    expect(result.writable, isTrue);
    expect(result.directory, root.path);
    expect(await user.readAsString(), 'user bytes');
    expect(await root.list().map((value) => value.path).toList(), previous);
  });

  test(
      'directory permission failure is explicit and creates no fallback folder',
      () async {
    final checks = OnboardingChecks(createProbe: (directory) async {
      throw FileSystemException('fixture permission denied', directory.path,
          const OSError('denied', 13));
    });
    final result = await checks.checkDirectory(root.path);
    expect(result.state, DirectoryCheckState.denied);
    expect(result.writable, isFalse);
    expect(await root.list().isEmpty, isTrue);
  });

  test(
      'failed cleanup is not writable success and never recursively deletes another file',
      () async {
    late Directory probe;
    final checks = OnboardingChecks(createProbe: (directory) async {
      probe = await directory.createTemp('.kownload-write-check-');
      await File('${probe.path}/foreign').writeAsString('keep this file');
      return probe;
    });
    final result = await checks.checkDirectory(root.path);
    expect(result.state, DirectoryCheckState.cleanupFailed);
    expect(result.remainingProbe, probe.path);
    expect(result.writable, isFalse);
    expect(
        await File('${probe.path}/foreign').readAsString(), 'keep this file');
    expect(await File('${probe.path}/probe').exists(), isFalse);
  });

  test(
      'tool defaults and relative configuration remain unknown without launching',
      () async {
    var launches = 0;
    final checks = OnboardingChecks(start: (_, __) async {
      launches++;
      return _VersionProcess();
    });
    for (final value in ['', 'yt-dlp', '~/bin/node', './ffmpeg']) {
      expect((await checks.checkTool(MediaTool.ytDlp, value)).state,
          ToolCheckState.unknown);
    }
    expect(launches, 0);
  });

  test('tool uses exact executable and fixed argv for each optional tool',
      () async {
    final invocations = <(String, List<String>)>[];
    final checks = OnboardingChecks(start: (executable, arguments) async {
      invocations.add((executable, arguments));
      return _VersionProcess(bytes: 'v1 fixture\n'.codeUnits);
    });
    final executable = '${root.path}/tool with spaces; dollar\$value';
    for (final tool in MediaTool.values) {
      expect((await checks.checkTool(tool, executable)).state,
          ToolCheckState.succeeded);
    }
    expect(invocations.map((value) => value.$1),
        [executable, executable, executable]);
    expect(invocations.map((value) => value.$2).toList(), [
      ['--version'],
      ['-version'],
      ['--version']
    ]);
  });

  test('real exact path with shell characters is executed without a shell',
      () async {
    final executable = '${root.path}/echo with spaces; touch sentinel';
    await Link(executable).create('/bin/echo');
    final result =
        await OnboardingChecks().checkTool(MediaTool.node, executable);
    expect(result.state, ToolCheckState.succeeded);
    expect(result.message, contains('--version'));
    expect(await File('${root.path}/sentinel').exists(), isFalse);
    expect(await root.list().length, 1);
  });

  test('missing executable, permission errors and nonzero version are failures',
      () async {
    expect(
        (await OnboardingChecks()
                .checkTool(MediaTool.node, '${root.path}/absent'))
            .state,
        ToolCheckState.failed);
    final denied = OnboardingChecks(
        start: (executable, args) async =>
            throw ProcessException(executable, args, 'fixture denied', 13));
    expect((await denied.checkTool(MediaTool.node, '/fixture/node')).state,
        ToolCheckState.failed);
    final failed = OnboardingChecks(
        start: (_, __) async =>
            _VersionProcess(bytes: 'bad'.codeUnits, code: 7));
    expect((await failed.checkTool(MediaTool.node, '/fixture/node')).state,
        ToolCheckState.failed);
  });

  test('version timeout kills process and returns without an unbounded wait',
      () async {
    final process = _VersionProcess(code: null);
    final checks = OnboardingChecks(
        timeout: const Duration(milliseconds: 20),
        start: (_, __) async => process);
    expect((await checks.checkTool(MediaTool.node, '/fixture/node')).state,
        ToolCheckState.timedOut);
    expect(process.kills, 1);
  });

  test('a process launched after deadline is also stopped', () async {
    final pending = Completer<Process>();
    final process = _VersionProcess(code: null);
    final checks = OnboardingChecks(
        timeout: const Duration(milliseconds: 20),
        start: (_, __) => pending.future);
    expect((await checks.checkTool(MediaTool.node, '/fixture/node')).state,
        ToolCheckState.timedOut);
    pending.complete(process);
    await Future<void>.delayed(Duration.zero);
    expect(process.kills, 1);
  });

  test('stdout and stderr share the bounded output budget', () async {
    final process = _VersionProcess(code: null);
    final checks = OnboardingChecks(
        maxOutputBytes: 8,
        start: (_, __) async {
          scheduleMicrotask(() {
            process.output.add([1, 2, 3, 4]);
            process.errors.add([5, 6, 7, 8, 9]);
          });
          return process;
        });
    expect((await checks.checkTool(MediaTool.ffmpeg, '/fixture/ffmpeg')).state,
        ToolCheckState.outputLimit);
    expect(process.kills, 1);
  });
}
