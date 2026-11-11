import 'dart:async';
import 'dart:convert';
import 'dart:io';
import 'package:flutter_test/flutter_test.dart';
import 'package:hive/hive.dart';
import 'package:gopeed/app/modules/app/controllers/app_controller.dart';
import 'package:gopeed/database/database.dart';
import 'package:gopeed/core/common/start_config.dart';

class _DeferredBox extends Fake implements Box<dynamic> {
  final completion = Completer<void>();
  final stored = <dynamic, dynamic>{};
  final writes = <MapEntry<dynamic, dynamic>>[];

  @override
  Future<void> put(dynamic key, dynamic value) {
    writes.add(MapEntry(key, value));
    return completion.future.then((_) {
      stored[key] = value;
    });
  }

  @override
  dynamic get(dynamic key, {dynamic defaultValue}) =>
      stored[key] ?? defaultValue;
}

class _StartupOnlyController extends AppController {
  @override
  Future<void> saveConfig() =>
      throw StateError('full config must not be submitted');
}

StartConfig _fixtureConfig(AppController app) {
  final cfg = app.startConfig.value;
  cfg.network = 'unix';
  cfg.address = '/fixture/next-start.sock';
  cfg.apiToken = 'synthetic-token';
  cfg.storage = 'bolt';
  cfg.storageDir = '/fixture/store';
  cfg.refreshInterval = 0;
  return cfg;
}

void main() {
  test('startup-only save awaits storage and preserves engine object/options',
      () async {
    final box = _DeferredBox();
    Database.instance.box = box;
    final app = _StartupOnlyController();
    final cfg = _fixtureConfig(app);
    final before = cfg.toJson();
    var saved = false;
    final saving = app.saveStartConfig().then((_) {
      saved = true;
    });
    expect(box.writes, hasLength(1));
    expect(saved, false);
    expect(box.stored, isEmpty);
    final payload = jsonDecode(box.writes.single.value as String);
    expect(payload, {
      'network': 'unix',
      'address': '/fixture/next-start.sock',
      'apiToken': 'synthetic-token'
    });
    // Mutating a later input must not alter the in-flight Entity snapshot.
    cfg.address = '/fixture/later.sock';
    box.completion.complete();
    await saving;
    expect(saved, true);
    expect(identical(cfg, app.startConfig.value), true);
    for (final key in before.keys
        .where((key) => !['network', 'address', 'apiToken'].contains(key))) {
      expect(cfg.toJson()[key], before[key],
          reason: 'engine option $key changed');
    }
    expect(jsonDecode(box.stored['startConfig'])['address'],
        '/fixture/next-start.sock');
  });

  test('storage failure reaches caller and leaves persisted previous values',
      () async {
    final box = _DeferredBox();
    const old =
        '{"network":"unix","address":"/fixture/original.sock","apiToken":"old-fixture"}';
    box.stored['startConfig'] = old;
    Database.instance.box = box;
    final app = _StartupOnlyController();
    final cfg = _fixtureConfig(app);
    final saving = expectLater(app.saveStartConfig(), throwsStateError);
    box.completion.completeError(StateError('fixture disk failure'));
    await saving;
    expect(box.stored['startConfig'], old);
    expect(identical(app.startConfig.value, cfg), true);
  });

  test('awaited Hive save survives reopen without writing other preferences',
      () async {
    final root = await Directory.systemTemp.createTemp('startup-save-fixture-');
    Box<dynamic>? box;
    try {
      box =
          await Hive.openBox<dynamic>('startup_save_fixture', path: root.path);
      await box.put('otherPreference', 'keep');
      Database.instance.box = box;
      final app = _StartupOnlyController();
      _fixtureConfig(app);
      await app.saveStartConfig();
      await box.close();
      box =
          await Hive.openBox<dynamic>('startup_save_fixture', path: root.path);
      expect(jsonDecode(box.get('startConfig')), {
        'network': 'unix',
        'address': '/fixture/next-start.sock',
        'apiToken': 'synthetic-token'
      });
      expect(box.get('otherPreference'), 'keep');
      expect(box.keys.toSet(), {'startConfig', 'otherPreference'});
    } finally {
      await box?.close();
      await root.delete(recursive: true);
    }
  });
}
