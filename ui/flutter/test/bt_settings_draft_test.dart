import 'dart:async';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/downloader_config.dart';
import 'package:gopeed/app/modules/setting/views/bt_settings_draft.dart';

void main() {
  for (final updateFirst in [true, false]) {
    test(
        'BT keeps captured objects when automatic update completes ${updateFirst ? 'before' : 'after'} draft save',
        () async {
      final current = DownloaderConfig();
      current.extra.bt.subscribeTrackers = ['old'];
      current.protocolConfig.bt.trackers = ['old'];
      final snapshot = BtSettingsDraft(current);
      final draft = DownloaderConfig.fromJson(current.toJson());
      draft.protocolConfig.bt.listenPort = 43210;
      final capturedExtra = current.extra.bt;
      final gate = Completer<void>();
      final timestamp = DateTime.utc(2026, 1, 2);
      final updating = () async {
        await gate.future;
        capturedExtra.subscribeTrackers = ['new-subscription'];
        capturedExtra.lastTrackerUpdateTime = timestamp;
        current.protocolConfig.bt.trackers = [
          ...capturedExtra.subscribeTrackers,
          ...capturedExtra.customTrackers
        ];
      }();
      // A newer live config can arrive while the unrelated draft is open.
      current.protocolConfig.bt = BtConfig.fromJson({
        ...current.protocolConfig.bt.toJson(),
        'seedRatio': 2.5,
        'futureBt': {
          'keep': [1, null, true]
        }
      });
      final capturedProtocol = current.protocolConfig.bt;
      if (updateFirst) {
        gate.complete();
        await updating;
      }
      snapshot.apply(current, draft);
      if (!updateFirst) {
        gate.complete();
        await updating;
      }
      expect(identical(current.extra.bt, capturedExtra), isTrue);
      expect(identical(current.protocolConfig.bt, capturedProtocol), isTrue);
      expect(current.protocolConfig.bt.listenPort, 43210);
      expect(current.protocolConfig.bt.seedRatio, 2.5);
      expect(current.protocolConfig.bt.trackers, ['new-subscription']);
      expect(current.extra.bt.subscribeTrackers, ['new-subscription']);
      expect(current.extra.bt.lastTrackerUpdateTime, timestamp);
      expect(current.protocolConfig.bt.toJson()['futureBt'], {
        'keep': [1, null, true]
      });
    });
  }

  test(
      'custom tracker edits and failed-save restore use latest subscription result',
      () {
    final current = DownloaderConfig();
    current.extra.bt.customTrackers = ['old-custom'];
    current.extra.bt.subscribeTrackers = ['old-subscription'];
    final snapshot = BtSettingsDraft(current);
    final draft = DownloaderConfig.fromJson(current.toJson());
    draft.extra.bt.customTrackers = ['edited-custom'];
    current.extra.bt.subscribeTrackers = ['fresh-subscription'];
    final restore = snapshot.apply(current, draft);
    expect(current.protocolConfig.bt.trackers,
        ['fresh-subscription', 'edited-custom']);
    current.extra.bt.subscribeTrackers = ['latest-subscription'];
    final time = DateTime.utc(2026, 1, 3);
    current.extra.bt.lastTrackerUpdateTime = time;
    restore();
    expect(current.extra.bt.customTrackers, ['old-custom']);
    expect(current.protocolConfig.bt.trackers,
        ['latest-subscription', 'old-custom']);
    expect(current.extra.bt.subscribeTrackers, ['latest-subscription']);
    expect(current.extra.bt.lastTrackerUpdateTime, time);
  });
}
