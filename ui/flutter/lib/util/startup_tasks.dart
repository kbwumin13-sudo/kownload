import '../api/model/task.dart';

/// Browser credentials are deliberately memory-only. Cloud tasks must wait for
/// the user to reauthorize their source after a restart.
Future<void> resumeStartupTasks({
  required Future<List<Task>> Function() load,
  required Future<void> Function(List<String>) resume,
}) async {
  final tasks = await load();
  final ids = tasks
      .where((task) =>
          task.protocol != Protocol.netdisk &&
          task.meta.req.protocol != 'netdisk' &&
          task.status != Status.done &&
          task.status != Status.running)
      .map((task) => task.id)
      .toList();
  // The server treats an empty ID filter as "all tasks".
  if (ids.isNotEmpty) await resume(ids);
}
