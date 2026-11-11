import 'dart:convert';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/downloader_config.dart';

void main() {
  Map<String, dynamic> protocolJson() => {
        'http': {
          'userAgent': 'old',
          'futureHeaderPolicy': {
            'mode': 'keep',
            'values': [1, null, true],
          },
        },
        'bt': {
          'trackers': ['https://tracker.test'],
          'futurePeerPolicy': false,
        },
        'ed2k': {
          'serverAddr': 'server.test:4661',
          'futureNodes': [
            3,
            {'x': 'y'},
          ],
        },
        'media': {'jsRuntimePath': '/custom/node', 'futureFormat': null},
        'netdisk': {
          'connections': 7,
          'provider': {
            'future': ['a', false, null],
          },
        },
        'futureString': 'opaque',
        'futureNumber': 3.5,
        'futureBool': true,
        'futureNull': null,
        'futureArray': [
          1,
          'two',
          {'nested': null},
        ],
        'futureProvider': {
          'enabled': true,
          'options': {'limit': 9},
        },
      };

  test('unknown protocols and nested protocol fields survive known edits', () {
    final original = protocolJson();
    final config = ProtocolConfig.fromJson(original);
    config.http.userAgent = 'new';
    config.bt.listenPort = 1234;
    config.ed2k.udpPort = 4321;
    config.media.connections = 6;
    final result = jsonDecode(jsonEncode(config.toJson())) as Map;
    for (final key in original.keys.where(
      (k) => !['http', 'bt', 'ed2k', 'media'].contains(k),
    )) {
      expect(result[key], original[key], reason: key);
    }
    for (final pair in [
      ('http', 'futureHeaderPolicy'),
      ('bt', 'futurePeerPolicy'),
      ('ed2k', 'futureNodes'),
      ('media', 'futureFormat'),
    ]) {
      expect(
        (result[pair.$1] as Map)[pair.$2],
        (original[pair.$1] as Map)[pair.$2],
      );
    }
    expect(result['http']['userAgent'], 'new');
    expect(result['bt']['listenPort'], 1234);
    expect(result['ed2k']['udpPort'], 4321);
    expect(result['media']['connections'], 6);
    expect(jsonEncode(result), isNot(contains('_unrecognized')));
  });

  test('empty and nullable known values never resurrect their old value', () {
    final config = ProtocolConfig.fromJson(protocolJson());
    config.http.userAgent = '';
    config.bt.trackers = [];
    config.ed2k.serverAddr = '';
    config.media.jsRuntimePath = null;
    config.netdisk = null;
    final result = config.toJson();
    expect(result['http']['userAgent'], '');
    expect(result['bt']['trackers'], isEmpty);
    expect(result['ed2k']['serverAddr'], '');
    expect(result['media'].containsKey('jsRuntimePath'), isFalse);
    expect(result.containsKey('netdisk'), isFalse);
    expect(result.containsKey('futureNull'), isTrue);
    expect(result['media'].containsKey('futureFormat'), isTrue);
    final again = ProtocolConfig.fromJson(result).toJson();
    expect(again, result);
  });

  test('unknown JSON is not aliased through decode or encode', () {
    final input = protocolJson();
    final config = ProtocolConfig.fromJson(input);
    (input['futureProvider']['options'] as Map)['limit'] = 100;
    (input['http']['futureHeaderPolicy']['values'] as List).clear();
    final first = config.toJson();
    expect(first['futureProvider']['options']['limit'], 9);
    expect(first['http']['futureHeaderPolicy']['values'], [1, null, true]);
    (first['futureArray'] as List).clear();
    (first['ed2k']['futureNodes'] as List).clear();
    expect(config.toJson()['futureArray'], [
      1,
      'two',
      {'nested': null},
    ]);
    expect(config.toJson()['ed2k']['futureNodes'], [
      3,
      {'x': 'y'},
    ]);
  });

  test('saving ordinary preferences retains unknown protocol data', () {
    final json = DownloaderConfig().toJson();
    json['protocolConfig'] = protocolJson();
    final config = DownloaderConfig.fromJson(json);
    final before = config.protocolConfig.toJson();
    config.maxRunning = 7;
    config.downloadDir = '/tmp/fixture-downloads';
    final restored = DownloaderConfig.fromJson(config.toJson());
    expect(restored.protocolConfig.toJson(), before);
    expect(restored.maxRunning, 7);
    expect(restored.downloadDir, '/tmp/fixture-downloads');
  });
}
