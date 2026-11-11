import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'dart:math' as math;
import 'dart:ui' show FontFeature;

import 'package:contentsize_tabbarview/contentsize_tabbarview.dart';
import 'package:desktop_drop/desktop_drop.dart';
import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import 'package:path/path.dart' as path;
import 'package:rounded_loading_button_plus/rounded_loading_button.dart';

import '../../../../api/api.dart';
import '../../../../api/model/create_task.dart';
import '../../../../api/model/create_task_batch.dart';
import '../../../../api/model/downloader_config.dart';
import '../../../../api/model/options.dart';
import '../../../../api/model/request.dart';
import '../../../../api/model/resolve_result.dart';
import '../../../../api/model/resolve_task.dart';
import '../../../../api/model/task.dart';
import '../../../../api/netdisk.dart';
import '../../../../api/task_batch_api.dart';
import '../../../../api/model/task_batch_operation.dart';
import '../../../../api/model/task_batch_scope.dart';
import '../../../../database/task_batch_journal.dart';
import '../../../../database/task_batch_journal_store.dart';
import '../../../../database/database.dart';
import '../../../../util/input_formatter.dart';
import '../../../../util/message.dart';
import '../../../../util/log_util.dart';
import '../../../../util/util.dart';
import '../../../../util/netdisk_browser.dart';
import '../../../../macos/mac_widgets.dart';
import '../../../../macos/mac_glass.dart';
import '../../../../macos/mac_appearance.dart';
import '../../../../macos/mac_panel.dart';
import '../../../../macos/mac_onboarding.dart';
import '../../task/controllers/task_controller.dart';
import '../../task/views/task_batch_groups.dart';
import '../../task/views/http_completion_status.dart';
import '../../../routes/app_pages.dart';
import '../../../views/compact_checkbox.dart';
import '../../../views/directory_selector.dart';
import '../../../views/mac_task_failure.dart';
import '../../../views/file_tree_view.dart';
import '../../app/controllers/app_controller.dart';
import '../../history/views/history_view.dart';
import '../controllers/create_controller.dart';
import 'netdisk_view.dart';
import 'browser_media_view.dart';
import 'http_checksum_input.dart';
import '../../../../../api/verified_http_api.dart';

class CreateView extends StatefulWidget {
  final TaskBatchJournalStore? batchJournal;
  final Duration batchAcknowledgementTimeout;
  final NetdiskApi? netdiskApi;
  final NetdiskBrowserTool? netdiskBrowserTool;
  final Future<String> Function(CreateTask)? taskCreator;
  final Future<ResolveResult> Function(ResolveTask)? taskResolver;
  const CreateView(
      {super.key,
      this.batchJournal,
      this.batchAcknowledgementTimeout = const Duration(seconds: 5),
      this.netdiskApi,
      this.netdiskBrowserTool,
      this.taskCreator,
      this.taskResolver});
  Protocol? parseProtocol(String url) => _parseProtocol(url);
  @override
  State<CreateView> createState() => _CreateViewState();
}

Protocol? _parseProtocol(String url) {
  if (netdiskProviderForUrl(url.trim()) != null) return Protocol.netdisk;
  final value = url.toUpperCase();
  if (value.startsWith('HTTP:') || value.startsWith('HTTPS:'))
    return Protocol.http;
  if (value.startsWith('MAGNET:') || value.endsWith('.TORRENT'))
    return Protocol.bt;
  if (value.startsWith('ED2K:')) return Protocol.ed2k;
  return null;
}

class _ManualBatchAttempt {
  final ScopedTaskBatchApi api;
  final TaskBatchJournalScope scope;
  final TaskBatchOperationRef operation;
  final CreateTaskBatch batch;
  final String input, signature;
  final String? expectedSha256;
  bool sent = false;
  _ManualBatchAttempt(this.api, this.scope, this.operation, this.batch,
      this.input, this.signature,
      {this.expectedSha256});
}

class _CreateViewState extends State<CreateView> {
  _ManualBatchAttempt? _manualBatch;
  int _manualEpoch = 0;
  int _confirmationEpoch = 0;
  int? _manualConfirmationEpoch;
  Completer<void>? _manualConfirmationStop;
  String _manualObservedText = '';
  bool _manualBusy = false;
  final _manualNotice = RxnString();

  void _manualInputChanged() {
    final text = _urlController.text;
    if (text == _manualObservedText) return;
    _manualObservedText = text;
    final wasWaiting = _manualBusy;
    ++_manualEpoch;
    _manualBusy = false;
    if (_manualBatch != null && _manualBatch!.input == text.trim()) {
      _manualNotice.value = '输入已变化，原批次只可查询。';
    } else {
      _manualBatch = null;
      _manualNotice.value = null;
    }
    if (wasWaiting) _endManualConfirmationWait();
  }

  CreateTaskBatch _manualPayload(List<String> urls, Options opts) =>
      CreateTaskBatch(
          reqs: urls
              .map((url) =>
                  CreateTaskBatchItem(req: _requestForUrl(url), opts: opts))
              .toList());
  bool _manualReferenceCurrent(_ManualBatchAttempt attempt, int epoch) =>
      mounted &&
      epoch == _manualEpoch &&
      identical(_manualBatch, attempt) &&
      attempt.api.snapshot.isCurrent;
  bool _manualDraftCurrent(_ManualBatchAttempt attempt, int epoch) =>
      _manualReferenceCurrent(attempt, epoch) &&
      _checksumDraftMatches(attempt) &&
      attempt.input == _urlController.text.trim() &&
      attempt.signature ==
          jsonEncode(_manualPayload(
                  Util.textToLines(attempt.input),
                  Options(
                      name: attempt.expectedSha256 == null
                          ? ''
                          : _renameController.text,
                      path: _pathController.text,
                      selectFiles: [],
                      extra: parseReqOptsExtra()))
              .toJson());
  bool _checksumDraftMatches(_ManualBatchAttempt attempt) {
    try {
      return normalizedExpectedSha256(_expectedSha256Controller.text) ==
              attempt.expectedSha256 &&
          (attempt.expectedSha256 == null ||
              _expectedSha256Url == attempt.input);
    } on FormatException {
      return false;
    }
  }

  void _checksumChanged() {
    _expectedSha256Url = _urlController.text.trim();
    if (_manualBusy) {
      ++_manualEpoch;
      _manualBusy = false;
      _endManualConfirmationWait();
      _manualNotice.value = '摘要已变化，原引用只可查询，当前草稿已保留。';
    }
  }

  void _releaseConfirmation() {
    _confirmController.reset();
    _confirmingCapture = null;
    _captureCompletedDuringConfirm = false;
    controller.isConfirming.value = false;
    _drainQueuedInputs();
  }

  void _endManualConfirmationWait() {
    final stop = _manualConfirmationStop;
    if (stop != null && _manualConfirmationEpoch == _confirmationEpoch) {
      ++_confirmationEpoch;
      _manualConfirmationStop = null;
      _manualConfirmationEpoch = null;
      // Release only this UI wait. The real journal write keeps its Box fence,
      // and the attempt keeps its consumed first-send permission.
      _releaseConfirmation();
      stop.complete();
    }
  }

  void _stopManualWait() {
    ++_manualEpoch;
    _endManualConfirmationWait();
    _manualBusy = false;
    _manualNotice.value =
        _manualBatch == null ? '已停止本次提交，未发送创建请求。' : '已停止等待。本批次引用已保留，请在原连接查询结果。';
    if (mounted) setState(() {});
  }

  Widget _manualBatchStatus() {
    final notice = _manualBatch?.api.snapshot.isCurrent == false
        ? '连接已变化，原批次引用已保留。'
        : _manualNotice.value;
    if (_manualBatch == null && notice == null) return const SizedBox.shrink();
    return Padding(
        padding: const EdgeInsets.symmetric(horizontal: 16, vertical: 4),
        child: Row(children: [
          Expanded(
              child: Text(notice ?? '正在确认批次…',
                  maxLines: 3,
                  overflow: TextOverflow.ellipsis,
                  style: const TextStyle(fontSize: 12))),
          if (_manualBatch != null)
            TextButton(
                key: const ValueKey('manual-batch-query'),
                onPressed: _manualBusy || controller.isConfirming.value
                    ? null
                    : _queryManualBatch,
                child: const Text('查询原批次')),
          if (_manualBusy)
            TextButton(
                key: const ValueKey('manual-batch-stop'),
                onPressed: _stopManualWait,
                child: const Text('停止等待'))
        ]));
  }

  Future<void> _acceptManualReply(_ManualBatchAttempt attempt, int epoch,
      ScopedTaskBatchReply reply) async {
    if (!_manualReferenceCurrent(attempt, epoch)) {
      return;
    }
    final receipt = reply.receipt;
    if (reply.envelope.message == 'dataset_mismatch') {
      _manualNotice.value = '数据域已变化，原批次引用已隔离保留。';
      TaskBatchGroups.referencesChanged();
      return;
    }
    if (reply.envelope.statusCode == 409) {
      _manualNotice.value = '这次内容与原批次冲突；请查询原批次，不会再次创建。';
      TaskBatchGroups.referencesChanged();
      return;
    }
    _manualNotice.value = receipt == null || receipt.state == 'unknown'
        ? '批次结果尚未确认，请查询原批次。'
        : '已创建 ${receipt.createdCount} 项，失败 ${receipt.failedCount} 项，待创建 ${receipt.pendingCount} 项。';
    TaskBatchGroups.referencesChanged();
    final count = attempt.batch.reqs!.length;
    if (!reply.requiresQuery &&
        receipt?.createdCount == count &&
        receipt?.failedCount == 0) {
      if (!_manualDraftCurrent(attempt, epoch)) {
        _manualNotice.value = '原批次已确认，当前修改后的草稿已保留。';
        return;
      }
      _createdIds.addAll(reply.createdTaskIds);
      _manualBatch = null;
      _manualBusy = false;
      _manualNotice.value = null;
      await _completeCreate();
    } else if (reply.createdTaskIds.isNotEmpty) {
      await MacPanel.showTaskResult(reply.createdTaskIds, '批次尚未全部确认，请查询原批次。');
      if (Get.isRegistered<MacTaskListController>()) {
        unawaited(Get.find<MacTaskListController>().getTasksState());
      }
    }
  }

  Future<void> _queryManualBatch() async {
    final attempt = _manualBatch;
    if (attempt == null || _manualBusy) {
      return;
    }
    final epoch = ++_manualEpoch;
    setState(() => _manualBusy = true);
    try {
      if (!_manualReferenceCurrent(attempt, epoch)) return;
      final sameDataset = await attempt.api.stillSameDataset();
      if (!_manualReferenceCurrent(attempt, epoch)) return;
      if (!sameDataset) {
        _manualNotice.value = '数据域已变化，原批次引用继续保留。';
        return;
      }
      final reply = await attempt.api.query(attempt.operation);
      await _acceptManualReply(attempt, epoch, reply);
    } catch (_) {
      if (_manualReferenceCurrent(attempt, epoch)) {
        _manualNotice.value = '暂时无法查询原批次，请保留引用并稍后查询。';
      }
    } finally {
      if (mounted && epoch == _manualEpoch) setState(() => _manualBusy = false);
    }
  }

  Future<void> _submitManualBatch(List<String> urls, Options opts,
      {String? expectedSha256}) async {
    if (_manualBatch != null) {
      await _queryManualBatch();
      return;
    }
    final snapshot = activeApiSnapshot;
    if (snapshot?.taskBatchRoute?.supported != true) {
      _manualNotice.value = '当前连接尚不能确认批次归属，未提交任务。';
      return;
    }
    final input = _urlController.text.trim();
    final epoch = ++_manualEpoch;
    final raw = jsonEncode(_manualPayload(urls, opts).toJson());
    final frozen = CreateTaskBatch.fromJson(jsonDecode(raw));
    setState(() => _manualBusy = true);
    _manualNotice.value = '正在检查批次能力…';
    try {
      final identity = await ScopedTaskBatchApi.readIdentity(snapshot!);
      if (!mounted || epoch != _manualEpoch || !snapshot.isCurrent) {
        return;
      }
      final ScopedTaskBatchApi bound = expectedSha256 == null
          ? ScopedTaskBatchApi(snapshot: snapshot, identity: identity)
          : VerifiedHttpApi(
              snapshot: snapshot,
              identity: identity,
              expectedSHA256: expectedSha256);
      if (bound is VerifiedHttpApi && !await bound.supported()) {
        if (mounted && epoch == _manualEpoch && snapshot.isCurrent) {
          _manualNotice.value = '当前下载服务未确认 SHA-256 校验能力，未提交任务。';
        }
        return;
      }
      if (!mounted || epoch != _manualEpoch || !snapshot.isCurrent) return;
      final id =
          'manual-${List.generate(16, (_) => math.Random.secure().nextInt(256).toRadixString(16).padLeft(2, '0')).join()}';
      final operation = TaskBatchOperationRef(
          namespace: TaskBatchOperationRef.namespaceV1, operationId: id);
      final attempt = _ManualBatchAttempt(
          bound,
          TaskBatchJournalScope(
              route: snapshot.taskBatchRoute!, dataset: identity),
          operation,
          frozen,
          input,
          raw,
          expectedSha256: expectedSha256);
      // Preflight is a pure capability/domain query, never a recovered submit permit.
      final preflight = await bound.query(operation);
      if (!mounted ||
          epoch != _manualEpoch ||
          !snapshot.isCurrent ||
          input != _urlController.text.trim()) {
        return;
      }
      if (!preflight.hasMatchingSnapshot ||
          preflight.envelope.statusCode != 200 ||
          preflight.envelope.code != 0) {
        _manualNotice.value = '当前下载服务不支持受保护的批次提交，未创建任务。';
        return;
      }
      _manualBatch = attempt;
      if (!_manualDraftCurrent(attempt, epoch)) {
        return;
      }
      _manualNotice.value = '正在记录批次引用…';
      final journal = widget.batchJournal ??
          TaskBatchJournalStore(box: Database.instance.box, capacity: 10000);
      final recorded = await journal.recordForQuery(attempt.scope, operation,
          acknowledgementTimeout: widget.batchAcknowledgementTimeout);
      TaskBatchGroups.referencesChanged();
      if (!_manualDraftCurrent(attempt, epoch)) {
        return;
      }
      if (!recorded.confirmed || preflight.receipt?.state != 'unknown') {
        _manualNotice.value = '批次引用需要查询确认，未再次发送创建请求。';
        return;
      }
      final sameDataset = await bound.stillSameDataset();
      if (!_manualReferenceCurrent(attempt, epoch)) return;
      if (!sameDataset || !_manualDraftCurrent(attempt, epoch)) {
        _manualNotice.value = '连接或数据域已变化，未发送创建请求，原引用已保留。';
        return;
      }
      // Synchronous, one-shot consumption before the only POST. Never restored
      // by finally, query, cancellation, duplicate record or a reopened journal.
      if (attempt.sent) {
        return;
      }
      attempt.sent = true;
      _manualNotice.value = '正在提交批次…';
      final reply = await bound.create(operation, frozen);
      await _acceptManualReply(attempt, epoch, reply);
    } catch (_) {
      if (mounted && epoch == _manualEpoch) {
        _manualNotice.value = _manualBatch == null
            ? '无法确认批次能力或数据域，未发送创建请求。'
            : '批次结果尚未确认，请只查询原批次。';
      }
    } finally {
      if (mounted && epoch == _manualEpoch) setState(() => _manualBusy = false);
    }
  }

  final _createdIds = <String>[];
  Future<String> _createTask(CreateTask task) async {
    _checkCaptureCompletion();
    final operation = _panelRequest?.browserOperationId;
    final matches =
        _panelRequest?.req?.url.trim() == _urlController.text.trim();
    if (operation != null && matches && _panelRequest?.browserCapture == true) {
      await Get.find<AppController>().authorizeBrowserConfirmation(operation);
    }
    final id = await (widget.taskCreator != null
        ? widget.taskCreator!(task)
        : createTask(task, operationId: matches ? operation : null));
    if (Util.isMacos()) {
      _createdIds.add(id);
      if (operation != null && matches) {
        Get.find<AppController>()
            .completeBrowserConfirmation(operation, taskIds: [id]);
      }
    }
    return id;
  }

  Future<List<String>> _createBatch(CreateTaskBatch batch) async {
    _checkCaptureCompletion();
    final operation =
        _panelRequest?.req?.url.trim() == _urlController.text.trim()
            ? _panelRequest?.browserOperationId
            : null;
    if (operation != null && _panelRequest?.browserCapture == true) {
      await Get.find<AppController>().authorizeBrowserConfirmation(operation);
    }
    final ids = await createTaskBatch(batch, operationId: operation);
    if (Util.isMacos()) {
      _createdIds.addAll(ids);
      if (operation != null) {
        Get.find<AppController>()
            .completeBrowserConfirmation(operation, taskIds: ids);
      }
    }
    return ids;
  }

  CreateController get controller => Get.find<CreateController>();
  NetdiskApi? get netdiskApi => widget.netdiskApi;
  NetdiskBrowserTool? get netdiskBrowserTool => widget.netdiskBrowserTool;
  final _inputFocus = FocusNode();
  int _inputFocusGeneration = 0;
  final _panelError = RxnString();
  final _panelShowCompleted = false.obs;
  final _queuedInputs = <CreateTask>[];
  CreateTask? _panelRequest;
  String? _confirmingCapture;
  Route<dynamic>? _captureResolveRoute;
  bool _captureCompletedDuringConfirm = false;
  Request? _originalRequest;
  Uri? _credentialOrigin;
  bool _clipboardChecked = false;
  Future<bool>? _onboarding;
  String? _lastDefaultDownloadPath;
  Worker? _pendingWorker, _captureWorker, _captureEndWorker;
  @override
  void initState() {
    super.initState();
    _urlController.addListener(_manualInputChanged);
    if (Util.isMacos()) {
      MacPanel.focusRequests.addListener(_focusInput);
      MacPanel.incoming.addListener(_receiveInput);
      MacPanel.taskResult.addListener(_receiveTaskResult);
      Get.find<AppController>()
          .browserIntegration
          .media
          .addListener(_receiveMedia);
      Get.find<AppController>().browserIntegration.addListener(_receiveMedia);
      _captureWorker =
          ever(Get.find<AppController>().browserCaptureRequests, (_) {
        if (!mounted ||
            controller.isConfirming.value ||
            _panelRequest?.browserOperationId != null ||
            _netdiskOpen.value) return;
        _clearBrowserDraft();
        setState(() {});
      });
      _captureEndWorker =
          ever(Get.find<AppController>().completedBrowserOperation, (id) {
        if (id != null)
          _finishBrowserCapture(
              id, Get.find<AppController>().browserCaptureCompletionMessage);
      });
      _panelRequest = MacPanel.incoming.value;
      _pendingWorker = ever(Get.find<AppController>().pendingUpdateTask, (_) {
        if (mounted) setState(() => _initialNetdiskHandled = false);
      });
    }
  }

  void _receiveMedia() {
    if (mounted) setState(() {});
  }

  void _receiveTaskResult() {
    final result = MacPanel.taskResult.value;
    if (!mounted) return;
    setState(() {
      if (result != null) {
        _panelShowCompleted.value = false;
        _panelError.value = result.error;
        controller.showAdvanced.value = false;
      }
    });
  }

  void _focusInput() {
    final generation = ++_inputFocusGeneration;
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted &&
          generation == _inputFocusGeneration &&
          MacPanel.visible.value &&
          MacPanel.presentation.value == 'panel' &&
          !_netdiskOpen.value &&
          _onboarding == null) {
        _inputFocus.requestFocus();
      }
    });
  }

  void _receiveInput() {
    final task = MacPanel.incoming.value;
    if (task == null) return;
    if (controller.isConfirming.value ||
        _netdiskOpen.value ||
        _onboarding != null ||
        _panelRequest?.browserOperationId != null) {
      _queuedInputs.add(task);
      return;
    }
    _resetTaskParameters();
    setState(() {
      _panelRequest = task;
      controller.pendingCreateHandled = false;
      _initialNetdiskHandled = false;
      _panelError.value = null;
      MacPanel.notice.value = null;
    });
  }

  @override
  void dispose() {
    ++_manualEpoch;
    _urlController.removeListener(_manualInputChanged);
    if (Util.isMacos()) MacPanel.cancelAutoHide();
    _pendingWorker?.dispose();
    _captureWorker?.dispose();
    _captureEndWorker?.dispose();
    MacPanel.focusRequests.removeListener(_focusInput);
    MacPanel.incoming.removeListener(_receiveInput);
    MacPanel.taskResult.removeListener(_receiveTaskResult);
    if (Get.isRegistered<AppController>()) {
      Get.find<AppController>()
          .browserIntegration
          .media
          .removeListener(_receiveMedia);
      Get.find<AppController>()
          .browserIntegration
          .removeListener(_receiveMedia);
    }
    _inputFocus.dispose();
    for (final field in [
      _urlController,
      _renameController,
      _connectionsController,
      _expectedSha256Controller,
      _pathController,
      _proxyIpController,
      _proxyPortController,
      _proxyUsrController,
      _proxyPwdController,
      _btTrackerController,
      _archivePasswordController,
      ..._httpHeaderControllers.expand((e) => [e.name, e.value])
    ]) {
      field.dispose();
    }
    super.dispose();
  }

  final _netdiskRequest = Rxn<Request>();
  final _netdiskOpen = false.obs;
  bool _initialNetdiskHandled = false;
  final _confirmFormKey = GlobalKey<FormState>();

  final _urlController = TextEditingController();
  final _renameController = TextEditingController();
  final _connectionsController = TextEditingController();
  final _expectedSha256Controller = TextEditingController();
  String? _expectedSha256Url;
  final _pathController = TextEditingController();
  final _confirmController = RoundedLoadingButtonController();
  final _proxyIpController = TextEditingController();
  final _proxyPortController = TextEditingController();
  final _proxyUsrController = TextEditingController();
  final _proxyPwdController = TextEditingController();
  final _httpHeaderControllers = [
    (
      name: TextEditingController(text: "User-Agent"),
      value: TextEditingController()
    ),
    (
      name: TextEditingController(text: "Cookie"),
      value: TextEditingController()
    ),
    (
      name: TextEditingController(text: "Referer"),
      value: TextEditingController()
    ),
  ];
  final _btTrackerController = TextEditingController();
  final _archivePasswordController = TextEditingController();

  final _availableSchemes = ["http:", "https:", "magnet:", "ed2k:"];

  final _skipVerifyCertController = false.obs;
  final _autoTorrentController = Rxn<bool>();
  final _deleteTorrentAfterDownloadController = Rxn<bool>();
  final _autoExtractController = Rxn<bool>();
  final _deleteAfterExtractController = Rxn<bool>();

  Widget _macLabeledField(String label, Widget field) {
    if (!Util.isMacos()) return field;
    return Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
      Text(label,
          style: const TextStyle(fontSize: 12, fontWeight: FontWeight.w500)),
      const SizedBox(height: 5),
      field is TextField ? SizedBox(height: 36, child: field) : field,
    ]);
  }

  double _macUrlHeight(BuildContext context, String text, double width) {
    final painter = TextPainter(
      text: TextSpan(
          text: text.isEmpty ? ' ' : text,
          style: Theme.of(context).textTheme.bodyMedium),
      textDirection: Directionality.of(context),
      maxLines: 5,
    )..layout(maxWidth: width > 149 ? width - 148 : 1);
    final lines = painter.computeLineMetrics().length.clamp(1, 5);
    painter.dispose();
    return 36.0 + 20.0 * (lines - 1);
  }

  Future<void> _cancelCreate() async {
    if (_manualBusy) {
      _stopManualWait();
      return;
    }
    if (Util.isMacos()) {
      if (controller.isConfirming.value) return;
      if (controller.showAdvanced.value) {
        controller.showAdvanced.value = false;
      } else {
        final operation = _panelRequest?.browserOperationId;
        if (operation != null) {
          if (_panelRequest?.browserHandoff != null) {
            final ctx = _panelRequest!.browserHandoff!;
            try {
              await Get.find<AppController>().browserHandoff.command({
                'action': 'handoffAbort',
                'operationId': operation,
                'clientId': ctx.clientId,
                'browser': ctx.browser,
                'reason': 'user_cancelled'
              });
              if (_panelRequest == null) await MacPanel.hide();
            } catch (_) {
              MacPanel.notice.value = '交接结果未知，请检查原下载和已有任务';
            }
            // The synchronous terminal callback may already have opened the next queued input.
            return;
          } else {
            Get.find<AppController>()
                .completeBrowserConfirmation(operation, cancelled: true);
          }
          if (identical(MacPanel.incoming.value, _panelRequest))
            MacPanel.incoming.value = null;
          _panelRequest = null;
          _urlController.clear();
          _resetTaskParameters();
          if (_queuedInputs.isNotEmpty) {
            _drainQueuedInputs();
            return;
          }
        }
        await MacPanel.hide();
      }
      return;
    }
    if (controller.isConfirming.value) return;
    if (Util.isMacos() && Get.rootDelegate.history.length > 1) {
      await Get.rootDelegate.popRoute(popMode: PopMode.History);
    } else {
      await _completeCreate();
    }
  }

  @override
  Widget build(BuildContext context) {
    final appController = Get.find<AppController>();
    final mediaSession = appController.browserIntegration.media.active;
    if (Util.isMacos() && mediaSession != null) {
      final preferences = appController.browserIntegration.clients
          .where((c) => c.id == mediaSession.clientId)
          .firstOrNull
          ?.preferences;
      return BrowserMediaView(
        key: ValueKey(mediaSession.id),
        media: appController.browserIntegration.media,
        session: mediaSession,
        showSmallImages: preferences?.showAll ?? false,
        defaultDirectory: appController.downloaderConfig.value.downloadDir,
        disableSite: () =>
            appController.browserIntegration.update(mediaSession.clientId, {
          'sniffing': {
            'disabledSites': {mediaSession.site: true}
          },
        }),
      );
    }

    if (_connectionsController.text.isEmpty) {
      _connectionsController.text = appController
          .downloaderConfig.value.protocolConfig.http.connections
          .toString();
    }
    if (Util.isMacos()) {
      _syncDefaultDownloadPath();
    } else if (_pathController.text.isEmpty) {
      // Render placeholders when initializing the path
      final downloadDir = appController.downloaderConfig.value.downloadDir;
      _pathController.text = renderPathPlaceholders(downloadDir);
    }

    // Handle pending create task from deep link
    final args = Get.rootDelegate.arguments();
    final CreateTask? routerParams = Util.isMacos()
        ? _panelRequest ?? (args is CreateTask ? args : null)
        : (args is CreateTask ? args : null);
    if (((routerParams?.req?.url.isNotEmpty ?? false) ||
            routerParams?.req?.protocol == 'netdisk') &&
        !controller.pendingCreateHandled) {
      controller.pendingCreateHandled = true;
      // get url from route arguments
      _originalRequest = routerParams!.req;
      _credentialOrigin = Uri.tryParse(routerParams.req!.url);
      _skipVerifyCertController.value = routerParams.req!.skipVerifyCert;
      controller.proxyConfig.value = routerParams.req!.proxy;
      final requestProxy = routerParams.req!.proxy;
      if (requestProxy?.mode == RequestProxyMode.custom) {
        final address =
            Uri.tryParse('${requestProxy!.scheme}://${requestProxy.host}');
        _proxyIpController.text = address?.host ?? '';
        _proxyPortController.text =
            address?.hasPort == true ? address!.port.toString() : '';
        _proxyUsrController.text = requestProxy.usr;
        _proxyPwdController.text = requestProxy.pwd;
      }
      final url = routerParams.req!.url;
      _urlController.text = url;
      _urlController.selection = TextSelection.fromPosition(
          TextPosition(offset: _urlController.text.length));
      final protocol = parseProtocol(url);
      if (routerParams.req!.protocol == 'netdisk') {
        _netdiskRequest.value = routerParams.req;
        if (routerParams.opts?.path.isNotEmpty ?? false) {
          _pathController.text = routerParams.opts!.path;
        }
      } else if (protocol != null) {
        final extraHandlers = {
          Protocol.http: () {
            final reqExtra = ReqExtraHttp.fromJson(
                jsonDecode(jsonEncode(routerParams.req!.extra)));
            _httpHeaderControllers.clear();
            reqExtra.header.forEach((key, value) {
              _httpHeaderControllers.add(
                (
                  name: TextEditingController(text: key),
                  value: TextEditingController(text: value),
                ),
              );
            });
            _skipVerifyCertController.value = routerParams.req!.skipVerifyCert;
          },
          Protocol.bt: () {
            final reqExtra = ReqExtraBt.fromJson(
                jsonDecode(jsonEncode(routerParams.req!.extra)));
            _btTrackerController.text = reqExtra.trackers.join("\n");
          },
          Protocol.ed2k: null,
        };
        if (routerParams.req?.extra != null) {
          extraHandlers[protocol]?.call();
        }

        // handle options
        if (routerParams.opts != null) {
          _renameController.text = routerParams.opts!.name;
          _pathController.text = routerParams.opts!.path;

          final optionsHandlers = {
            Protocol.http: () {
              final opt = routerParams.opts!;
              _renameController.text = opt.name;
              _pathController.text = opt.path;
              if (opt.extra != null) {
                final optsExtraHttp =
                    OptsExtraHttp.fromJson(jsonDecode(jsonEncode(opt.extra)));
                _connectionsController.text =
                    optsExtraHttp.connections.toString();
              }
            },
            Protocol.bt: null,
            Protocol.ed2k: null,
          };
          if (routerParams.opts?.extra != null) {
            optionsHandlers[protocol]?.call();
          }
        }
      }
    } else if (!Util.isIsolatedPreview &&
        _urlController.text.isEmpty &&
        !_clipboardChecked) {
      _clipboardChecked = true;
      // read clipboard
      Clipboard.getData('text/plain').then((value) {
        final clipboardText = value?.text ?? '';
        if (clipboardText.isNotEmpty) {
          if (parseNetdiskInputs(clipboardText).isNotEmpty ||
              _availableSchemes
                  .where((e) =>
                      clipboardText.startsWith(e) ||
                      clipboardText.startsWith(e.toUpperCase()))
                  .isNotEmpty) {
            _urlController.text = clipboardText;
            _urlController.selection = TextSelection.fromPosition(
                TextPosition(offset: _urlController.text.length));
            return;
          }

          recognizeMagnetUri(clipboardText);
        }
      });
    }

    if ((!Util.isMacos() || MacPanel.presentation.value == 'panel') &&
        !_initialNetdiskHandled &&
        (appController.pendingUpdateTask.value != null ||
            _netdiskRequest.value != null)) {
      _initialNetdiskHandled = true;
      WidgetsBinding.instance.addPostFrameCallback((_) async {
        if (!context.mounted) return;
        try {
          if (!await _ensureMacOnboarding() || !mounted) return;
          if (await _showPendingNetdiskTask()) return;
          final request = _netdiskRequest.value;
          if (request != null &&
              await _showNetdisk(NetdiskInput.fromRequest(request),
                  options: routerParams?.opts)) {
            _netdiskRequest.value = null;
            _completeCreate();
          }
        } catch (e) {
          _panelError.value = e.toString();
        } finally {
          _drainQueuedInputs();
        }
      });
    }

    final torrentButton = IconButton(
      constraints: Util.isMacos()
          ? const BoxConstraints.tightFor(width: 32, height: 32)
          : null,
      padding: Util.isMacos() ? EdgeInsets.zero : null,
      icon: Icon(Util.isMacos() ? CupertinoIcons.folder : Icons.folder_open),
      onPressed: () async {
        if (Util.isMacos()) {
          final paths =
              await MacPanel.chooseFiles(extensions: const ['torrent']);
          if (paths != null && paths.isNotEmpty) {
            _urlController.text = paths.first;
          }
          return;
        }
        var pr = await FilePicker.platform
            .pickFiles(type: FileType.custom, allowedExtensions: ["torrent"]);
        if (pr != null) {
          if (!Util.isWeb()) {
            _urlController.text = pr.files[0].path ?? "";
            return;
          }
          _urlController.text = pr.files[0].name;
          controller.setFileDataUri(pr.files[0].bytes!);
        }
      },
    );
    final historyButton = IconButton(
      constraints: Util.isMacos()
          ? const BoxConstraints.tightFor(width: 32, height: 32)
          : null,
      padding: Util.isMacos() ? EdgeInsets.zero : null,
      icon: Icon(Util.isMacos() ? CupertinoIcons.clock : Icons.history_rounded),
      onPressed: () async {
        List<String> resultOfHistories =
            Database.instance.getCreateHistory() ?? [];
        // show dialog box to list history
        if (context.mounted) {
          showGeneralDialog(
            barrierColor: Colors.black.withOpacity(0.5),
            transitionBuilder: (context, a1, a2, widget) {
              return Transform.scale(
                scale: a1.value,
                child: Opacity(
                  opacity: a1.value,
                  child: HistoryView(
                    isHistoryListEmpty: resultOfHistories.isEmpty,
                    historyList: ListView.builder(
                      itemCount: resultOfHistories.length,
                      itemBuilder: (context, index) {
                        return GestureDetector(
                          onTap: () {
                            _urlController.text = resultOfHistories[index];
                            Navigator.pop(context);
                          },
                          child: MouseRegion(
                            cursor: SystemMouseCursors.click,
                            child: Container(
                              padding: const EdgeInsets.symmetric(
                                horizontal: 8.0,
                                vertical: 8.0,
                              ),
                              margin: const EdgeInsets.symmetric(
                                horizontal: 10.0,
                                vertical: 8.0,
                              ),
                              decoration: BoxDecoration(
                                color: Theme.of(context).colorScheme.surface,
                                borderRadius: BorderRadius.circular(10.0),
                              ),
                              child: Text(
                                resultOfHistories[index],
                              ),
                            ),
                          ),
                        );
                      },
                    ),
                  ),
                ),
              );
            },
            transitionDuration: const Duration(milliseconds: 250),
            barrierDismissible: true,
            barrierLabel: '',
            context: context,
            pageBuilder: (context, animation1, animation2) {
              return const Text('PAGE BUILDER');
            },
          );
        }
      },
    );
    final clearUrlButton = IconButton(
      constraints: Util.isMacos()
          ? const BoxConstraints.tightFor(width: 32, height: 32)
          : null,
      padding: Util.isMacos() ? EdgeInsets.zero : null,
      onPressed: () {
        _urlController.clear();
        controller.clearFileDataUri();
      },
      icon:
          Icon(Util.isMacos() ? CupertinoIcons.xmark_circle_fill : Icons.clear),
    );

    final content = Scaffold(
      appBar: AppBar(
        leading: IconButton(
            icon:
                Icon(Util.isMacos() ? CupertinoIcons.xmark : Icons.arrow_back),
            tooltip: 'cancel'.tr,
            onPressed: _cancelCreate),
        // actions: [],
        title: Text('create'.tr),
        bottom: Util.isMacos()
            ? PreferredSize(
                preferredSize: const Size.fromHeight(1),
                child: Divider(height: 1, color: MacColors.of(context).border))
            : null,
      ),
      bottomNavigationBar: Util.isMacos()
          ? DecoratedBox(
              decoration: BoxDecoration(
                  border: Border(
                      top: BorderSide(color: MacColors.of(context).border))),
              child: Padding(
                padding: const EdgeInsets.fromLTRB(24, 8, 24, 12),
                child: Obx(() => Wrap(
                        alignment: WrapAlignment.end,
                        spacing: 10,
                        runSpacing: 8,
                        crossAxisAlignment: WrapCrossAlignment.center,
                        children: [
                          SizedBox(
                              width: double.infinity,
                              height: 32,
                              child: GestureDetector(
                                  behavior: HitTestBehavior.opaque,
                                  onTap: () =>
                                      controller.directDownload.toggle(),
                                  child: Row(children: [
                                    SizedBox(
                                        key: const ValueKey(
                                            'mac-create-direct-checkbox'),
                                        width: 18,
                                        height: 18,
                                        child: CupertinoCheckbox(
                                            value:
                                                controller.directDownload.value,
                                            activeColor:
                                                MacColors.of(context).accent,
                                            onChanged: (value) => controller
                                                .directDownload
                                                .value = value ?? false)),
                                    const SizedBox(width: 8),
                                    Text('directDownload'.tr),
                                  ]))),
                          TextButton(
                              onPressed: () => controller.showAdvanced.toggle(),
                              child: Row(
                                  mainAxisSize: MainAxisSize.min,
                                  children: [
                                    Icon(
                                        controller.showAdvanced.value
                                            ? CupertinoIcons.chevron_up
                                            : CupertinoIcons.chevron_down,
                                        size: 12),
                                    const SizedBox(width: 6),
                                    Text('advancedOptions'.tr),
                                  ])),
                          MacButton(
                              onPressed: controller.isConfirming.value
                                  ? null
                                  : _cancelCreate,
                              child: Text('cancel'.tr)),
                          MacButton(
                              primary: true,
                              onPressed: controller.isConfirming.value
                                  ? null
                                  : _doConfirm,
                              child: controller.isConfirming.value
                                  ? SizedBox(
                                      width: 14,
                                      height: 14,
                                      child: CircularProgressIndicator(
                                          strokeWidth: 2,
                                          color: MacColors.of(context)
                                              .primaryText))
                                  : Text('confirm'.tr)),
                        ])),
              ))
          : null,
      body: DropTarget(
        onDragDone: (details) async {
          if (!Util.isWeb()) {
            _urlController.text = details.files[0].path;
            return;
          }
          _urlController.text = details.files[0].name;
          final bytes = await details.files[0].readAsBytes();
          controller.setFileDataUri(bytes);
        },
        child: GestureDetector(
          behavior: HitTestBehavior.opaque,
          onTap: () {
            FocusScope.of(context).requestFocus(FocusNode());
          },
          child: SingleChildScrollView(
            child: Padding(
              padding:
                  const EdgeInsets.symmetric(vertical: 16.0, horizontal: 24.0),
              child: Form(
                key: _confirmFormKey,
                autovalidateMode: AutovalidateMode.onUserInteraction,
                child: Column(
                  children: [
                    if (MacPanel.presentation.value != 'panel')
                      Obx(_manualBatchStatus),
                    _macLabeledField(
                        'downloadLink'.tr,
                        Row(children: [
                          Expanded(
                            child: ValueListenableBuilder<TextEditingValue>(
                              valueListenable: _urlController,
                              builder: (context, value, _) => LayoutBuilder(
                                builder: (context, constraints) =>
                                    ConstrainedBox(
                                  constraints: BoxConstraints(
                                      minHeight: Util.isMacos()
                                          ? _macUrlHeight(context, value.text,
                                              constraints.maxWidth)
                                          : 0,
                                      maxHeight: Util.isMacos()
                                          ? 120
                                          : double.infinity),
                                  child: TextFormField(
                                    autofocus: !Util.isMobile(),
                                    controller: _urlController,
                                    minLines: 1,
                                    maxLines: Util.isMacos() ? null : 5,
                                    decoration: InputDecoration(
                                      isDense: Util.isMacos() ? true : null,
                                      hintText: _hitText(),
                                      hintStyle: const TextStyle(fontSize: 12),
                                      labelText: Util.isMacos()
                                          ? null
                                          : 'downloadLink'.tr,
                                      contentPadding: Util.isMacos()
                                          ? const EdgeInsets.symmetric(
                                              horizontal: 10, vertical: 8)
                                          : null,
                                      prefixIconConstraints: Util.isMacos()
                                          ? const BoxConstraints.tightFor(
                                              width: 32, height: 32)
                                          : null,
                                      suffixIconConstraints: Util.isMacos()
                                          ? const BoxConstraints.tightFor(
                                              height: 32)
                                          : null,
                                      icon: Util.isMacos()
                                          ? null
                                          : const Icon(Icons.link),
                                      prefixIcon: Util.isMacos()
                                          ? const Icon(CupertinoIcons.link,
                                              size: 18)
                                          : null,
                                      suffixIcon: Util.isMacos()
                                          ? Row(
                                              mainAxisSize: MainAxisSize.min,
                                              children: [
                                                  clearUrlButton,
                                                  torrentButton,
                                                  historyButton
                                                ])
                                          : clearUrlButton,
                                    ),
                                    validator: (v) {
                                      return v!.trim().isNotEmpty
                                          ? null
                                          : 'downloadLinkValid'.tr;
                                    },
                                    onChanged: (v) async {
                                      _clearCapturedHeadersFor(v);
                                      _netdiskRequest.value = null;
                                      controller.clearFileDataUri();
                                      if (controller.oldUrl.value.isEmpty) {
                                        recognizeMagnetUri(v);
                                      }
                                      controller.oldUrl.value = v;
                                    },
                                  ),
                                ),
                              ),
                            ),
                          ),
                          if (!Util.isMacos()) torrentButton,
                          if (!Util.isMacos()) historyButton,
                        ])),
                    Padding(
                      padding: Util.isMacos()
                          ? const EdgeInsets.only(top: 12)
                          : const EdgeInsets.only(left: 40),
                      child: Column(children: [
                        _macLabeledField(
                            'rename'.tr,
                            TextField(
                              controller: _renameController,
                              textAlignVertical: TextAlignVertical.center,
                              decoration: InputDecoration(
                                  isDense: Util.isMacos() ? true : null,
                                  labelText:
                                      Util.isMacos() ? null : 'rename'.tr,
                                  constraints: Util.isMacos()
                                      ? const BoxConstraints.tightFor(
                                          height: 36)
                                      : null,
                                  contentPadding: Util.isMacos()
                                      ? const EdgeInsets.symmetric(
                                          horizontal: 10, vertical: 8)
                                      : null),
                            )),
                        if (Util.isMacos()) const SizedBox(height: 12),
                        _macLabeledField(
                            'connections'.tr,
                            TextField(
                              controller: _connectionsController,
                              textAlignVertical: TextAlignVertical.center,
                              decoration: InputDecoration(
                                isDense: Util.isMacos() ? true : null,
                                labelText:
                                    Util.isMacos() ? null : 'connections'.tr,
                                constraints: Util.isMacos()
                                    ? const BoxConstraints.tightFor(height: 36)
                                    : null,
                                contentPadding: Util.isMacos()
                                    ? const EdgeInsets.symmetric(
                                        horizontal: 10, vertical: 8)
                                    : null,
                              ),
                              keyboardType: TextInputType.number,
                              inputFormatters: [
                                FilteringTextInputFormatter.digitsOnly,
                                NumericalRangeFormatter(min: 1, max: 256),
                              ],
                            )),
                        if (Util.isMacos()) const SizedBox(height: 12),
                        if (Util.isMacos()) ...[
                          HttpChecksumInput(
                            controller: _expectedSha256Controller,
                            onChanged: (_) => _checksumChanged(),
                          ),
                          const SizedBox(height: 12),
                        ],
                        _macLabeledField(
                            'downloadDir'.tr,
                            DirectorySelector(
                              controller: _pathController,
                              inlineActions: Util.isMacos(),
                            )),
                        // Category selector
                        _buildCategorySelector(appController),
                        Obx(
                          () => Visibility(
                            visible: controller.showAdvanced.value,
                            child: Column(
                              mainAxisSize: MainAxisSize.min,
                              crossAxisAlignment: CrossAxisAlignment.start,
                              children: [
                                Column(
                                  mainAxisSize: MainAxisSize.min,
                                  crossAxisAlignment: CrossAxisAlignment.start,
                                  children: [
                                    Transform.translate(
                                      offset: const Offset(-40, 0),
                                      child: Row(
                                        children: [
                                          const Icon(
                                            Icons.wifi_2_bar,
                                            color: Colors.grey,
                                          ),
                                          const SizedBox(
                                            width: 15,
                                          ),
                                          SizedBox(
                                              width: 150,
                                              child: DropdownButton<
                                                  RequestProxyMode>(
                                                hint: Text('proxy'.tr),
                                                isExpanded: true,
                                                value: controller
                                                    .proxyConfig.value?.mode,
                                                onChanged: (value) async {
                                                  if (value != null) {
                                                    controller.proxyConfig
                                                        .value = RequestProxy()
                                                      ..mode = value;
                                                  }
                                                },
                                                items: [
                                                  DropdownMenuItem<
                                                      RequestProxyMode>(
                                                    value:
                                                        RequestProxyMode.follow,
                                                    child: Text(
                                                        'followSettings'.tr),
                                                  ),
                                                  DropdownMenuItem<
                                                      RequestProxyMode>(
                                                    value:
                                                        RequestProxyMode.none,
                                                    child: Text('noProxy'.tr),
                                                  ),
                                                  DropdownMenuItem<
                                                      RequestProxyMode>(
                                                    value:
                                                        RequestProxyMode.custom,
                                                    child:
                                                        Text('customProxy'.tr),
                                                  ),
                                                ],
                                              ))
                                        ],
                                      ),
                                    ),
                                    ...(controller.proxyConfig.value?.mode ==
                                            RequestProxyMode.custom
                                        ? [
                                            SizedBox(
                                              width: 150,
                                              child: DropdownButtonFormField<
                                                  String>(
                                                value: controller
                                                    .proxyConfig.value?.scheme,
                                                onChanged: (value) async {
                                                  if (value != null) {}
                                                },
                                                items: const [
                                                  DropdownMenuItem<String>(
                                                    value: 'http',
                                                    child: Text('HTTP'),
                                                  ),
                                                  DropdownMenuItem<String>(
                                                    value: 'https',
                                                    child: Text('HTTPS'),
                                                  ),
                                                  DropdownMenuItem<String>(
                                                    value: 'socks5',
                                                    child: Text('SOCKS5'),
                                                  ),
                                                ],
                                              ),
                                            ),
                                            Row(children: [
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyIpController,
                                                  decoration: InputDecoration(
                                                    labelText: 'server'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                ),
                                              ),
                                              const Padding(
                                                  padding: EdgeInsets.only(
                                                      left: 10)),
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyPortController,
                                                  decoration: InputDecoration(
                                                    labelText: 'port'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                  keyboardType:
                                                      TextInputType.number,
                                                  inputFormatters: [
                                                    FilteringTextInputFormatter
                                                        .digitsOnly,
                                                    NumericalRangeFormatter(
                                                        min: 0, max: 65535),
                                                  ],
                                                ),
                                              ),
                                            ]),
                                            Row(children: [
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyUsrController,
                                                  decoration: InputDecoration(
                                                    labelText: 'username'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                ),
                                              ),
                                              const Padding(
                                                  padding: EdgeInsets.only(
                                                      left: 10)),
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyPwdController,
                                                  decoration: InputDecoration(
                                                    labelText: 'password'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                ),
                                              ),
                                            ])
                                          ]
                                        : const []),
                                  ],
                                ),
                                const Divider(),
                                TabBar(
                                  controller: controller.advancedTabController,
                                  tabs: const [
                                    Tab(
                                      text: 'HTTP',
                                    ),
                                    Tab(
                                      text: 'BitTorrent',
                                    ),
                                  ],
                                ),
                                DefaultTabController(
                                  length: 2,
                                  child: ContentSizeTabBarView(
                                    controller:
                                        controller.advancedTabController,
                                    children: [
                                      Column(
                                        children: [
                                          ..._httpHeaderControllers.map((e) {
                                            return Row(
                                              children: [
                                                Flexible(
                                                  child: TextFormField(
                                                    controller: e.name,
                                                    decoration: InputDecoration(
                                                      hintText:
                                                          'httpHeaderName'.tr,
                                                    ),
                                                  ),
                                                ),
                                                const Padding(
                                                    padding: EdgeInsets.only(
                                                        left: 10)),
                                                Flexible(
                                                  child: TextFormField(
                                                    controller: e.value,
                                                    decoration: InputDecoration(
                                                      hintText:
                                                          'httpHeaderValue'.tr,
                                                    ),
                                                  ),
                                                ),
                                                const Padding(
                                                    padding: EdgeInsets.only(
                                                        left: 10)),
                                                IconButton(
                                                  icon: const Icon(Icons.add),
                                                  onPressed: () {
                                                    _httpHeaderControllers.add(
                                                      (
                                                        name:
                                                            TextEditingController(),
                                                        value:
                                                            TextEditingController(),
                                                      ),
                                                    );
                                                    controller.showAdvanced
                                                        .update((val) => val);
                                                  },
                                                ),
                                                IconButton(
                                                  icon:
                                                      const Icon(Icons.remove),
                                                  onPressed: () {
                                                    if (_httpHeaderControllers
                                                            .length <=
                                                        1) {
                                                      return;
                                                    }
                                                    _httpHeaderControllers
                                                        .remove(e);
                                                    controller.showAdvanced
                                                        .update((val) => val);
                                                  },
                                                ),
                                              ],
                                            );
                                          }),
                                          Padding(
                                            padding:
                                                const EdgeInsets.only(top: 10),
                                            child: CompactCheckbox(
                                              label: 'skipVerifyCert'.tr,
                                              value: _skipVerifyCertController
                                                  .value,
                                              onChanged: (bool? value) {
                                                _skipVerifyCertController
                                                    .value = value ?? false;
                                              },
                                              textStyle: const TextStyle(
                                                color: Colors.grey,
                                              ),
                                            ),
                                          ),
                                          // AutoTorrent options
                                          Padding(
                                            padding:
                                                const EdgeInsets.only(top: 10),
                                            child: CompactCheckbox(
                                              label: 'autoTorrentEnable'.tr,
                                              value: _autoTorrentController
                                                      .value ??
                                                  false,
                                              onChanged: (bool? value) {
                                                _autoTorrentController.value =
                                                    value ?? false;
                                              },
                                              textStyle: const TextStyle(
                                                color: Colors.grey,
                                              ),
                                            ),
                                          ),
                                          Obx(
                                            () => Visibility(
                                              visible: _autoTorrentController
                                                      .value ??
                                                  false,
                                              child: Padding(
                                                padding: const EdgeInsets.only(
                                                    top: 10, left: 20),
                                                child: CompactCheckbox(
                                                  label:
                                                      'autoTorrentDeleteAfterDownload'
                                                          .tr,
                                                  value:
                                                      _deleteTorrentAfterDownloadController
                                                              .value ??
                                                          false,
                                                  onChanged: (bool? value) {
                                                    _deleteTorrentAfterDownloadController
                                                        .value = value ?? false;
                                                  },
                                                  textStyle: const TextStyle(
                                                    color: Colors.grey,
                                                  ),
                                                ),
                                              ),
                                            ),
                                          ),
                                          // AutoExtract options
                                          Padding(
                                            padding:
                                                const EdgeInsets.only(top: 10),
                                            child: CompactCheckbox(
                                              label: 'autoExtract'.tr,
                                              value: _autoExtractController
                                                      .value ??
                                                  false,
                                              onChanged: (bool? value) {
                                                _autoExtractController.value =
                                                    value ?? false;
                                              },
                                              textStyle: const TextStyle(
                                                color: Colors.grey,
                                              ),
                                            ),
                                          ),
                                          Obx(
                                            () => Visibility(
                                              visible: _autoExtractController
                                                      .value ??
                                                  false,
                                              child: Column(
                                                children: [
                                                  Padding(
                                                    padding:
                                                        const EdgeInsets.only(
                                                            top: 10, left: 20),
                                                    child: TextFormField(
                                                      controller:
                                                          _archivePasswordController,
                                                      obscureText: true,
                                                      decoration:
                                                          InputDecoration(
                                                        labelText:
                                                            'archivePassword'
                                                                .tr,
                                                        hintText:
                                                            'archivePasswordHint'
                                                                .tr,
                                                      ),
                                                    ),
                                                  ),
                                                  Padding(
                                                    padding:
                                                        const EdgeInsets.only(
                                                            top: 10, left: 20),
                                                    child: CompactCheckbox(
                                                      label:
                                                          'deleteAfterExtract'
                                                              .tr,
                                                      value:
                                                          _deleteAfterExtractController
                                                                  .value ??
                                                              false,
                                                      onChanged: (bool? value) {
                                                        _deleteAfterExtractController
                                                                .value =
                                                            value ?? false;
                                                      },
                                                      textStyle:
                                                          const TextStyle(
                                                        color: Colors.grey,
                                                      ),
                                                    ),
                                                  ),
                                                ],
                                              ),
                                            ),
                                          ),
                                        ],
                                      ),
                                      Column(
                                        children: [
                                          TextFormField(
                                              controller: _btTrackerController,
                                              maxLines: 5,
                                              decoration: InputDecoration(
                                                labelText: 'Trackers',
                                                hintText: 'addTrackerHit'.tr,
                                              )),
                                        ],
                                      ),
                                    ],
                                  ),
                                )
                              ],
                            ).paddingOnly(top: 16),
                          ),
                        ),
                      ]),
                    ),
                    if (!Util.isMacos())
                      Center(
                        child: Padding(
                          padding: const EdgeInsets.only(top: 15),
                          child: Column(
                            children: [
                              Row(
                                mainAxisSize: MainAxisSize.min,
                                children: [
                                  CompactCheckbox(
                                      label: 'directDownload'.tr,
                                      value: controller.directDownload.value,
                                      onChanged: (bool? value) {
                                        controller.directDownload.value =
                                            value ?? false;
                                      }),
                                  TextButton(
                                    onPressed: () {
                                      controller.showAdvanced.value =
                                          !controller.showAdvanced.value;
                                    },
                                    child: Row(children: [
                                      Obx(() => Checkbox(
                                            value:
                                                controller.showAdvanced.value,
                                            onChanged: (bool? value) {
                                              controller.showAdvanced.value =
                                                  value ?? false;
                                            },
                                          )),
                                      Text('advancedOptions'.tr),
                                    ]),
                                  ),
                                ],
                              ),
                              SizedBox(
                                width: 150,
                                child: Util.isMacos()
                                    ? Obx(() => MacButton(
                                        primary: true,
                                        onPressed: controller.isConfirming.value
                                            ? null
                                            : _doConfirm,
                                        child: controller.isConfirming.value
                                            ? const CupertinoActivityIndicator(
                                                radius: 7)
                                            : Text('confirm'.tr)))
                                    : RoundedLoadingButton(
                                        color: Get.theme.colorScheme.secondary,
                                        onPressed: _doConfirm,
                                        controller: _confirmController,
                                        child: Text('confirm'.tr),
                                      ),
                              ),
                            ],
                          ),
                        ),
                      ),
                  ],
                ),
              ),
            ),
          ),
        ),
      ),
    );
    if (!Util.isMacos()) return content;
    return _buildMacPanel(context, content);
  }

  Request _requestForUrl(String url) {
    _clearCapturedHeadersFor(url);
    final original = _originalRequest;
    if (original != null && original.url == url) {
      final request = Request.fromJson(original.toJson());
      request.proxy = parseProxy();
      request.skipVerifyCert = _skipVerifyCertController.value;
      if (original.protocol == null || original.protocol == 'http') {
        final edited = parseReqExtra(url);
        final preserved = jsonDecode(jsonEncode(original.extra));
        if (preserved is Map) {
          request.extra = {
            ...preserved,
            'header':
                edited is ReqExtraHttp ? edited.header : <String, String>{}
          };
        } else {
          request.extra = edited;
        }
      } else if (original.protocol == 'bt') {
        request.extra = parseReqExtra(url);
      }
      return request;
    }
    return Request(
        url: url,
        extra: parseReqExtra(url),
        proxy: parseProxy(),
        skipVerifyCert: _skipVerifyCertController.value);
  }

  Future<void> _completeCreate() async {
    if (!mounted) return;
    if (!Util.isMacos()) {
      await Get.rootDelegate.offNamed(Routes.TASK);
      return;
    }
    final ids = _createdIds.toList();
    _createdIds.clear();
    _urlController.clear();
    _originalRequest = null;
    _credentialOrigin = null;
    if (identical(MacPanel.incoming.value, _panelRequest))
      MacPanel.incoming.value = null;
    _panelRequest = null;
    _netdiskRequest.value = null;
    controller.clearFileDataUri();
    controller.showAdvanced.value = false;
    _resetTaskParameters();
    if (ids.isNotEmpty) {
      await MacPanel.showTaskResult(ids, null);
    } else {
      MacPanel.submitted();
    }
    if (Get.isRegistered<MacTaskListController>()) {
      Get.find<MacTaskListController>().getTasksState();
    }
  }

  void _finishBrowserCapture(String id, String message) {
    if (!mounted) return;
    if (_confirmingCapture == id) {
      _captureCompletedDuringConfirm = true;
      final route = _captureResolveRoute;
      _captureResolveRoute = null;
      if (route?.isActive == true) route!.navigator?.removeRoute(route);
    }
    _queuedInputs.removeWhere(
        (task) => task.browserCapture && task.browserOperationId == id);
    if (_panelRequest?.browserCapture != true ||
        _panelRequest?.browserOperationId != id) return;
    _clearBrowserDraft();
    _panelError.value = message;
    _drainQueuedInputs();
    setState(() {});
  }

  void _checkCaptureCompletion() {
    if (_captureCompletedDuringConfirm) {
      throw BrowserCaptureEnded(
          Get.find<AppController>().browserCaptureCompletionMessage);
    }
  }

  void _clearBrowserDraft() {
    // Close EditableText's connection before controller changes can send an
    // editing state to a native field that is being dismissed. Unfocus alone
    // defers its listeners to a microtask; apply them before clearing the draft.
    _inputFocusGeneration++;
    final focus = FocusManager.instance.primaryFocus;
    if (focus?.context?.findAncestorStateOfType<_CreateViewState>() == this) {
      focus!.unfocus();
      FocusManager.instance.applyFocusChangesIfNeeded();
    }
    if (identical(MacPanel.incoming.value, _panelRequest)) {
      MacPanel.incoming.value = null;
    }
    _panelRequest = null;
    _urlController.clear();
    controller.clearFileDataUri();
    controller.pendingCreateHandled = false;
    controller.showAdvanced.value = false;
    _initialNetdiskHandled = false;
    _resetTaskParameters();
  }

  void _resetTaskParameters() {
    _netdiskRequest.value = null;
    _credentialOrigin = null;
    for (final row in _httpHeaderControllers) {
      row.name.dispose();
      row.value.dispose();
    }
    _httpHeaderControllers.clear();
    for (final name in ['User-Agent', 'Cookie', 'Referer']) {
      _httpHeaderControllers.add((
        name: TextEditingController(text: name),
        value: TextEditingController()
      ));
    }
    _originalRequest = null;
    _renameController.clear();
    _expectedSha256Controller.clear();
    _expectedSha256Url = null;
    _btTrackerController.clear();
    _archivePasswordController.clear();
    _proxyIpController.clear();
    _proxyPortController.clear();
    _proxyUsrController.clear();
    _proxyPwdController.clear();
    controller.proxyConfig.value = null;
    _skipVerifyCertController.value = false;
    _autoTorrentController.value = null;
    _deleteTorrentAfterDownloadController.value = null;
    _autoExtractController.value = null;
    _deleteAfterExtractController.value = null;
    final config = Get.find<AppController>().downloaderConfig.value;
    _connectionsController.text =
        config.protocolConfig.http.connections.toString();
    _lastDefaultDownloadPath = renderPathPlaceholders(config.downloadDir);
    _pathController.text = _lastDefaultDownloadPath!;
    controller.directDownload.value = config.extra.defaultDirectDownload;
  }

  void _drainQueuedInputs() {
    if (!mounted ||
        controller.isConfirming.value ||
        _netdiskOpen.value ||
        _onboarding != null ||
        _panelRequest?.browserOperationId != null ||
        _queuedInputs.isEmpty) return;
    final next = _queuedInputs.removeAt(0);
    MacPanel.cancelAutoHide();
    _resetTaskParameters();
    setState(() {
      _panelRequest = next;
      controller.pendingCreateHandled = false;
      _initialNetdiskHandled = false;
      _panelError.value = null;
      MacPanel.notice.value = null;
    });
    _focusInput();
  }

  void _clearCapturedHeadersFor(String value) {
    final origin = _credentialOrigin;
    if (origin == null) return;
    final next = Uri.tryParse(value.trim());
    if (next?.scheme == origin.scheme &&
        next?.host == origin.host &&
        next?.port == origin.port) return;
    for (final field in _httpHeaderControllers) {
      field.value.clear();
    }
    _credentialOrigin = null;
    _originalRequest = null;
  }

  Widget _buildMacPanel(BuildContext context, Widget fullForm) {
    final c = MacColors.of(context);
    return ValueListenableBuilder<MacAppearanceState>(
        valueListenable: MacAppearance.state,
        builder: (_, appearance, __) => ValueListenableBuilder<
                TextEditingValue>(
            valueListenable: _urlController,
            builder: (context, input, _) => Obx(() {
                  final expanded = controller.showAdvanced.value;
                  final taskList = Get.isRegistered<MacTaskListController>()
                      ? Get.find<MacTaskListController>()
                      : null;
                  final allTasks = taskList?.tasks.toList() ?? <Task>[];
                  final completed =
                      allTasks.where((t) => t.status == Status.done).length;
                  final active =
                      allTasks.where((t) => t.status != Status.done).length;
                  final runningCount = allTasks
                      .where((t) =>
                          t.status == Status.running &&
                          t.handoff?.blocked != true)
                      .length;
                  final showCompleted = _panelShowCompleted.value;
                  final recent =
                      MacPanel.taskResult.value?.taskIds ?? const <String>[];
                  final tasks = <Task>[
                    if (!showCompleted)
                      for (final id in recent)
                        ...allTasks.where((t) => t.id == id),
                    ...allTasks.where((t) =>
                        (showCompleted || !recent.contains(t.id)) &&
                        (t.status == Status.done) == showCompleted),
                  ].take(3).toList();
                  final error = _panelError.value;
                  final manualVisible =
                      _manualBatch != null || _manualNotice.value != null;
                  final hasInput = input.text.trim().isNotEmpty ||
                      controller.fileDataUri.isNotEmpty ||
                      _netdiskRequest.value != null ||
                      Get.find<AppController>().pendingUpdateTask.value != null;
                  final painter = TextPainter(
                      text: TextSpan(
                          text: input.text.isEmpty ? ' ' : input.text,
                          style: const TextStyle(
                              fontFamily: '.AppleSystemUIFont',
                              fontSize: 20,
                              height: 1.2)),
                      textDirection: Directionality.of(context),
                      maxLines: 3)
                    ..layout(maxWidth: input.text.isEmpty ? 430 : 402);
                  final inputHeight = 68.0 +
                      (painter.computeLineMetrics().length.clamp(1, 3) - 1) *
                          24;
                  painter.dispose();
                  WidgetsBinding.instance.addPostFrameCallback((_) {
                    if (mounted &&
                        !_netdiskOpen.value &&
                        _onboarding == null &&
                        Get.isDialogOpen != true) {
                      MacPanel.resize(expanded
                          ? 700
                          : inputHeight +
                              76 +
                              (tasks.isEmpty ? 70 : tasks.length * 58) +
                              (error == null ? 0 : 44) +
                              (manualVisible ? 72 : 0));
                    }
                  });
                  return CallbackShortcuts(
                    bindings: {
                      const SingleActivator(LogicalKeyboardKey.comma,
                          meta: true): () => MacPanel.showSettings(),
                      const SingleActivator(LogicalKeyboardKey.escape): () {
                        if (!_urlController.value.composing.isValid ||
                            _urlController.value.composing.isCollapsed) {
                          _cancelCreate();
                        }
                      },
                    },
                    child: Material(
                        color: appearance.reduceTransparency
                            ? c.background
                            : c.panelTint,
                        child: Column(children: [
                          if (expanded)
                            Padding(
                                padding:
                                    const EdgeInsets.fromLTRB(20, 8, 12, 0),
                                child: Row(children: [
                                  Text('下载选项',
                                      style: TextStyle(
                                          fontSize: 13, color: c.secondary)),
                                  const Spacer(),
                                  _panelIcon(c, CupertinoIcons.gear, '设置',
                                      () => MacPanel.showSettings()),
                                ]))
                          else
                            SizedBox(
                                height: inputHeight,
                                child: Padding(
                                    padding: const EdgeInsets.symmetric(
                                        horizontal: 20),
                                    child: Row(children: [
                                      MacSymbol('link',
                                          fallback: CupertinoIcons.link,
                                          size: 18,
                                          color: c.secondary),
                                      const SizedBox(width: 12),
                                      Expanded(
                                          child: DropTarget(
                                              onDragDone: (details) {
                                                if (details.files.isNotEmpty) {
                                                  _urlController.text =
                                                      details.files.first.path;
                                                  _originalRequest = null;
                                                  MacPanel.cancelAutoHide();
                                                }
                                              },
                                              child: Form(
                                                  key: _confirmFormKey,
                                                  child: TextFormField(
                                                    key: const ValueKey(
                                                        'panel-download-input'),
                                                    controller: _urlController,
                                                    focusNode: _inputFocus,
                                                    autofocus: true,
                                                    minLines: 1,
                                                    maxLines: 3,
                                                    textInputAction:
                                                        TextInputAction.done,
                                                    style: TextStyle(
                                                        fontFamily:
                                                            '.AppleSystemUIFont',
                                                        fontSize: 20,
                                                        height: 1.2,
                                                        fontWeight:
                                                            FontWeight.w400,
                                                        color: c.text),
                                                    cursorColor: c.accent,
                                                    decoration: InputDecoration(
                                                      hintText: '粘贴下载链接…',
                                                      hintStyle: TextStyle(
                                                          fontFamily:
                                                              '.AppleSystemUIFont',
                                                          fontSize: 20,
                                                          color: c.secondary),
                                                      filled: false,
                                                      isDense: true,
                                                      contentPadding:
                                                          const EdgeInsets
                                                              .symmetric(
                                                              vertical: 8),
                                                      border: InputBorder.none,
                                                      enabledBorder:
                                                          InputBorder.none,
                                                      focusedBorder:
                                                          InputBorder.none,
                                                      errorBorder:
                                                          InputBorder.none,
                                                      focusedErrorBorder:
                                                          InputBorder.none,
                                                    ),
                                                    validator: (v) =>
                                                        v?.trim().isNotEmpty ==
                                                                true
                                                            ? null
                                                            : '请填写下载链接或选择种子',
                                                    onFieldSubmitted: (_) =>
                                                        _doConfirm(),
                                                    onChanged: (value) {
                                                      MacPanel.cancelAutoHide();
                                                      _clearCapturedHeadersFor(
                                                          value);
                                                      _originalRequest = null;
                                                      _netdiskRequest.value =
                                                          null;
                                                      _panelError.value = null;
                                                      controller
                                                          .clearFileDataUri();
                                                      recognizeMagnetUri(value);
                                                    },
                                                  )))),
                                      const SizedBox(width: 8),
                                      if (input.text.isNotEmpty)
                                        _panelIcon(
                                            c,
                                            CupertinoIcons.xmark_circle_fill,
                                            '清空', () {
                                          _urlController.clear();
                                          controller.clearFileDataUri();
                                          _inputFocus.requestFocus();
                                        }),
                                      const SizedBox(width: 8),
                                      Tooltip(
                                          message: '开始下载',
                                          child: MacButton(
                                              primary: hasInput,
                                              onPressed: hasInput &&
                                                      !controller
                                                          .isConfirming.value
                                                  ? _doConfirm
                                                  : null,
                                              child: controller
                                                      .isConfirming.value
                                                  ? const CupertinoActivityIndicator(
                                                      radius: 7)
                                                  : Row(
                                                      mainAxisSize:
                                                          MainAxisSize.min,
                                                      children: [
                                                          const Text('下载',
                                                              style: TextStyle(
                                                                  fontSize:
                                                                      12)),
                                                          const SizedBox(
                                                              width: 8),
                                                          ExcludeSemantics(
                                                              child: Icon(
                                                                  CupertinoIcons
                                                                      .return_icon,
                                                                  size: 11,
                                                                  color: hasInput
                                                                      ? c.primaryText.withValues(
                                                                          alpha:
                                                                              .75)
                                                                      : c.secondary.withValues(
                                                                          alpha:
                                                                              .5))),
                                                        ]))),
                                      const SizedBox(width: 8),
                                      _panelIcon(c, CupertinoIcons.gear, '设置',
                                          () => MacPanel.showSettings()),
                                    ]))),
                          if (!expanded) ...[
                            Divider(color: c.border, height: 1, thickness: .5),
                            SizedBox(
                                height: 36,
                                child: Padding(
                                    padding: const EdgeInsets.symmetric(
                                        horizontal: 14),
                                    child: Row(children: [
                                      _panelFilter(
                                          c,
                                          '未完成',
                                          active,
                                          !showCompleted,
                                          () => _panelShowCompleted.value =
                                              false),
                                      const SizedBox(width: 6),
                                      _panelFilter(
                                          c,
                                          '已完成',
                                          completed,
                                          showCompleted,
                                          () =>
                                              _panelShowCompleted.value = true),
                                      const Spacer(),
                                      _panelIcon(
                                          c,
                                          CupertinoIcons.doc_on_clipboard,
                                          '粘贴', () async {
                                        final value = await Clipboard.getData(
                                            'text/plain');
                                        if (!mounted) return;
                                        _urlController.text = value?.text ?? '';
                                        _originalRequest = null;
                                        MacPanel.cancelAutoHide();
                                        _inputFocus.requestFocus();
                                      }),
                                      _panelIcon(
                                          c, CupertinoIcons.folder, '选择种子',
                                          () async {
                                        final paths =
                                            await MacPanel.chooseFiles(
                                                extensions: const ['torrent']);
                                        if (mounted &&
                                            paths != null &&
                                            paths.isNotEmpty) {
                                          _urlController.text = paths.first;
                                          _originalRequest = null;
                                          MacPanel.cancelAutoHide();
                                          _inputFocus.requestFocus();
                                        }
                                      }),
                                      _panelAction(
                                          c,
                                          CupertinoIcons.slider_horizontal_3,
                                          '更多选项',
                                          () => controller.showAdvanced.value =
                                              true),
                                    ]))),
                          ],
                          _manualBatchStatus(),
                          if (error != null)
                            SizedBox(
                                height: 44,
                                child: Padding(
                                    padding: const EdgeInsets.symmetric(
                                        horizontal: 20, vertical: 6),
                                    child: Align(
                                        alignment: Alignment.centerLeft,
                                        child: Text(error,
                                            maxLines: 2,
                                            overflow: TextOverflow.ellipsis,
                                            style: const TextStyle(
                                                fontSize: 12,
                                                color: CupertinoColors
                                                    .systemRed))))),
                          if (expanded)
                            Expanded(child: fullForm)
                          else ...[
                            Expanded(
                                child: tasks.isEmpty
                                    ? Center(
                                        child: Text(
                                            showCompleted
                                                ? '暂无已完成的下载'
                                                : '暂无未完成的下载',
                                            style: TextStyle(
                                                fontSize: 12,
                                                color: c.secondary)))
                                    : ListView(
                                        padding: const EdgeInsets.symmetric(
                                            horizontal: 20),
                                        children: [
                                            for (final task in tasks)
                                              _compactTask(c, task, taskList!)
                                          ])),
                            Divider(color: c.border, height: 1, thickness: .5),
                            SizedBox(
                                height: 36,
                                child: Padding(
                                    padding: const EdgeInsets.symmetric(
                                        horizontal: 20),
                                    child: Row(children: [
                                      Expanded(
                                          child: ValueListenableBuilder<
                                                  String?>(
                                              valueListenable: MacPanel.notice,
                                              builder: (_, message, __) => Text(
                                                  message ??
                                                      (runningCount > 0
                                                          ? '$runningCount 个任务运行中'
                                                          : active > 0
                                                              ? '当前无运行任务'
                                                              : '准备下载'),
                                                  maxLines: 1,
                                                  overflow:
                                                      TextOverflow.ellipsis,
                                                  style: TextStyle(
                                                      fontSize: 11,
                                                      color: message == null
                                                          ? c.secondary
                                                          : c.accent)))),
                                      ValueListenableBuilder<String>(
                                          valueListenable: MacPanel.hotKeyLabel,
                                          builder: (_, label, __) => Text(
                                              '$label 唤起 · esc 收起',
                                              style: TextStyle(
                                                  fontSize: 11,
                                                  color: c.secondary))),
                                      const SizedBox(width: 10),
                                      _panelAction(
                                          c,
                                          CupertinoIcons.list_bullet,
                                          '全部下载',
                                          () => MacPanel.showSettings(
                                              section: 'tasks')),
                                      const SizedBox(width: 6),
                                    ]))),
                          ],
                        ])),
                  );
                })));
  }

  Widget _panelIcon(
          MacColors c, IconData icon, String label, VoidCallback action) =>
      Tooltip(
          message: label,
          child: CupertinoButton(
              padding: const EdgeInsets.all(6),
              minimumSize: const Size(28, 28),
              onPressed: action,
              child: icon == CupertinoIcons.gear
                  ? MacSymbol('gearshape',
                      fallback: icon, size: 15, color: c.secondary)
                  : Icon(icon, size: 15, color: c.secondary)));

  Widget _panelFilter(MacColors c, String label, int count, bool selected,
          VoidCallback action) =>
      CupertinoButton(
          padding: EdgeInsets.zero,
          minimumSize: const Size(0, 26),
          onPressed: action,
          child: Container(
              padding: const EdgeInsets.symmetric(horizontal: 10, vertical: 5),
              decoration: BoxDecoration(
                  color: selected
                      ? c.text.withValues(alpha: .065)
                      : Colors.transparent,
                  borderRadius: BorderRadius.circular(12)),
              child: Text.rich(
                  TextSpan(children: [
                    TextSpan(
                        text: label,
                        style: TextStyle(
                            fontWeight:
                                selected ? FontWeight.w500 : FontWeight.w400,
                            color: selected ? c.text : c.secondary)),
                    TextSpan(
                        text: ' $count', style: TextStyle(color: c.secondary)),
                  ]),
                  style: const TextStyle(fontSize: 11.5))));

  Widget _panelAction(
          MacColors c, IconData icon, String label, VoidCallback action) =>
      CupertinoButton(
          padding: const EdgeInsets.symmetric(horizontal: 6, vertical: 5),
          minimumSize: const Size(0, 28),
          onPressed: action,
          child: Row(mainAxisSize: MainAxisSize.min, children: [
            Icon(icon, size: 13, color: c.secondary),
            const SizedBox(width: 4),
            Text(label, style: TextStyle(fontSize: 11, color: c.secondary)),
          ]));

  Widget _compactTask(MacColors c, Task task, MacTaskListController list) {
    final running = task.status == Status.running;
    final failed = task.status == Status.error;
    final done = task.status == Status.done;
    final completion = HttpCompletionView.forTask(task);
    final total = task.meta.res?.size ?? 0;
    final progress =
        total > 0 ? (task.progress.downloaded / total).clamp(0.0, 1.0) : 0.0;
    final extension = path.extension(task.name).toLowerCase();
    final icon = ['.mp4', '.mkv', '.mov', '.webm'].contains(extension)
        ? CupertinoIcons.film
        : ['.zip', '.rar', '.7z', '.gz'].contains(extension)
            ? CupertinoIcons.archivebox
            : ['.jpg', '.png', '.heic', '.gif'].contains(extension)
                ? CupertinoIcons.photo
                : CupertinoIcons.doc;
    return SizedBox(
        height: 58,
        child: Row(children: [
          SizedBox(width: 28, child: Icon(icon, size: 24, color: c.secondary)),
          const SizedBox(width: 12),
          Expanded(
              child: Column(
                  mainAxisAlignment: MainAxisAlignment.center,
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                Text(task.name,
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: TextStyle(
                        fontSize: 14,
                        fontWeight: FontWeight.w400,
                        color: c.text)),
                const SizedBox(height: 3),
                if (failed && completion == null)
                  MacTaskFailure(
                      key: ValueKey('failure-${task.id}'),
                      taskId: task.id,
                      style: const TextStyle(
                          fontSize: 11, color: CupertinoColors.systemRed))
                else
                  Text(
                      task.handoff?.blocked == true
                          ? task.handoff!.label
                          : completion != null
                              ? completion.labelFor(task.status)
                              : failed
                                  ? '下载失败 · 查看详情'
                                  : done
                                      ? '已完成 · ${Util.fmtByte(task.progress.downloaded)}'
                                      : task.fetchingTorrentInfo
                                          ? '正在获取种子信息'
                                          : running
                                              ? (total > 0
                                                  ? '${Util.fmtByte(task.progress.downloaded)} / ${Util.fmtByte(total)}'
                                                  : Util.fmtByte(
                                                      task.progress.downloaded))
                                              : task.status == Status.pause
                                                  ? '已暂停'
                                                  : '等待下载',
                      maxLines: 1,
                      overflow: TextOverflow.ellipsis,
                      style: TextStyle(
                          fontSize: 11,
                          color: failed
                              ? CupertinoColors.systemRed
                              : c.secondary)),
                if (!done &&
                    task.handoff?.blocked != true &&
                    (running || task.progress.downloaded > 0)) ...[
                  const SizedBox(height: 5),
                  LinearProgressIndicator(
                      minHeight: 2,
                      borderRadius: BorderRadius.circular(1),
                      value: progress,
                      backgroundColor: c.text.withValues(alpha: .06),
                      color: c.accent),
                ],
              ])),
          const SizedBox(width: 12),
          SizedBox(
              width: 100,
              child: Column(
                  mainAxisAlignment: MainAxisAlignment.center,
                  crossAxisAlignment: CrossAxisAlignment.end,
                  children: [
                    Text(
                        running ? '${Util.fmtByte(task.progress.speed)}/s' : '',
                        maxLines: 1,
                        style: TextStyle(
                            fontSize: 12,
                            color: c.text,
                            fontFeatures: const [
                              FontFeature.tabularFigures()
                            ])),
                    if (running &&
                        total > task.progress.downloaded &&
                        task.progress.speed > 0)
                      Text(
                          '${((total - task.progress.downloaded) / task.progress.speed / 60).ceil()} 分钟剩余',
                          maxLines: 1,
                          style: TextStyle(fontSize: 11, color: c.secondary)),
                  ])),
          SizedBox(
              width: 56,
              child: Row(mainAxisAlignment: MainAxisAlignment.end, children: [
                if (failed)
                  _panelIcon(c, CupertinoIcons.info_circle, '查看错误',
                      () => _showTaskError(task)),
                if (task.handoff?.blocked == true)
                  _panelIcon(c, CupertinoIcons.arrow_clockwise, '检查交接',
                      () async {
                    try {
                      final result = await Get.find<AppController>()
                          .checkBrowserHandoff(task);
                      MacPanel.notice.value = result.message;
                      await list.getTasksState();
                    } catch (e) {
                      _panelError.value = e.toString();
                    }
                  })
                else if (done)
                  Icon(
                      completion == null ||
                              completion.isConfirmedDoneFor(task.status)
                          ? CupertinoIcons.checkmark_circle
                          : CupertinoIcons.question_circle,
                      size: 17,
                      color: c.secondary)
                else
                  _panelIcon(
                      c,
                      running ? CupertinoIcons.pause : CupertinoIcons.play,
                      running ? '暂停' : '继续', () async {
                    try {
                      running
                          ? await pauseTask(task.id)
                          : await continueTask(task.id);
                      await list.getTasksState();
                    } catch (e) {
                      _panelError.value = e.toString();
                    }
                  }),
              ])),
        ]));
  }

  Future<void> _showTaskError(Task task) async {
    var reason = '下载失败。可重试；网盘任务可在任务管理中重新同步登录。';
    var hasReason = false;
    try {
      final value = (await getTaskStats(task.id)).lastError;
      if (value != null && value.trim().isNotEmpty) {
        reason = displayTaskFailure(value);
        hasReason = true;
      }
    } catch (_) {}
    if (!hasReason)
      try {
        final file = File(path.join(logsDir(), 'core.log'));
        final handle = await file.open();
        try {
          final length = await handle.length();
          await handle.setPosition(length > 131072 ? length - 131072 : 0);
          final lines = utf8
              .decode(await handle.read(131072), allowMalformed: true)
              .split('\n');
          for (final line in lines.reversed) {
            if (!line.contains(task.id)) continue;
            try {
              final entry = jsonDecode(line);
              if (entry is Map && entry['error'] is String) {
                reason = (entry['error'] as String)
                    .replaceAll(RegExp(r'https?://\S+'), '[下载地址]')
                    .replaceAll(RegExp(r'Bearer\s+\S+', caseSensitive: false),
                        'Bearer …');
                break;
              }
            } catch (_) {}
          }
        } finally {
          await handle.close();
        }
      } catch (_) {}
    if (!mounted) return;
    MacPanel.resize(380);
    await showDialog<void>(
        context: context,
        builder: (dialogContext) => AlertDialog(
              title:
                  Text(task.name, maxLines: 1, overflow: TextOverflow.ellipsis),
              content: SelectableText(reason),
              actions: [
                TextButton(
                    onPressed: () => Navigator.of(dialogContext).pop(),
                    child: const Text('关闭'))
              ],
            ));
    if (mounted) setState(() {});
  }

  // parse protocol from url
  parseProtocol(String url) {
    if (netdiskProviderForUrl(url.trim()) != null) return Protocol.netdisk;
    final uppercaseUrl = url.toUpperCase();
    Protocol? protocol;
    if (uppercaseUrl.startsWith("HTTP:") || uppercaseUrl.startsWith("HTTPS:")) {
      protocol = Protocol.http;
    }
    if (uppercaseUrl.startsWith("MAGNET:") ||
        uppercaseUrl.endsWith(".TORRENT")) {
      protocol = Protocol.bt;
    }
    if (uppercaseUrl.startsWith("ED2K:")) {
      protocol = Protocol.ed2k;
    }
    return protocol;
  }

  // recognize magnet uri, if length == 40, auto add magnet prefix
  recognizeMagnetUri(String text) {
    if (text.length != 40) {
      return;
    }
    final exp = RegExp(r"^[0-9a-fA-F]{40}$");
    if (exp.hasMatch(text)) {
      final uri = "magnet:?xt=urn:btih:$text";
      _urlController.text = uri;
      _urlController.selection = TextSelection.fromPosition(
          TextPosition(offset: _urlController.text.length));
    }
  }

  void _syncDefaultDownloadPath() {
    final nextDefault = renderPathPlaceholders(
        Get.find<AppController>().downloaderConfig.value.downloadDir);
    // A persistent panel follows Settings only while its draft uses the default.
    // Native/browser inputs and the directory picker may supply a custom path.
    if (_pathController.text.isEmpty ||
        _pathController.text == _lastDefaultDownloadPath) {
      _pathController.text = nextDefault;
    }
    _lastDefaultDownloadPath = nextDefault;
  }

  Future<bool> _ensureMacOnboarding() async {
    if (!Util.isMacos()) return true;
    final pending = _onboarding;
    if (pending != null) return pending;
    _syncDefaultDownloadPath();
    final operation = MacOnboarding.show(context);
    _onboarding = operation;
    try {
      return await operation;
    } finally {
      if (mounted) _syncDefaultDownloadPath();
      _onboarding = null;
    }
  }

  bool _checksumInputBlocked() {
    String? expected;
    try {
      expected = normalizedExpectedSha256(_expectedSha256Controller.text);
    } on FormatException catch (error) {
      _manualNotice.value = error.message;
      return true;
    }
    if (expected == null) return false;
    final input = _urlController.text.trim();
    final urls = Util.textToLines(input);
    if (urls.length != 1 ||
        parseProtocol(urls.single) != Protocol.http ||
        !const {'http', 'https'}.contains(Uri.tryParse(urls.single)?.scheme) ||
        (Uri.tryParse(urls.single)?.host.isEmpty ?? true) ||
        _panelRequest != null ||
        _originalRequest != null ||
        _netdiskRequest.value != null ||
        Get.find<AppController>().pendingUpdateTask.value != null) {
      _manualNotice.value = 'SHA-256 校验目前仅支持手动单链接 HTTP(S)，未提交任务。';
    } else if (_expectedSha256Url != input) {
      _manualNotice.value = '链接已变化，请重新填写当前链接的 SHA-256，未提交任务。';
    } else {
      return false;
    }
    return true;
  }

  Future<void> _doConfirm() async {
    if (controller.isConfirming.value) {
      return;
    }
    if (_checksumInputBlocked()) return;
    final confirmation = ++_confirmationEpoch;
    controller.isConfirming.value = true;
    _confirmingCapture = _panelRequest?.browserCapture == true
        ? _panelRequest?.browserOperationId
        : null;
    _captureCompletedDuringConfirm = false;
    _createdIds.clear();
    _panelError.value = null;
    MacPanel.cancelAutoHide();
    try {
      if (Util.isMacos()) _syncDefaultDownloadPath();
      _confirmController.start();
      final hasInput = _urlController.text.trim().isNotEmpty ||
          controller.fileDataUri.isNotEmpty ||
          _netdiskRequest.value != null ||
          Get.find<AppController>().pendingUpdateTask.value != null;
      if (hasInput && (!await _ensureMacOnboarding() || !mounted)) return;
      _checkCaptureCompletion();
      final handoffRequest = _panelRequest;
      if (handoffRequest?.browserHandoff != null &&
          handoffRequest?.req?.url.trim() == _urlController.text.trim()) {
        final prepared = await Get.find<AppController>().prepareBrowserHandoff(
            CreateTask(
                req: _requestForUrl(_urlController.text.trim()),
                opts: Options(
                    name: _renameController.text,
                    path: _pathController.text,
                    selectFiles: [],
                    extra: parseReqOptsExtra()))
              ..browserHandoff = handoffRequest!.browserHandoff
              ..browserOperationId = handoffRequest.browserOperationId
              ..browserCapture = true);
        _checkCaptureCompletion();
        if (prepared.state == 'prepared' || prepared.committed) {
          _clearBrowserDraft();
          await MacPanel.showTaskResult(prepared.taskIds, null);
          MacPanel.notice.value = prepared.message;
          if (Get.isRegistered<MacTaskListController>())
            await Get.find<MacTaskListController>().getTasksState();
        } else if (prepared.state == 'aborted') {
          _finishBrowserCapture(
              handoffRequest.browserOperationId!, prepared.message);
        } else {
          throw StateError(prepared.message);
        }
        return;
      }
      // An existing netdisk task owns its file/source, including transferred
      // copies. Browser/native input must never turn recovery into a new batch.
      if (await _showPendingNetdiskTask()) return;
      if (_confirmFormKey.currentState!.validate()) {
        final isWebFileChosen =
            Util.isWeb() && controller.fileDataUri.isNotEmpty;
        var submitUrl = isWebFileChosen
            ? controller.fileDataUri.value
            : _urlController.text.trim();

        // Intercept before history, extension resolve, pending HTTP updates,
        // direct download, and the multiline shortcut.
        if (_netdiskRequest.value != null) {
          if (await _showNetdisk(
              NetdiskInput.fromRequest(_netdiskRequest.value!))) {
            _netdiskRequest.value = null;
            _completeCreate();
          }
          return;
        }
        final netdiskInputs = parseNetdiskInputs(submitUrl);
        if (netdiskInputs.isNotEmpty) {
          final ordinary = ordinaryNetdiskCompanionLinks(submitUrl);
          for (var i = 0; i < netdiskInputs.length; i++) {
            if (!await _showNetdisk(netdiskInputs[i])) return;
            // Keep only pending inputs so a later cancel cannot duplicate a
            // batch already created earlier in this mixed submission.
            _urlController.text = [
              for (final input in netdiskInputs.skip(i + 1))
                '${input.source.url}${input.source.password.isEmpty ? '' : ' 提取码：${input.source.password}'}',
              ...ordinary,
            ].join('\n');
          }
          if (ordinary.isEmpty) {
            _completeCreate();
            return;
          }
          submitUrl = ordinary.join('\n');
        }

        final urls = Util.textToLines(submitUrl);

        // Check if there is a pending update task (only for single URL)
        if (urls.length == 1) {
          final appController = Get.find<AppController>();
          final pendingTask = appController.pendingUpdateTask.value;
          if (pendingTask != null) {
            final shouldUpdate = await _showPendingUpdateDialog(
                pendingTask.id, pendingTask.name);
            if (shouldUpdate == true) {
              // Update the pending task instead of creating a new one
              await _updatePendingTask(pendingTask.id, urls.first);
              return;
            } else if (shouldUpdate == null) {
              // User cancelled, don't create task either
              return;
            }
            // shouldUpdate == false, continue to create new task
          }
        }

        // Add url to the history
        if (!isWebFileChosen) {
          for (final url in urls) {
            Database.instance.saveCreateHistory(url);
          }
        }

        /*
        Check if is direct download, there has two ways to direct download
        1. Direct download option is checked
        2. Muli line urls
        */
        final expectedSha256 =
            normalizedExpectedSha256(_expectedSha256Controller.text);
        final isMultiLine = urls.length > 1;
        final isDirect = controller.directDownload.value || isMultiLine;
        final opt = Options(
          name: isMultiLine ? "" : _renameController.text,
          path: _pathController.text,
          selectFiles: [],
          extra: parseReqOptsExtra(),
        );
        if (Util.isMacos() &&
            netdiskInputs.isEmpty &&
            (isMultiLine || expectedSha256 != null || _manualBatch != null) &&
            _panelRequest == null &&
            _originalRequest == null &&
            _netdiskRequest.value == null &&
            Get.find<AppController>().pendingUpdateTask.value == null &&
            urls.every((url) => parseProtocol(url) == Protocol.http)) {
          final stop = Completer<void>();
          _manualConfirmationEpoch = confirmation;
          _manualConfirmationStop = stop;
          await Future.any<void>([
            _submitManualBatch(urls, opt, expectedSha256: expectedSha256),
            stop.future
          ]);
          return;
        }
        if (isDirect) {
          await Future.wait(urls.map((url) {
            return _createTask(CreateTask(
              req: _requestForUrl(url),
              opts: opt,
            ));
          }));
          _completeCreate();
        } else {
          final rr = await (widget.taskResolver ?? resolve)(ResolveTask(
            req: _requestForUrl(submitUrl),
            opts: opt,
          ));
          _checkCaptureCompletion();
          await _showResolveDialog(rr);
        }
      }
    } catch (e) {
      final error = _captureCompletedDuringConfirm
          ? BrowserCaptureEnded(
              Get.find<AppController>().browserCaptureCompletionMessage)
          : e;
      if (error is BrowserCaptureEnded &&
          _panelRequest?.browserCapture == true) {
        _finishBrowserCapture(
            _panelRequest!.browserOperationId!, error.message);
      }
      _panelError.value = error.toString();
      if (!Util.isMacos()) showErrorMessage(error);
    } finally {
      if (_manualConfirmationEpoch == confirmation) {
        _manualConfirmationEpoch = null;
        _manualConfirmationStop = null;
      }
      if (confirmation == _confirmationEpoch) _releaseConfirmation();
    }
  }

  /// Returns true when this is a netdisk recovery, including cancellation.
  Future<bool> _showPendingNetdiskTask() async {
    final appController = Get.find<AppController>();
    final pending = appController.pendingUpdateTask.value;
    if (pending == null) return false;
    final api = netdiskApi ?? NetdiskApi();
    final task = await api.getTask(pending.id);
    if (task.meta.req.protocol != 'netdisk') return false;
    final resumed = await _showNetdisk(NetdiskInput.fromRequest(task.meta.req),
        options: Options.fromJson(task.meta.opts.toJson()),
        proxy: task.meta.req.proxy,
        onResume: () => api.continueTask(task.id));
    if (resumed) {
      if (appController.pendingUpdateTask.value?.id == task.id) {
        appController.pendingUpdateTask.value = null;
      }
      _netdiskRequest.value = null;
      _completeCreate();
    }
    return true;
  }

  Future<bool> _showNetdisk(NetdiskInput input,
      {Options? options,
      RequestProxy? proxy,
      Future<void> Function()? onResume}) async {
    if (_netdiskOpen.value) return false;
    _netdiskOpen.value = true;
    if (Util.isMacos()) MacPanel.resize(700);
    try {
      final opts = options ?? Options(extra: parseReqOptsExtra());
      if (opts.path.isEmpty) opts.path = _pathController.text;
      return await showDialog<bool>(
            context: Get.context!,
            barrierDismissible: false,
            builder: (_) => NetdiskView(
                input: input,
                options: opts,
                proxy: onResume != null
                    ? proxy
                    : _netdiskRequest.value?.proxy ?? parseProxy(),
                api: netdiskApi,
                browserTool: netdiskBrowserTool,
                onResume: onResume),
          ) ==
          true;
    } finally {
      _netdiskOpen.value = false;
      if (mounted) setState(() {});
    }
  }

  /// Shows a dialog to ask if user wants to update pending task or create new
  /// Returns true to update, false to create new, null if cancelled
  Future<bool?> _showPendingUpdateDialog(String taskId, String taskName) async {
    return showDialog<bool>(
      context: Get.context!,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: Text('pendingUpdateFound'.tr),
        content: Text('pendingUpdateConfirm'.trParams({'name': taskName})),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(null),
            child: Text('cancel'.tr),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: Text('pendingUpdateNo'.tr),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: Text('pendingUpdateYes'.tr),
          ),
        ],
      ),
    );
  }

  /// Updates the pending task with new URL and headers
  Future<void> _updatePendingTask(String taskId, String newUrl) async {
    try {
      // Build headers from current form
      final headers = <String, String>{};
      for (final c in _httpHeaderControllers) {
        final key = c.name.text.trim();
        final value = c.value.text.trim();
        if (key.isNotEmpty) {
          headers[key] = value;
        }
      }

      // Build ReqExtraHttp
      final reqExtra = ReqExtraHttp(header: headers);

      // Create patch request
      final patchData = ResolveTask(
        req: Request(
          url: newUrl,
          extra: reqExtra.toJson(),
          proxy: parseProxy(),
          skipVerifyCert: _skipVerifyCertController.value,
        ),
      );

      await patchTask(taskId, patchData);
      await continueTask(taskId);

      // Clear pending update state
      final appController = Get.find<AppController>();
      appController.pendingUpdateTask.value = null;

      _completeCreate();
    } catch (e) {
      showErrorMessage(e);
    }
  }

  RequestProxy? parseProxy() {
    if (controller.proxyConfig.value?.mode == RequestProxyMode.custom) {
      return RequestProxy()
        ..mode = RequestProxyMode.custom
        ..scheme = controller.proxyConfig.value?.scheme ?? 'http'
        ..host = "${_proxyIpController.text}:${_proxyPortController.text}"
        ..usr = _proxyUsrController.text
        ..pwd = _proxyPwdController.text;
    }
    return controller.proxyConfig.value;
  }

  Object? parseReqExtra(String url) {
    Object? reqExtra;
    final protocol = parseProtocol(url);
    switch (protocol) {
      case Protocol.http:
        final header = Map<String, String>.fromEntries(_httpHeaderControllers
            .map((e) => MapEntry(e.name.text, e.value.text)));
        header.removeWhere(
            (key, value) => key.trim().isEmpty || value.trim().isEmpty);
        if (header.isNotEmpty) {
          reqExtra = ReqExtraHttp()..header = header;
        }
        break;
      case Protocol.bt:
        if (_btTrackerController.text.trim().isNotEmpty) {
          reqExtra = ReqExtraBt()
            ..trackers = Util.textToLines(_btTrackerController.text);
        }
        break;
      case Protocol.ed2k:
      case Protocol.netdisk:
      case Protocol.media:
      case null:
        break;
    }
    return reqExtra;
  }

  Object? parseReqOptsExtra() {
    return OptsExtraHttp()
      ..connections = int.tryParse(_connectionsController.text) ?? 0
      ..autoTorrent = _autoTorrentController.value
      ..deleteTorrentAfterDownload = _deleteTorrentAfterDownloadController.value
      ..autoExtract = _autoExtractController.value
      ..archivePassword = _archivePasswordController.text
      ..deleteAfterExtract = _deleteAfterExtractController.value ?? false;
  }

  String _hitText() {
    return 'downloadLinkHit'.trParams({
      'append':
          Util.isDesktop() || Util.isWeb() ? 'downloadLinkHitDesktop'.tr : '',
    });
  }

  Future<void> _showResolveDialog(ResolveResult rr) async {
    if (Util.isMacos() &&
        rr.res.files.length == 1 &&
        parseProtocol(_urlController.text.trim()) == Protocol.http) {
      final file = rr.res.files.single;
      if (rr.id.isNotEmpty) {
        await _createTask(CreateTask(
            rid: rr.id,
            opts: Options(
                name: _renameController.text,
                path: _pathController.text,
                selectFiles: [0],
                extra: parseReqOptsExtra())));
      } else if (file.req != null) {
        await _createTask(CreateTask(
            req: file.req,
            opts: Options(
                name: file.name,
                path: path.join(_pathController.text, rr.res.name, file.path),
                selectFiles: [],
                extra: parseReqOptsExtra())));
      } else {
        throw StateError('解析结果缺少下载来源');
      }
      await _completeCreate();
      return;
    }
    if (Util.isMacos()) MacPanel.resize(700);
    final createFormKey = GlobalKey<FormState>();
    final downloadController = RoundedLoadingButtonController();
    return showDialog<void>(
        context: Get.context!,
        barrierDismissible: false,
        builder: (dialogContext) {
          if (_confirmingCapture != null) {
            _captureResolveRoute = ModalRoute.of(dialogContext);
          }
          return AlertDialog(
            title: rr.res.name.isEmpty ? null : Text(rr.res.name),
            content: Builder(
              builder: (context) {
                // Get available height and width of the build area of this widget. Make a choice depending on the size.
                var height = MediaQuery.of(context).size.height;
                var width = MediaQuery.of(context).size.width;

                return SizedBox(
                  height: height * 0.75,
                  width: width,
                  child: Form(
                      key: createFormKey,
                      autovalidateMode: AutovalidateMode.always,
                      child: FileTreeView(
                        files: rr.res.files,
                        initialValues: rr.res.files.asMap().keys.toList(),
                        onSelectionChanged: (List<int> values) {
                          controller.selectedIndexes.value = values;
                        },
                      )),
                );
              },
            ),
            actions: [
              ConstrainedBox(
                constraints: BoxConstraints.tightFor(
                  width: Get.theme.buttonTheme.minWidth,
                  height: Get.theme.buttonTheme.height,
                ),
                child: ElevatedButton(
                  style: ElevatedButton.styleFrom(shape: const StadiumBorder())
                      .copyWith(
                          backgroundColor: MaterialStateProperty.all(
                              Get.theme.colorScheme.background)),
                  onPressed: () {
                    Get.back();
                  },
                  child: Text('cancel'.tr),
                ),
              ),
              ConstrainedBox(
                constraints: BoxConstraints.tightFor(
                  width: Get.theme.buttonTheme.minWidth,
                  height: Get.theme.buttonTheme.height,
                ),
                child: RoundedLoadingButton(
                    color: Get.theme.colorScheme.secondary,
                    onPressed: () async {
                      try {
                        downloadController.start();
                        if (controller.selectedIndexes.isEmpty) {
                          showMessage('tip'.tr, 'noFileSelected'.tr);
                          return;
                        }
                        final optExtra = parseReqOptsExtra();
                        if (createFormKey.currentState!.validate()) {
                          if (rr.id.isEmpty) {
                            // from extension resolve result
                            final reqs =
                                controller.selectedIndexes.map((index) {
                              final file = rr.res.files[index];
                              return CreateTaskBatchItem(
                                  req: file.req!..proxy = parseProxy(),
                                  opts: Options(
                                      name: file.name,
                                      path: path.join(_pathController.text,
                                          rr.res.name, file.path),
                                      selectFiles: [],
                                      extra: optExtra));
                            }).toList();
                            await _createBatch(CreateTaskBatch(reqs: reqs));
                          } else {
                            await _createTask(CreateTask(
                                rid: rr.id,
                                opts: Options(
                                    name: _renameController.text,
                                    path: _pathController.text,
                                    selectFiles: controller.selectedIndexes,
                                    extra: optExtra)));
                          }
                          Get.back();
                          _completeCreate();
                        }
                      } catch (e) {
                        showErrorMessage(e);
                      } finally {
                        downloadController.reset();
                      }
                    },
                    controller: downloadController,
                    child: Text(
                      'download'.tr,
                      // style: controller.selectedIndexes.isEmpty
                      //     ? Get.textTheme.disabled
                      //     : Get.textTheme.titleSmall
                    )),
              ),
            ],
          );
        }).whenComplete(() => _captureResolveRoute = null);
  }

  Widget _buildCategorySelector(AppController appController) {
    final categories =
        appController.downloaderConfig.value.extra.downloadCategories
            .where((c) => !c.isDeleted) // Filter out deleted categories
            .toList();
    if (categories.isEmpty) {
      return const SizedBox.shrink();
    }

    // Helper to get display name
    String getCategoryDisplayName(DownloadCategory category) {
      if (category.nameKey != null && category.nameKey!.isNotEmpty) {
        return category.nameKey!.tr;
      }
      return category.name;
    }

    return Padding(
      padding: const EdgeInsets.only(top: 8),
      child: Row(
        children: [
          Text(
            'selectCategory'.tr,
            style: TextStyle(
              color: Get.theme.hintColor,
              fontSize: 12,
            ),
          ),
          const SizedBox(width: 8),
          Expanded(
            child: SingleChildScrollView(
              scrollDirection: Axis.horizontal,
              child: Row(
                children: categories.map((category) {
                  return Padding(
                    padding: const EdgeInsets.only(right: 8),
                    child: OutlinedButton(
                      onPressed: () {
                        _pathController.text =
                            renderPathPlaceholders(category.path);
                      },
                      style: OutlinedButton.styleFrom(
                        padding: const EdgeInsets.symmetric(
                          horizontal: 12,
                          vertical: 4,
                        ),
                        minimumSize: Size.zero,
                      ),
                      child: Text(
                        getCategoryDisplayName(category),
                        style: const TextStyle(fontSize: 12),
                      ),
                    ),
                  );
                }).toList(),
              ),
            ),
          ),
        ],
      ),
    );
  }
}
