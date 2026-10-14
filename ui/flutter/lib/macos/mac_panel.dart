import 'dart:async';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import '../api/model/create_task.dart';
import '../app/routes/app_pages.dart';
import '../database/database.dart';

/// One native window and one engine serve both the launcher and management UI.
class MacPanel {
  static const channel = MethodChannel('gopeed/macos_panel');
  static final presentation = ValueNotifier<String>('panel');
  static final visible = ValueNotifier<bool>(false);
  static final settingsSection = ValueNotifier<String>('general');
  static final focusRequests = ValueNotifier<int>(0);
  static final incoming = ValueNotifier<CreateTask?>(null);
  static final hotKeyLabel = ValueNotifier<String>('⌃⌥D');
  static final hotKeyError = ValueNotifier<String?>(null);
  static final notice = ValueNotifier<String?>(null);
  static double _height = 260;
  static Timer? _dismiss;
  static bool _initialized = false;
  static bool _settingsInitialized = false;
  static int _transientCount = 0;

  static Future<dynamic> _invoke(String method, [dynamic arguments]) async {
    try {
      return await channel.invokeMethod<dynamic>(method, arguments);
    } on MissingPluginException {
      return null;
    }
  }

  static Future<void> initialize() async {
    if (_initialized) return;
    _initialized = true;
    channel.setMethodCallHandler((call) async {
      if (call.method == 'visibility') {
        visible.value = call.arguments['visible'] == true;
      } else if (call.method == 'focusInput') {
        focusRequests.value++;
      } else if (call.method == 'command') {
        switch (call.arguments) {
          case 'togglePanel':
            await toggle();
            break;
          case 'settings':
            await showSettings();
            break;
          case 'openPanel':
          case 'newTask':
            await showPanel();
            break;
        }
      }
    });
    Map<String, dynamic>? saved;
    try {
      saved = Database.instance.get<Map<String, dynamic>>(
          'macPanelHotKey', (json) => Map<String, dynamic>.from(json));
    } catch (_) {}
    await changeHotKey(
        keyCode: saved?['keyCode'] as int? ?? 2,
        modifiers: saved?['modifiers'] as int? ?? 6144,
        label: saved?['label'] as String? ?? '⌃⌥D',
        persist: false);
    await _invoke('setPresentation',
        {'mode': 'panel', 'width': 640, 'height': _height, 'show': false});
  }

  static Future<bool> changeHotKey(
      {required int keyCode,
      required int modifiers,
      required String label,
      bool persist = true}) async {
    final result = await _invoke(
        'registerHotKey', {'keyCode': keyCode, 'modifiers': modifiers});
    if (result is Map && result['registered'] != true) {
      hotKeyError.value = '快捷键被占用或无法注册，请更换组合。';
      return false;
    }
    hotKeyLabel.value = label;
    hotKeyError.value = null;
    if (persist) {
      try {
        Database.instance.save('macPanelHotKey',
            {'keyCode': keyCode, 'modifiers': modifiers, 'label': label});
      } catch (_) {}
    }
    return true;
  }

  static Future<void> showPanel({CreateTask? task}) async {
    cancelAutoHide();
    if (presentation.value == 'settings') await rememberSettingsSize();
    presentation.value = 'panel';
    if (task != null) incoming.value = task;
    await _invoke('setPresentation',
        {'mode': 'panel', 'width': 640, 'height': _height, 'show': true});
    visible.value = true;
    focusRequests.value++;
  }

  static Future<void> showSettings({String section = 'general'}) async {
    cancelAutoHide();
    settingsSection.value = section;
    presentation.value = 'settings';
    final route = section == 'tasks' ? Routes.TASK : Routes.SETTING;
    if (Get.rootDelegate.currentConfiguration?.uri.path != route) {
      Get.rootDelegate.toNamed(route);
    }
    final arguments = <String, dynamic>{'mode': 'settings', 'show': true};
    if (!_settingsInitialized) {
      _settingsInitialized = true;
      try {
        final saved = Database.instance.get<Map<String, dynamic>>(
            'macPanelSettingsSize', (json) => Map<String, dynamic>.from(json));
        arguments['width'] = saved?['width'] ?? 900;
        arguments['height'] = saved?['height'] ?? 700;
      } catch (_) {
        arguments.addAll({'width': 900, 'height': 700});
      }
    }
    await _invoke('setPresentation', arguments);
    visible.value = true;
  }

  static Future<void> rememberSettingsSize() async {
    if (presentation.value != 'settings') return;
    final state = await _invoke('getState');
    if (state is! Map || state['mode'] != 'settings') return;
    final width = state['width'];
    final height = state['height'];
    if (width is! num ||
        height is! num ||
        !width.isFinite ||
        !height.isFinite ||
        width < 800 ||
        height < 560) return;
    try {
      Database.instance.save('macPanelSettingsSize',
          {'width': width.toDouble(), 'height': height.toDouble()});
    } catch (_) {}
  }

  static Future<void> toggle() async {
    if (visible.value && presentation.value == 'panel') {
      await hide();
    } else {
      await showPanel();
    }
  }

  static Future<void> hide() async {
    cancelAutoHide();
    await _invoke('hide');
    visible.value = false;
  }

  static Future<void> resize(double height) async {
    final next = height.clamp(180.0, 760.0);
    if ((_height - next).abs() < 1) return;
    _height = next;
    if (presentation.value == 'panel') {
      await _invoke('resizePanel', {'height': next});
    }
  }

  static Future<T> transient<T>(Future<T> Function() operation) async {
    _transientCount++;
    await _invoke('setTransient', {'active': true});
    try {
      return await operation();
    } finally {
      _transientCount--;
      await _invoke('setTransient', {'active': _transientCount > 0});
    }
  }

  static void cancelAutoHide() {
    _dismiss?.cancel();
    _dismiss = null;
    notice.value = null;
  }

  static void submitted() {
    cancelAutoHide();
    notice.value = '已加入下载';
    _dismiss = Timer(const Duration(milliseconds: 800), () {
      if (presentation.value == 'panel') hide();
    });
  }
}
