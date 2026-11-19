import 'package:flutter/material.dart';
import 'package:get/get.dart';
import 'package:url_launcher/url_launcher.dart';

import '../../api/api.dart';
import '../../api/http_recovery_api.dart';
import '../../api/model/request.dart';
import '../../api/model/resolve_task.dart';
import '../../api/model/task.dart';
import '../../api/model/task_stats.dart';
import '../../api/netdisk.dart';
import '../../macos/mac_panel.dart';
import '../../util/util.dart';
import '../modules/app/controllers/app_controller.dart';
import '../modules/task/controllers/task_recovery_feedback.dart';
import '../modules/task/views/http_task_retry_button.dart';
import '../routes/app_pages.dart';

enum TaskFailure {
  unknown('taskFailureUnknown'),
  expired('taskFailureExpired'),
  access('taskFailureAccess'),
  invalidLink('taskFailureInvalidLink'),
  cooldown('taskFailureCooldown'),
  unsafeResume('taskFailureUnsafeResume'),
  disk('taskFailureDisk'),
  network('taskFailureNetwork');

  final String _key;
  const TaskFailure(this._key);
  String get label => _key.tr;
}

/// Diagnostics can contain request URLs, headers and provider messages. Only
/// fixed categories cross into the task row or recovery dialog.
TaskFailure taskFailure(TaskStats? stats, {bool afterRecovery = false}) {
  final reason = stats?.lastError?.toLowerCase() ?? '';
  if (stats?.cooldownUntil?.isAfter(DateTime.now()) == true ||
      stats?.coolingDown == true ||
      _statusIn(reason, '429')) {
    return TaskFailure.cooldown;
  }
  if (reason.contains('cannot verify http resource identity') ||
      reason.contains('partial file preserved')) {
    return TaskFailure.unsafeResume;
  }
  if (reason.contains('no space left') ||
      reason.contains('disk full') ||
      reason.contains('read-only file system') ||
      reason.contains('permission denied') &&
          (reason.contains('write download file') ||
              reason.contains('mkdir') ||
              reason.startsWith('open '))) {
    return TaskFailure.disk;
  }
  if (reason.contains('expired') ||
      reason.contains('过期') ||
      _statusIn(reason, '410')) {
    return afterRecovery ? TaskFailure.invalidLink : TaskFailure.expired;
  }
  if (_statusIn(reason, '401|403') ||
      reason.contains('login_required') ||
      reason.contains('请在浏览器登录')) {
    return afterRecovery ? TaskFailure.invalidLink : TaskFailure.access;
  }
  if (_statusIn(reason, '404')) return TaskFailure.invalidLink;
  if (reason.contains('timeout') ||
      reason.contains('connection reset') ||
      reason.contains('unexpected eof') ||
      reason.contains('no such host')) {
    return TaskFailure.network;
  }
  return TaskFailure.unknown;
}

bool _statusIn(String reason, String codes) =>
    RegExp('(?:status[=:]?|code:|http(?: error)?)\\s*(?:$codes)\\b')
        .hasMatch(reason);

Uri? _webAddress(String value) {
  final uri = Uri.tryParse(value);
  return uri != null &&
          const {'http', 'https'}.contains(uri.scheme) &&
          uri.host.isNotEmpty &&
          uri.userInfo.isEmpty
      ? uri
      : null;
}

Uri? _taskSourceSite(Task task) {
  final req = task.meta.req;
  final extra = req.extra;
  if (task.protocol == Protocol.media && extra is Map) {
    final page = extra['sourcePage'];
    final address = page is String ? _webAddress(page) : null;
    if (address != null) return address;
  }
  return _webAddress(req.url);
}

bool canReplaceTaskLink(Task task, TaskStats? stats) {
  if (task.protocol != Protocol.http ||
      task.handoff?.blocked == true ||
      task.sourceFrozen ||
      task.completionView != null ||
      task.status != Status.error && task.status != Status.pause ||
      _webAddress(task.meta.req.url) == null) {
    return false;
  }
  final extra = task.meta.req.extra;
  if (extra != null && extra is! Map) return false;
  if (extra is Map) {
    final method = extra['method'];
    final body = extra['body'];
    final headers = extra['header'];
    if (method != null &&
            method != '' &&
            (method is! String || method.toUpperCase() != 'GET') ||
        body != null && body != '' ||
        headers is Map &&
            headers.keys
                .any((key) => key.toString().toLowerCase() == 'range')) {
      return false;
    }
  }
  return task.progress.downloaded == 0 ||
      task.meta.res?.range == true &&
          task.meta.res!.size > 0 &&
          !const {'unsupported', 'probeFailed'}.contains(stats?.rangeState);
}

bool canSyncTaskLogin(Task task) {
  if (task.protocol != Protocol.netdisk ||
      task.meta.req.protocol != 'netdisk' ||
      task.handoff?.blocked == true ||
      task.status != Status.error && task.status != Status.pause) {
    return false;
  }
  try {
    NetdiskInput.fromRequest(task.meta.req);
    return true;
  } catch (_) {
    return false;
  }
}

void openTaskLogin(Task task) {
  final controller = Get.find<AppController>();
  controller.pendingUpdateTask.value =
      PendingUpdateTask(id: task.id, name: task.name);
  if (Util.isMacos()) {
    MacPanel.showPanel();
  } else {
    Get.rootDelegate.toNamed(Routes.CREATE);
  }
}

String _nextStep(Task task, TaskFailure failure, TaskStats? stats) {
  if (task.handoff?.blocked == true) return 'taskRecoveryCheckHandoff'.tr;
  if (failure == TaskFailure.cooldown) return 'httpRecoveryCooldown'.tr;
  if (failure == TaskFailure.disk) return 'taskRecoveryCheckDisk'.tr;
  if (failure != TaskFailure.network && canSyncTaskLogin(task)) {
    return 'taskRecoverySyncDriveHint'.tr;
  }
  if (task.protocol == Protocol.media && _taskSourceSite(task) != null) {
    return 'taskRecoveryMediaHint'.tr;
  }
  if (canReplaceTaskLink(task, stats) &&
      const {
        TaskFailure.expired,
        TaskFailure.access,
        TaskFailure.invalidLink,
        TaskFailure.unsafeResume
      }.contains(failure)) {
    return 'taskRecoveryReplaceHint'.tr;
  }
  if (task.protocol == Protocol.http &&
      (failure == TaskFailure.expired ||
          failure == TaskFailure.access ||
          failure == TaskFailure.invalidLink ||
          failure == TaskFailure.unsafeResume)) {
    return 'taskRecoveryLinkUnavailable'.tr;
  }
  return 'taskRecoveryRetryHint'.tr;
}

class MacTaskFailure extends StatefulWidget {
  final Task task;
  final RecoveryStage? stage;
  final bool afterLinkUpdate;
  final String suffix;
  final TextStyle style;
  final Future<TaskStats> Function(String) read;

  const MacTaskFailure(
      {super.key,
      required this.task,
      required this.style,
      this.stage,
      this.afterLinkUpdate = false,
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
    _read();
  }

  void _read() => _stats = Future.sync(() => widget.read(widget.task.id));

  @override
  void didUpdateWidget(MacTaskFailure oldWidget) {
    super.didUpdateWidget(oldWidget);
    if (oldWidget.task.id != widget.task.id ||
        oldWidget.task.updatedAt != widget.task.updatedAt) {
      _read();
    }
  }

  @override
  Widget build(BuildContext context) => FutureBuilder<TaskStats>(
      key: ValueKey((widget.task.id, widget.task.updatedAt)),
      future: _stats,
      builder: (_, result) {
        final failure = taskFailure(result.data,
            afterRecovery:
                widget.afterLinkUpdate && widget.stage == RecoveryStage.failed);
        final text = [
          if (widget.stage != null) widget.stage!.label,
          failure.label,
          _nextStep(widget.task, failure, result.data),
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

/// Shared by the two macOS task rows. The recovery sheet uses the current Task
/// and only the existing scoped read, PATCH and Continue operations.
Future<void> showTaskRecovery(
  BuildContext context,
  Task task,
  TaskRecoveryFeedback feedback, {
  required Future<void> Function() refresh,
  required Task? Function() currentTask,
  bool allowLinkUpdate = true,
}) =>
    showDialog<void>(
      context: context,
      barrierDismissible: false,
      builder: (_) => _TaskRecoveryDialog(
          task, feedback, refresh, currentTask, allowLinkUpdate),
    );

class _TaskRecoveryDialog extends StatefulWidget {
  final Task task;
  final TaskRecoveryFeedback feedback;
  final Future<void> Function() refresh;
  final Task? Function() currentTask;
  final bool allowLinkUpdate;

  const _TaskRecoveryDialog(this.task, this.feedback, this.refresh,
      this.currentTask, this.allowLinkUpdate);

  @override
  State<_TaskRecoveryDialog> createState() => _TaskRecoveryDialogState();
}

class _TaskRecoveryDialogState extends State<_TaskRecoveryDialog> {
  final _url = TextEditingController();
  final _form = GlobalKey<FormState>();
  late final _stats = getTaskStats(widget.task.id);
  final _snapshot = activeApiSnapshot;

  @override
  void dispose() {
    _url.dispose();
    super.dispose();
  }

  Future<void> _replace(TaskStats? stats) async {
    if (!_form.currentState!.validate()) return;
    final task = widget.currentTask();
    if (task == null ||
        _snapshot?.isCurrent != true ||
        task.createdAt != widget.task.createdAt ||
        task.updatedAt != widget.task.updatedAt ||
        task.meta.req.url != widget.task.meta.req.url ||
        !canReplaceTaskLink(task, stats) ||
        hasUnconfirmedHttpRetry(task)) {
      return;
    }
    final newLink = _url.text.trim();
    final attempt = widget.feedback.begin(task, linkUpdate: true);
    if (attempt == null) return;
    void report(RecoveryStage stage) => widget.feedback.report(attempt, stage);
    var writing = false, linkSaved = false;
    try {
      final identity = await HttpRecoveryApi.readIdentity(attempt.snapshot);
      if (identity == null || !mounted) {
        report(RecoveryStage.rejected);
        return;
      }
      final client = HttpRecoveryApi(
          snapshot: attempt.snapshot, identity: identity, taskId: task.id);
      final state = (await client.query()).recovery;
      if (!mounted ||
          state == null ||
          state.status != task.status.name ||
          state.management == 'held' ||
          !await client.sameDataset()) {
        report(RecoveryStage.rejected);
        return;
      }
      if (state.reason == 'cooldown' ||
          stats?.cooldownUntil?.isAfter(DateTime.now()) == true) {
        report(RecoveryStage.cooldown);
        return;
      }
      if (!mounted || !attempt.snapshot.isCurrent) return;
      writing = true;
      await patchTask(task.id, ResolveTask(req: Request(url: newLink)),
          distinguishRejection: true);
      linkSaved = true;
      if (!await client.sameDataset() || !attempt.snapshot.isCurrent) {
        report(RecoveryStage.unknown);
        return;
      }
      await continueTask(task.id, distinguishRejection: true);
      report(RecoveryStage.submitted);
      if (mounted) Navigator.of(context).pop();
    } on TaskWriteRejected catch (e) {
      final result = e.message;
      report(switch (result.msg) {
        _ when linkSaved => RecoveryStage.continueRejected,
        final reason? when reason.contains('partial file preserved') =>
          RecoveryStage.unsafeLink,
        'path_freeze_held' ||
        'operation_conflict' ||
        'handoff_pending' ||
        'invalid_request_context' ||
        'needs_scoped_context' ||
        'recovery_state_incompatible' ||
        'pause HTTP task before changing the request' =>
          RecoveryStage.rejected,
        _ when const {1001, 1002, 2001}.contains(result.code) =>
          RecoveryStage.rejected,
        _ => RecoveryStage.unknown,
      });
    } catch (_) {
      report(writing ? RecoveryStage.unknown : RecoveryStage.rejected);
    } finally {
      await widget.refresh();
    }
  }

  @override
  Widget build(BuildContext context) => FutureBuilder<TaskStats>(
      future: _stats,
      builder: (context, result) => Obx(() {
            final task = widget.task;
            final stage = widget.feedback.stage(task);
            final locked = widget.feedback.blocksWrite(task);
            final current = widget.currentTask();
            final failure = taskFailure(result.data,
                afterRecovery: widget.feedback.isLinkUpdate(task) &&
                    stage == RecoveryStage.failed);
            final canReplace = widget.allowLinkUpdate &&
                current != null &&
                current.updatedAt == task.updatedAt &&
                current.meta.req.url == task.meta.req.url &&
                canReplaceTaskLink(current, result.data) &&
                !hasUnconfirmedHttpRetry(task);
            final site = task.protocol == Protocol.media ||
                    task.protocol == Protocol.http &&
                        failure == TaskFailure.access
                ? _taskSourceSite(task)
                : null;
            return AlertDialog(
              scrollable: true,
              title:
                  Text(task.name, maxLines: 1, overflow: TextOverflow.ellipsis),
              content: SizedBox(
                width: 380,
                child: Form(
                  key: _form,
                  child: Column(mainAxisSize: MainAxisSize.min, children: [
                    Text(failure.label),
                    const SizedBox(height: 6),
                    Text(_nextStep(task, failure, result.data)),
                    if (stage != null) ...[
                      const SizedBox(height: 8),
                      Semantics(liveRegion: true, child: Text(stage.label)),
                    ],
                    if (canReplace) ...[
                      const SizedBox(height: 12),
                      TextFormField(
                        controller: _url,
                        enabled: !locked,
                        autofocus: true,
                        autocorrect: false,
                        enableSuggestions: false,
                        decoration: InputDecoration(
                            labelText: 'taskRecoveryNewLink'.tr),
                        validator: (value) {
                          final link = value?.trim() ?? '';
                          return _webAddress(link) == null ||
                                  link == task.meta.req.url
                              ? 'taskRecoveryInvalidInput'.tr
                              : null;
                        },
                      ),
                    ],
                  ]),
                ),
              ),
              actions: [
                TextButton(
                    onPressed: stage == RecoveryStage.checking
                        ? null
                        : () => Navigator.of(context).pop(),
                    child: Text('cancel'.tr)),
                if (site != null)
                  TextButton(
                      onPressed: locked
                          ? null
                          : () async {
                              var opened = false;
                              try {
                                opened = await launchUrl(Uri.parse(site.origin),
                                    mode: LaunchMode.externalApplication);
                              } catch (_) {
                                // Platform errors can include request details.
                              }
                              if (!opened && context.mounted) {
                                ScaffoldMessenger.maybeOf(context)
                                    ?.showSnackBar(SnackBar(
                                        content:
                                            Text('taskRecoverySiteFailed'.tr)));
                              }
                            },
                      child: Text('taskRecoveryOpenSite'.tr)),
                if (current != null &&
                    task.protocol == Protocol.netdisk &&
                    failure == TaskFailure.network)
                  TextButton(
                      onPressed: locked
                          ? null
                          : () => continueFailedTask(
                              current, widget.feedback, widget.refresh),
                      child: Text('httpRetry'.tr)),
                if (current != null && canSyncTaskLogin(current))
                  FilledButton(
                      onPressed: locked
                          ? null
                          : () {
                              Navigator.of(context).pop();
                              openTaskLogin(current);
                            },
                      child: Text('taskRecoverySyncDrive'.tr)),
                if (canReplace)
                  FilledButton(
                      onPressed: locked ? null : () => _replace(result.data),
                      child: Text('taskRecoveryReplaceAndContinue'.tr)),
              ],
            );
          }));
}

Future<RecoveryStage> continueFailedTask(
    Task task, TaskRecoveryFeedback feedback, Future<void> Function() refresh,
    {Future<void> Function()? send}) async {
  final attempt = feedback.begin(task);
  if (attempt == null) return RecoveryStage.unknown;
  var stage = RecoveryStage.submitted;
  try {
    await (send != null
        ? send()
        : continueTask(task.id, distinguishRejection: true));
  } on TaskWriteRejected {
    stage = RecoveryStage.rejected;
  } on NetdiskException catch (e) {
    stage = const {
      'request_failed',
      'expired',
      'login_required',
      'invalid_context'
    }.contains(e.reasonCode)
        ? RecoveryStage.rejected
        : RecoveryStage.unknown;
  } catch (_) {
    stage = RecoveryStage.unknown;
  }
  feedback.report(attempt, stage);
  await refresh();
  return stage;
}
