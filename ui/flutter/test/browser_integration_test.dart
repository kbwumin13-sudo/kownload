import 'dart:convert';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/app/services/browser_integration.dart';

void main() {
  Map<String, dynamic>? stored;
  late BrowserIntegration app;
  setUp(() {
    stored = null;
    app = BrowserIntegration(
        read: () => stored,
        write: (value) async {
          stored = jsonDecode(jsonEncode(value));
        });
  });
  Map<String, dynamic> sync(String id, String browser, Map legacy) => {
        'action': 'sync',
        'clientId': id,
        'browser': browser,
        'legacySettings': legacy,
        'downloadsPermission': true,
        'version': '0.4.3',
      };

  test('Chrome, Edge and separate profiles migrate without merging preferences',
      () async {
    await app.command(sync('chrome-one', 'chrome', {
      'downloadCapture': {'enabled': true, 'confirm': false},
      'showAll': true,
      'disabledSites': {'a.test': true},
      'Cookie': 'must-not-persist',
    }));
    await app.command(sync('edge-one', 'edge', {
      'downloadCapture': {'enabled': false, 'confirm': true},
      'showAll': false,
      'disabledSites': {'b.test': true},
    }));
    await app.command(sync('chrome-two', 'chrome', {'showAll': false}));
    expect(app.clients, hasLength(3));
    final chrome = app.clients.first.preferences;
    final edge = app.clients[1].preferences;
    expect(chrome.captureEnabled, true);
    expect(chrome.confirmDownloads, false);
    expect(chrome.showAll, true);
    expect(chrome.disabledSites, {'a.test': true});
    expect(edge.captureEnabled, false);
    expect(edge.confirmDownloads, true);
    expect(edge.disabledSites, {'b.test': true});
    expect(app.clients.last.preferences.showAll, false);
    expect(jsonEncode(stored), isNot(contains('must-not-persist')));
    expect(jsonEncode(stored), isNot(contains('downloadsPermission')));
  });

  test('legacy snapshots cannot overwrite App changes, including after restart',
      () async {
    final original = sync('one', 'chrome', {
      'downloadCapture': {'enabled': true, 'confirm': false}
    });
    await app.command(original);
    await app.update('one', {
      'downloadCapture': {'enabled': false},
      'sniffing': {'showAll': true}
    });
    await app.command(original);
    app = BrowserIntegration(
        read: () => stored,
        write: (value) async {
          stored = value;
        });
    final result = await app.command(original);
    expect(result['revision'], 2);
    expect(result['settings']['downloadCapture'],
        {'enabled': false, 'confirm': false});
    expect(result['settings']['sniffing']['showAll'], true);
    expect(app.statuses['one']!.downloadsPermission, true);
  });

  test('rapid patches serialize and retain independent fields', () async {
    await app.command(sync('one', 'chrome', {}));
    final first = app.update('one', {
      'downloadCapture': {'enabled': true}
    });
    final second = app.update('one', {
      'sniffing': {'showAll': true}
    });
    await Future.wait([first, second]);
    expect(app.clients.single.preferences.captureEnabled, true);
    expect(app.clients.single.preferences.showAll, true);
    expect(app.clients.single.revision, 3);
  });

  test(
      'failed persistence does not change canonical state and a later patch can retry',
      () async {
    await app.command(sync('one', 'chrome', {}));
    var failSave = true;
    app = BrowserIntegration(
        read: () => stored,
        write: (value) async {
          if (failSave) throw StateError('disk unavailable');
          stored = value;
        });
    await expectLater(
        app.update('one', {
          'sniffing': {'showAll': true}
        }),
        throwsStateError);
    expect(app.clients.single.preferences.showAll, false);
    expect(app.clients.single.revision, 1);
    failSave = false;
    await app.update('one', {
      'sniffing': {'showAll': true}
    });
    expect(app.clients.single.preferences.showAll, true);
    expect(app.clients.single.revision, 2);
  });

  test('site patches preserve other sites and browser instances', () async {
    await app.command(sync('one', 'chrome', {
      'disabledSites': {'a.test': true, 'b.test': true}
    }));
    await app.command(sync('two', 'edge', {
      'disabledSites': {'a.test': true}
    }));
    await app.command({
      'action': 'updateSettings',
      'clientId': 'one',
      'browser': 'chrome',
      'patch': {
        'sniffing': {
          'disabledSites': {'a.test': false, 'c.test': true}
        }
      }
    });
    expect(app.clients.first.preferences.disabledSites,
        {'b.test': true, 'c.test': true});
    expect(app.clients.last.preferences.disabledSites, {'a.test': true});
  });

  test(
      'unknown fields, credentials, bad types and mismatched identities are rejected',
      () async {
    await app.command(sync('one', 'chrome', {}));
    for (final patch in [
      {'Cookie': 'secret'},
      {
        'sniffing': {'Authorization': 'secret'}
      },
      {
        'downloadCapture': {'enabled': 'yes'}
      },
      {
        'sniffing': {
          'disabledSites': {'https://a.test/?token=secret': true}
        }
      },
    ]) {
      await expectLater(app.update('one', patch), throwsFormatException);
    }
    await expectLater(
        app.command(
            {'action': 'settings', 'clientId': 'one', 'browser': 'edge'}),
        throwsFormatException);
    expect(app.clients.single.revision, 1);
    expect(jsonEncode(stored), isNot(contains('secret')));
  });

  test(
      'only explicit openSettings opens UI; submission queries never create downloads',
      () async {
    var opens = 0;
    final ids = <String>[];
    app = BrowserIntegration(
        read: () => stored,
        write: (value) async {
          stored = value;
        },
        openSettings: () async {
          opens++;
        },
        submission: (id) {
          ids.add(id);
          return {'state': 'accepted', 'taskIds': <String>[]};
        });
    await app.command(sync('one', 'edge', {}));
    expect(opens, 0);
    final result =
        await app.command({'action': 'submission', 'operationId': 'one-op'});
    expect(result['state'], 'accepted');
    expect(opens, 0);
    expect(ids, ['one-op']);
    await app.command({'action': 'openSettings'});
    expect(opens, 1);
  });

  test('a restarted App reports unknown instead of assuming cancellation',
      () async {
    final result =
        await app.command({'action': 'submission', 'operationId': 'lost-op'});
    expect(result, {'state': 'unknown', 'taskIds': []});
  });
}
