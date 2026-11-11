import 'dart:async';
import 'dart:convert';
import 'dart:io';

import 'package:path/path.dart' as path;

enum DirectoryCheckState {
  empty,
  relative,
  missing,
  notDirectory,
  denied,
  cleanupFailed,
  writable
}

class DirectoryCheck {
  final String directory;
  final DirectoryCheckState state;
  final String? remainingProbe;
  const DirectoryCheck(this.directory, this.state, {this.remainingProbe});

  bool get writable => state == DirectoryCheckState.writable;
  String get message => switch (state) {
        DirectoryCheckState.empty => '请选择下载保存位置。',
        DirectoryCheckState.relative => '请输入完整的绝对路径，或使用“选择…”选择文件夹。',
        DirectoryCheckState.missing => '文件夹不存在，请选择已有文件夹。',
        DirectoryCheckState.notDirectory => '此路径不是文件夹，请重新选择。',
        DirectoryCheckState.denied =>
          '无法写入此文件夹。请检查权限，或选择其他保存位置；如系统询问访问权限，请按需允许。',
        DirectoryCheckState.cleanupFailed =>
          '探测文件未能清理，请检查并删除此检查专用路径：$remainingProbe',
        DirectoryCheckState.writable => '已在此文件夹完成临时文件写入并清理。此结果不代表后续下载或磁盘空间检查。',
      };
}

enum MediaTool { ytDlp, ffmpeg, node }

extension MediaToolVersion on MediaTool {
  String get label => switch (this) {
        MediaTool.ytDlp => 'yt-dlp',
        MediaTool.ffmpeg => 'ffmpeg',
        MediaTool.node => 'Node'
      };
  List<String> get versionArguments =>
      this == MediaTool.ffmpeg ? const ['-version'] : const ['--version'];
}

enum ToolCheckState { unknown, succeeded, failed, timedOut, outputLimit }

class ToolCheck {
  final ToolCheckState state;
  final String message;
  const ToolCheck(this.state, this.message);
}

typedef VersionProcessStarter = Future<Process> Function(
    String executable, List<String> arguments);

/// Explicit, local checks only. No PATH/default-tool resolution or core requests.
class OnboardingChecks {
  final Duration timeout;
  final int maxOutputBytes;
  final VersionProcessStarter _start;
  final Future<Directory> Function(Directory) _createProbe;

  OnboardingChecks({
    this.timeout = const Duration(seconds: 3),
    this.maxOutputBytes = 4096,
    VersionProcessStarter? start,
    Future<Directory> Function(Directory)? createProbe,
  })  : _start = start ??
            ((executable, arguments) =>
                Process.start(executable, arguments, runInShell: false)),
        _createProbe = createProbe ??
            ((directory) => directory.createTemp('.kownload-write-check-'));

  /// Call only after the user explicitly confirms this directory for a write check.
  Future<DirectoryCheck> checkDirectory(String input) async {
    final directory = input.trim();
    if (directory.isEmpty) {
      return DirectoryCheck(directory, DirectoryCheckState.empty);
    }
    if (!path.isAbsolute(directory)) {
      return DirectoryCheck(directory, DirectoryCheckState.relative);
    }
    Directory? probe;
    File? file;
    var result = DirectoryCheck(directory, DirectoryCheckState.denied);
    try {
      final type = await FileSystemEntity.type(directory);
      if (type == FileSystemEntityType.notFound) {
        return DirectoryCheck(directory, DirectoryCheckState.missing);
      }
      if (type != FileSystemEntityType.directory) {
        return DirectoryCheck(directory, DirectoryCheckState.notDirectory);
      }
      // createTemp atomically reserves a unique child. Never overwrite or
      // recursively remove any user file, even when a check fails.
      probe = await _createProbe(Directory(directory));
      file = File(path.join(probe.path, 'probe'));
      await file.writeAsBytes(const [75, 111, 119, 110], flush: true);
      result = DirectoryCheck(directory, DirectoryCheckState.writable);
    } on FileSystemException {
      result = DirectoryCheck(directory, DirectoryCheckState.denied);
    } finally {
      if (probe != null) {
        try {
          if (file != null && await file.exists()) await file.delete();
          await probe.delete();
        } on FileSystemException {
          result = DirectoryCheck(directory, DirectoryCheckState.cleanupFailed,
              remainingProbe: probe.path);
        }
      }
    }
    return result;
  }

  /// This checks the specified executable only, not the core's effective tool.
  Future<ToolCheck> checkTool(MediaTool tool, String executable) async {
    if (executable.isEmpty || !path.isAbsolute(executable)) {
      return const ToolCheck(
          ToolCheckState.unknown, '未检测：需要明确的绝对可执行路径；默认或相对路径须由内核确认。');
    }
    Process? process;
    final subscriptions = <StreamSubscription<List<int>>>[];
    Timer? timer;
    final completion = Completer<ToolCheck>();
    var expired = false;
    void finish(ToolCheck result) {
      if (!completion.isCompleted) completion.complete(result);
    }

    void stop() => process?.kill(ProcessSignal.sigkill);
    timer = Timer(timeout, () {
      expired = true;
      stop();
      finish(
          const ToolCheck(ToolCheckState.timedOut, '版本检测超时，已停止检测；可检查路径后重试。'));
    });
    final bytes = <int>[];
    final drained = <Future<void>>[];
    try {
      // If launch completes after the deadline, stop that process as well.
      final launch = _start(executable, tool.versionArguments).then((value) {
        process = value;
        if (expired) stop();
        return value;
      });
      final started = await Future.any<Object>([launch, completion.future]);
      if (started is! Process) return started as ToolCheck;
      for (final stream in [started.stdout, started.stderr]) {
        final done = Completer<void>();
        drained.add(done.future);
        subscriptions.add(stream.listen(
            (chunk) {
              if (completion.isCompleted) return;
              if (bytes.length + chunk.length > maxOutputBytes) {
                stop();
                finish(const ToolCheck(
                    ToolCheckState.outputLimit, '版本输出超出限制，已停止检测。'));
              } else {
                bytes.addAll(chunk);
              }
            },
            onDone: done.complete,
            onError: (Object _) {
              stop();
              finish(
                  const ToolCheck(ToolCheckState.failed, '无法读取版本结果，请检查此工具。'));
            }));
      }
      unawaited(() async {
        try {
          final code = await started.exitCode;
          await Future.wait(drained);
          final text = utf8
              .decode(bytes, allowMalformed: true)
              .replaceAll(RegExp(r'[\x00-\x08\x0B-\x1F\x7F]'), '')
              .trim();
          final firstLine = text.split('\n').first;
          finish(code == 0 && firstLine.isNotEmpty
              ? ToolCheck(ToolCheckState.succeeded,
                  '此文件返回版本：${firstLine.length > 180 ? firstLine.substring(0, 180) : firstLine}（未确认内核使用此文件）')
              : const ToolCheck(
                  ToolCheckState.failed, '未取得版本，请检查文件、执行权限及工具是否可用。'));
        } catch (_) {
          finish(const ToolCheck(ToolCheckState.failed, '版本检测失败，请检查此工具。'));
        }
      }());
      return await completion.future;
    } on ProcessException {
      return const ToolCheck(ToolCheckState.failed, '无法执行此文件，请检查路径和执行权限。');
    } finally {
      timer.cancel();
      for (final subscription in subscriptions) {
        await subscription.cancel();
      }
    }
  }
}
