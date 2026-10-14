import 'dart:typed_data';
import 'package:flutter/foundation.dart';
import 'package:flutter/services.dart';

class MacAppearanceState {
  final bool reduceTransparency;
  final bool reduceMotion;
  final bool supportsGlass;
  const MacAppearanceState(
      {this.reduceTransparency = false,
      this.reduceMotion = false,
      this.supportsGlass = false});
}

class MacAppearance {
  static const _channel = MethodChannel('gopeed/macos_appearance');
  static final state = ValueNotifier(const MacAppearanceState());
  static final searchRequests = ValueNotifier(0);
  static final selectAllRequests = ValueNotifier(0);
  static final inspectorRequests = ValueNotifier(0);
  static bool pointerShift = false;
  static bool pointerMeta = false;
  static String _toolbarState = '';
  static ValueChanged<String>? onCommand;
  static double _sidebarWidth = -1;
  static String _windowTint = '';
  static Future<void> initialize() async {
    _channel.setMethodCallHandler((call) async {
      if (call.method == 'appearanceChanged') _read(call.arguments);
      if (call.method == 'pointerModifiers' && call.arguments is Map) {
        pointerShift = call.arguments['shift'] == true;
        pointerMeta = call.arguments['meta'] == true;
      }
      if (call.method == 'command' && call.arguments is String)
        onCommand?.call(call.arguments as String);
    });
    try {
      _read(await _channel.invokeMethod('appearance'));
    } on MissingPluginException {/* Widget tests have no native window. */}
  }

  static void _read(dynamic data) {
    if (data is! Map) return;
    state.value = MacAppearanceState(
        reduceTransparency: data['reduceTransparency'] == true,
        reduceMotion: data['reduceMotion'] == true,
        supportsGlass: data['supportsGlass'] == true);
  }

  static Future<void> setToolbarState(
      {required String section,
      required bool sidebarVisible,
      bool inspectorVisible = false,
      bool inspectorEnabled = false,
      required bool dark,
      bool modal = false}) async {
    final next = '$section/$sidebarVisible/$dark/$modal';
    if (_toolbarState == next) return;
    _toolbarState = next;
    try {
      await _channel.invokeMethod('toolbar', {
        'section': section,
        'sidebarVisible': sidebarVisible,
        'dark': dark,
        'modal': modal
      });
    } on MissingPluginException {/* Tests. */}
  }

  static Future<void> setInspectorState(
      {required bool visible, required bool enabled}) async {
    try {
      await _channel
          .invokeMethod('inspector', {'visible': visible, 'enabled': enabled});
    } on MissingPluginException {/* Tests. */}
  }

  static Future<void> setMinimumWindowSize() async {
    try {
      await _channel.invokeMethod('windowMinimum');
    } on MissingPluginException {/* Tests. */}
  }

  static Future<void> setSidebarWidth(double width) async {
    if (_sidebarWidth == width) return;
    _sidebarWidth = width;
    try {
      await _channel.invokeMethod('sidebar', {'width': width});
    } on MissingPluginException {/* Tests. */}
  }

  static Future<void> setGlassRegions(List<Map<String, Object>> regions) async {
    try {
      await _channel.invokeMethod('glassRegions', regions);
    } on MissingPluginException {/* Tests. */}
  }

  static Future<Uint8List?> symbol(String name, double size) async {
    try {
      return await _channel
          .invokeMethod<Uint8List>('symbol', {'name': name, 'size': size});
    } on MissingPluginException {
      return null;
    }
  }

  static Future<void> setWindowTint(int top, int bottom) async {
    if (_windowTint == '$top/$bottom') return;
    _windowTint = '$top/$bottom';
    try {
      await _channel.invokeMethod('windowTint', {'top': top, 'bottom': bottom});
    } on MissingPluginException {/* Tests. */}
  }
}
