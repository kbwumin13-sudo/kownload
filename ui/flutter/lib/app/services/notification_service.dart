import 'dart:async';
import 'dart:io';
import 'dart:convert';

import 'package:dio/dio.dart';

import 'package:flutter/services.dart';
import 'package:flutter_local_notifications/flutter_local_notifications.dart';
import 'package:get/get.dart';
import 'package:path_provider/path_provider.dart';

import '../../api/api.dart' as api;
import '../../api/model/task_batch_catalog.dart';
import '../../api/model/verified_completion_event.dart';
import '../../api/verified_completion_events.dart';
import '../../api/model/task.dart';
import '../../util/util.dart';
import '../modules/app/controllers/app_controller.dart';

class NotificationService extends GetxService {
  final Future<List<Task>> Function(List<Status>) _loadTasks;
  final CompletionEventsOpener _openEvents;
  NotificationService(
      {Future<List<Task>> Function(List<Status>)? loadTasks,
      CompletionEventsOpener? openEvents})
      : _loadTasks = loadTasks ?? api.getTasks,
        _openEvents = openEvents ?? openVerifiedCompletionEvents;
  Timer? _timer;
  Worker? _configWorker;
  bool _closed = false, _polling = false, _opening = false, _closing = false;
  bool _streamStopped = false;
  api.ActiveApiSnapshot? _snapshot;
  VerifiedCompletionConnection? _connection;
  StreamSubscription<VerifiedCompletionEvent>? _events;
  CancelToken? _cancel;
  String? _dataset;
  int _streamEpoch = 0;
  final _currentTasks = <String, Task>{};
  final _notified = <String>{};
  final _pending = <String, VerifiedCompletionEvent>{};
  static const _maximumPermissions = 2048;
  final Map<String, Status> _previousStatus = {};

  final AppController appController = Get.find<AppController>();
  final FlutterLocalNotificationsPlugin flutterLocalNotificationsPlugin =
      FlutterLocalNotificationsPlugin();

  @override
  void onInit() {
    super.onInit();
    _initNotifications();
    _snapshot = api.activeApiSnapshot;
    api.activeApiSnapshotChanges.addListener(_apiChanged);
    _configWorker = ever(appController.downloaderConfig, (_) {
      if (!appController.downloaderConfig.value.extra.desktopNotification) {
        _disconnect();
      }
    });
    _startPolling();
  }

  Future<void> _initNotifications() async {
    const DarwinInitializationSettings initializationSettingsDarwin =
        DarwinInitializationSettings(
            requestAlertPermission: false,
            requestBadgePermission: false,
            requestSoundPermission: false);

    final LinuxInitializationSettings initializationSettingsLinux =
        LinuxInitializationSettings(
      defaultActionName: 'Open notification',
      defaultIcon: AssetsLinuxIcon('assets/icon/icon.png'),
    );

    String? windowsIconPath;
    try {
      if (Util.isWindows()) {
        final byteData = await rootBundle.load('assets/icon/icon.ico');
        final tempDir = await getTemporaryDirectory();
        final file = File('${tempDir.path}/notification_icon.ico');
        await file.writeAsBytes(byteData.buffer
            .asUint8List(byteData.offsetInBytes, byteData.lengthInBytes));
        windowsIconPath = file.path;
      }
    } catch (e) {
      // Ignore
    }

    final WindowsInitializationSettings initializationSettingsWindows =
        WindowsInitializationSettings(
      appName: 'kownload',
      appUserModelId: 'com.gopeed.gopeed',
      guid: '3c1bf3f4-3d91-4eaa-a33f-8705e71cf1ce', // unique guid
      iconPath: windowsIconPath,
    );

    final InitializationSettings initializationSettings =
        InitializationSettings(
      macOS: initializationSettingsDarwin,
      linux: initializationSettingsLinux,
      windows: initializationSettingsWindows,
    );

    await flutterLocalNotificationsPlugin.initialize(
      settings: initializationSettings,
    );
  }

  @override
  void onClose() {
    _closed = true;
    _timer?.cancel();
    _configWorker?.dispose();
    api.activeApiSnapshotChanges.removeListener(_apiChanged);
    _disconnect();
    super.onClose();
  }

  void _apiChanged() {
    _snapshot = api.activeApiSnapshot;
    _streamStopped = false;
    _currentTasks.clear();
    _previousStatus.clear();
    _disconnect();
  }

  void _disconnect() {
    ++_streamEpoch;
    _dataset = null;
    _notified.clear();
    _pending.clear();
    _cancel?.cancel('completion_stream_closed');
    _cancel = null;
    final subscription = _events;
    final connection = _connection;
    _events = null;
    _connection = null;
    if (subscription != null || connection != null) {
      _closing = true;
      unawaited(() async {
        try {
          try {
            await subscription?.cancel();
          } finally {
            await connection?.close();
          }
          _closing = false;
        } catch (_) {
          // Keep the fence: an unconfirmed close cannot permit another socket.
        }
      }());
    }
    // _opening remains true until the real opener settles and its late
    // connection has closed. API changes/cancel cannot release this fence.
  }

  bool _current(api.ActiveApiSnapshot source, int epoch) =>
      !_closed &&
      identical(_snapshot, source) &&
      source.isCurrent &&
      epoch == _streamEpoch &&
      appController.downloaderConfig.value.extra.desktopNotification;

  Future<void> _ensureStream(api.ActiveApiSnapshot source) async {
    if (_closed ||
        _streamStopped ||
        _opening ||
        _closing ||
        _connection != null ||
        !source.isCurrent ||
        source.taskBatchRoute?.supported != true) {
      return;
    }
    _opening = true;
    final epoch = _streamEpoch;
    final cancel = CancelToken();
    _cancel = cancel;
    try {
      final response = await api.scopedTaskBatchRequest(
          source, 'GET', '/api/v1/dataset/identity',
          cancelToken: cancel);
      if (!_current(source, epoch)) return;
      if (const [401, 403, 404, 405, 409, 410, 501]
          .contains(response.statusCode)) {
        _streamStopped = true;
        return;
      }
      final body = response.data;
      if (response.statusCode != 200) {
        throw const CompletionEventsException('identity_unavailable',
            terminal: false);
      }
      if (body is! Map ||
          body['code'] is! int ||
          body['code'] != 0 ||
          body['data'] is! Map) {
        throw const CompletionEventsException('identity_protocol',
            terminal: true);
      }
      late DatasetIdentity identity;
      try {
        identity =
            DatasetIdentity.fromJson(Map<String, dynamic>.from(body['data']));
      } catch (_) {
        throw const CompletionEventsException('identity_protocol',
            terminal: true);
      }
      if (!identity.supported) {
        throw const CompletionEventsException('identity_protocol',
            terminal: true);
      }
      final connection = await _openEvents(source, identity.datasetId, cancel);
      if (!_current(source, epoch)) {
        try {
          await connection.close();
        } catch (_) {
          _closing = true;
        }
        return;
      }
      _dataset = identity.datasetId;
      _connection = connection;
      _events = connection.events.listen(
          (event) => _accepted(source, epoch, event), onError: (Object error) {
        if (!_current(source, epoch)) return;
        if (error is CompletionEventsException && error.terminal) {
          _streamStopped = true;
        }
        _disconnect();
      }, onDone: () {
        if (_current(source, epoch)) _disconnect();
      });
    } catch (error) {
      if (_current(source, epoch)) {
        if (error is CompletionEventsException && error.terminal) {
          _streamStopped = true;
        }
        if (error is DioException &&
            const [401, 403, 404, 405, 409, 410, 501]
                .contains(error.response?.statusCode)) {
          _streamStopped = true;
        }
      }
      cancel.cancel('completion_open_failed');
    } finally {
      // Do not free an in-flight opener just because its API epoch changed.
      _opening = false;
    }
  }

  void _accepted(
      api.ActiveApiSnapshot source, int epoch, VerifiedCompletionEvent event) {
    if (!_current(source, epoch)) {
      if (!_closed &&
          identical(_snapshot, source) &&
          epoch == _streamEpoch &&
          !appController.downloaderConfig.value.extra.desktopNotification) {
        _disconnect();
      }
      return;
    }
    if (event.datasetId != _dataset) return;
    final key = jsonEncode([event.datasetId, event.taskId, event.completionId]);
    if (_notified.contains(key)) return;
    if (!_pending.containsKey(key)) {
      if (_notified.length + _pending.length >= _maximumPermissions) return;
      _pending[key] = event;
    }
    _consumePending(source, epoch, key);
  }

  void _consumePending(api.ActiveApiSnapshot source, int epoch, String key) {
    if (!_current(source, epoch)) return;
    final event = _pending[key];
    if (event == null || event.datasetId != _dataset) return;
    final task = _currentTasks[event.taskId];
    // A live event can precede the next task poll. Retain its permission only
    // within this stream; a historical projection alone cannot create one.
    if (task == null) return;
    final view = task.completionView;
    if (task.protocol != Protocol.http ||
        view == null ||
        !view.available ||
        view.trustedErrorCategory != null ||
        view.datasetId != event.datasetId ||
        view.taskId != event.taskId ||
        view.completionId != event.completionId) {
      _pending.remove(key);
      return;
    }
    if (task.status != Status.done ||
        view.phase != 'committed' ||
        view.recordedMatch != true) return;
    // Synchronous, RAM-only consumption immediately before the existing effect.
    // Neither a failed plugin call nor a historical poll can regrant it.
    _pending.remove(key);
    _notified.add(key);
    unawaited(
        _showNotification(title: 'notificationTaskDone'.tr, body: task.name)
            .catchError((Object _) {}));
  }

  void _startPolling() {
    _timer = Timer.periodic(const Duration(seconds: 2), (_) => _poll());
  }

  Future<void> _poll() async {
    if (_closed || _polling) return;
    if (!appController.downloaderConfig.value.extra.desktopNotification) {
      _disconnect();
      return;
    }
    _polling = true;
    final source = api.activeApiSnapshot;
    try {
      final tasks = await _loadTasks(Status.values);
      if (_closed ||
          !identical(source, api.activeApiSnapshot) ||
          !appController.downloaderConfig.value.extra.desktopNotification) {
        return;
      }
      _currentTasks
        ..clear()
        ..addEntries(tasks.map((task) => MapEntry(task.id, task)));
      for (final task in tasks) {
        final previous = _previousStatus[task.id];
        if (previous != null && previous != task.status) {
          if (task.status == Status.done && task.completionView == null) {
            _showNotification(
                title: 'notificationTaskDone'.tr, body: task.name);
          } else if (task.status == Status.error) {
            _showNotification(
                title: 'notificationTaskError'.tr, body: task.name);
          }
        }
        _previousStatus[task.id] = task.status;
      }
      _previousStatus.removeWhere((id, _) => !_currentTasks.containsKey(id));
      final domains = tasks
          .map((task) => task.completionView)
          .where((view) => view?.available == true)
          .map((view) => view!.datasetId)
          .toSet();
      if (_dataset != null &&
          domains.length == 1 &&
          domains.single != _dataset) {
        _disconnect();
      }
      if (source != null) {
        for (final key in _pending.keys.toList()) {
          _consumePending(source, _streamEpoch, key);
        }
      }
      if (source?.taskBatchRoute?.supported == true && source!.isCurrent) {
        unawaited(_ensureStream(source));
      }
    } catch (_) {
      // Existing polling failure behavior: no notification inferred.
    } finally {
      _polling = false;
    }
  }

  int _notificationId = 0;

  Future<void> _showNotification(
      {required String title, required String body}) async {
    const NotificationDetails notificationDetails = NotificationDetails(
      macOS: DarwinNotificationDetails(),
      linux: LinuxNotificationDetails(),
      windows: WindowsNotificationDetails(),
    );

    await flutterLocalNotificationsPlugin.show(
      id: _notificationId++,
      title: title,
      body: body,
      notificationDetails: notificationDetails,
    );
  }
}
