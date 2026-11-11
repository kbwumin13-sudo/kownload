import 'package:flutter/foundation.dart';

import '../../../../api/model/downloader_config.dart';

/// Snapshot only editable fields. Generated trackers and unknown fields remain
/// on the current objects, including objects captured by an in-flight update.
class BtSettingsDraft {
  final int _port, _seedTime;
  final bool _seedKeep, _autoUpdate;
  final double _seedRatio;
  final List<String> _subscriptions, _custom;
  BtSettingsDraft(DownloaderConfig config)
      : _port = config.protocolConfig.bt.listenPort,
        _seedTime = config.protocolConfig.bt.seedTime,
        _seedKeep = config.protocolConfig.bt.seedKeep,
        _seedRatio = config.protocolConfig.bt.seedRatio,
        _autoUpdate = config.extra.bt.autoUpdateTrackers,
        _subscriptions = List.of(config.extra.bt.trackerSubscribeUrls),
        _custom = List.of(config.extra.bt.customTrackers);

  VoidCallback apply(DownloaderConfig current, DownloaderConfig draft) {
    final bt = current.protocolConfig.bt, edited = draft.protocolConfig.bt;
    final extra = current.extra.bt, editedExtra = draft.extra.bt;
    final restores = <VoidCallback>[];
    void field<T>(T before, T next, T Function() get, void Function(T) set) {
      if (before == next) return;
      final previous = get();
      set(next);
      restores.add(() {
        if (get() == next) set(previous);
      });
    }

    void list(List<String> before, List<String> next,
        List<String> Function() get, void Function(List<String>) set) {
      if (listEquals(before, next)) return;
      final previous = get(), applied = List<String>.of(next);
      set(applied);
      restores.add(() {
        if (listEquals(get(), applied)) set(previous);
      });
    }

    field(_port, edited.listenPort, () => bt.listenPort,
        (v) => bt.listenPort = v);
    field(
        _seedTime, edited.seedTime, () => bt.seedTime, (v) => bt.seedTime = v);
    field(
        _seedKeep, edited.seedKeep, () => bt.seedKeep, (v) => bt.seedKeep = v);
    field(_seedRatio, edited.seedRatio, () => bt.seedRatio,
        (v) => bt.seedRatio = v);
    field(_autoUpdate, editedExtra.autoUpdateTrackers,
        () => extra.autoUpdateTrackers, (v) => extra.autoUpdateTrackers = v);
    list(
        _subscriptions,
        editedExtra.trackerSubscribeUrls,
        () => extra.trackerSubscribeUrls,
        (v) => extra.trackerSubscribeUrls = v);
    final customChanged = !listEquals(_custom, editedExtra.customTrackers);
    list(_custom, editedExtra.customTrackers, () => extra.customTrackers,
        (v) => extra.customTrackers = v);
    void refreshGenerated() {
      bt.trackers = [...extra.subscribeTrackers, ...extra.customTrackers];
    }

    if (customChanged) refreshGenerated();
    return () {
      for (final restore in restores.reversed) {
        restore();
      }
      // Use the latest subscription result, never a pre-update tracker snapshot.
      if (customChanged) refreshGenerated();
    };
  }
}
