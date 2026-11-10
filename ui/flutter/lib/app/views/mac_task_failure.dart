import 'package:flutter/material.dart';
import '../../api/api.dart' show getTaskStats;
import '../../api/model/task_stats.dart';

/// Keep credentials and transient download URLs out of the compact label.
String displayTaskFailure(String value) => value
    .replaceAll(RegExp(r'https?://\S+'), '[下载地址]')
    .replaceAll(RegExp(r'Bearer\s+\S+', caseSensitive: false), 'Bearer …')
    .replaceAll(RegExp(r'\s+'), ' ')
    .trim();

/// Existing diagnostics, loaded once for this mounted failed row.
/// A missing or unavailable reason never changes the task's state or retry.
class MacTaskFailure extends StatefulWidget {
  final String taskId;
  final String fallback;
  final String suffix;
  final TextStyle style;
  final Future<TaskStats> Function(String) read;
  const MacTaskFailure(
      {super.key,
      required this.taskId,
      required this.style,
      this.fallback = '下载失败',
      this.suffix = '',
      this.read = getTaskStats});

  @override
  State<MacTaskFailure> createState() => _MacTaskFailureState();
}

class _MacTaskFailureState extends State<MacTaskFailure> {
  late Future<TaskStats> _stats;
  @override
  void initState() {
    super.initState();
    _stats = Future.sync(() => widget.read(widget.taskId));
  }

  @override
  void didUpdateWidget(MacTaskFailure oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.taskId != widget.taskId)
      _stats = Future.sync(() => widget.read(widget.taskId));
  }

  @override
  Widget build(BuildContext context) => FutureBuilder<TaskStats>(
      key: ValueKey(widget.taskId),
      future: _stats,
      builder: (_, result) {
        final reason = displayTaskFailure(result.data?.lastError ?? '');
        final text = [
          widget.fallback,
          if (reason.isNotEmpty) reason,
          if (widget.suffix.isNotEmpty) widget.suffix
        ].join(' · ');
        return Tooltip(
            message: text,
            child: Text(text,
                maxLines: 1,
                overflow: TextOverflow.ellipsis,
                style: widget.style));
      });
}
