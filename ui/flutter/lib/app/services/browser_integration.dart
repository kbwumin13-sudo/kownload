import 'dart:convert';

import 'package:flutter/foundation.dart';

import '../../database/database.dart';
import '../rpc/rpc.dart';

/// App-owned preferences. Browser instances keep separate choices; credentials
/// and captured download requests never belong in this store.
class BrowserPreferences {
  final bool captureEnabled;
  final bool confirmDownloads;
  final bool sniffEnabled;
  final bool showAll;
  final Map<String, bool> disabledSites;

  const BrowserPreferences({
    this.captureEnabled = false,
    this.confirmDownloads = true,
    this.sniffEnabled = true,
    this.showAll = false,
    this.disabledSites = const {},
  });

  static Map<String, bool> _sites(dynamic value) {
    if (value is! Map) return {};
    return {
      for (final entry in value.entries)
        if (entry.key is String && _host(entry.key) && entry.value == true)
          (entry.key as String).toLowerCase(): true,
    };
  }

  static bool _host(String value) {
    if (value.isEmpty ||
        value.length > 253 ||
        value.contains(RegExp(r'[\s/?#@]'))) return false;
    final parsed = Uri.tryParse('https://$value/');
    return parsed != null &&
        parsed.host.isNotEmpty &&
        parsed.host.toLowerCase() == value.toLowerCase();
  }

  factory BrowserPreferences.fromJson(Map value) {
    final capture = value['downloadCapture'];
    final sniff = value['sniffing'];
    return BrowserPreferences(
      captureEnabled: capture is Map && capture['enabled'] == true,
      confirmDownloads: capture is! Map || capture['confirm'] != false,
      sniffEnabled: sniff is! Map || sniff['enabled'] != false,
      showAll: sniff is Map && sniff['showAll'] == true,
      disabledSites: Map.unmodifiable(
          _sites(sniff is Map ? sniff['disabledSites'] : null)),
    );
  }

  factory BrowserPreferences.fromLegacy(Map value) =>
      BrowserPreferences.fromJson({
        'downloadCapture': value['downloadCapture'],
        'sniffing': {
          'showAll': value['showAll'],
          'disabledSites': value['disabledSites'],
        },
      });

  Map<String, dynamic> toJson() => {
        'downloadCapture': {
          'enabled': captureEnabled,
          'confirm': confirmDownloads
        },
        'sniffing': {
          'enabled': sniffEnabled,
          'showAll': showAll,
          'disabledSites': Map<String, bool>.from(disabledSites)
        },
      };

  BrowserPreferences patch(Map value) {
    final next = toJson();
    for (final entry in value.entries) {
      if (!const ['downloadCapture', 'sniffing'].contains(entry.key) ||
          entry.value is! Map) {
        throw const FormatException('invalid_request');
      }
      final target = next[entry.key] as Map<String, dynamic>;
      for (final field in (entry.value as Map).entries) {
        if (!target.containsKey(field.key))
          throw const FormatException('invalid_request');
        if (field.key == 'disabledSites') {
          if (field.value is! Map)
            throw const FormatException('invalid_request');
          final sites = Map<String, bool>.from(disabledSites);
          for (final site in (field.value as Map).entries) {
            if (site.key is! String ||
                !_host(site.key) ||
                site.value is! bool) {
              throw const FormatException('invalid_request');
            }
            if (site.value == true) {
              sites[(site.key as String).toLowerCase()] = true;
            } else {
              sites.remove((site.key as String).toLowerCase());
            }
          }
          target[field.key] = sites;
        } else {
          if (field.value is! bool)
            throw const FormatException('invalid_request');
          target[field.key] = field.value;
        }
      }
    }
    return BrowserPreferences.fromJson(next);
  }
}

class BrowserClient {
  final String id;
  final String browser;
  final BrowserPreferences preferences;
  final int revision;
  const BrowserClient(this.id, this.browser, this.preferences, this.revision);
  String get name => browser == 'edge' ? 'Edge' : 'Chrome';
  Map<String, dynamic> toJson() => {
        'browser': browser,
        'settings': preferences.toJson(),
        'revision': revision,
      };
}

class BrowserClientStatus {
  final DateTime seen;
  final bool downloadsPermission;
  final String version;
  const BrowserClientStatus(this.seen, this.downloadsPermission, this.version);
}

class BrowserIntegration extends ChangeNotifier {
  static const storageKey = 'browserIntegrationV1';
  final Map<String, dynamic>? Function() read;
  final Future<void> Function(Map<String, dynamic>) write;
  final Future<void> Function()? openSettings;
  final Map<String, dynamic> Function(String)? submission;
  final Map<String, dynamic> Function(String, String, String, Map)?
      decideSubmission;
  final _clients = <String, BrowserClient>{};
  final statuses = <String, BrowserClientStatus>{};
  Future<void> _queue = Future.value();
  bool _loaded = false;

  BrowserIntegration(
      {required this.read,
      required this.write,
      this.openSettings,
      this.submission,
      this.decideSubmission});

  factory BrowserIntegration.database(
          {Future<void> Function()? openSettings,
          Map<String, dynamic> Function(String)? submission,
          Map<String, dynamic> Function(String, String, String, Map)?
              decideSubmission}) =>
      BrowserIntegration(
        read: () => Database.instance.get<Map<String, dynamic>>(
            storageKey, (value) => Map<String, dynamic>.from(value)),
        write: (value) =>
            Database.instance.box.put(storageKey, jsonEncode(value)),
        openSettings: openSettings,
        submission: submission,
        decideSubmission: decideSubmission,
      );

  List<BrowserClient> get clients {
    _load();
    return List.unmodifiable(_clients.values);
  }

  void _load() {
    if (_loaded) return;
    final values = read()?['clients'];
    if (values is Map) {
      for (final entry in values.entries) {
        final value = entry.value;
        if (entry.key is String &&
            _validId(entry.key) &&
            value is Map &&
            const ['chrome', 'edge'].contains(value['browser']) &&
            value['settings'] is Map) {
          _clients[entry.key] = BrowserClient(
              entry.key,
              value['browser'],
              BrowserPreferences.fromJson(value['settings']),
              value['revision'] is int ? value['revision'] : 1);
        }
      }
    }
    _loaded = true;
  }

  static bool _validId(String id) =>
      RegExp(r'^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$').hasMatch(id);
  Map<String, dynamic> _data(BrowserClient client) => {
        'settings': client.preferences.toJson(),
        'revision': client.revision,
      };
  Future<T> _serial<T>(Future<T> Function() action) {
    final result = _queue.then((_) => action());
    _queue = result.then<void>((_) {}, onError: (Object _, StackTrace __) {});
    return result;
  }

  Future<void> _store(BrowserClient client) async {
    final all = {..._clients, client.id: client};
    await write({
      'clients': {
        for (final entry in all.entries) entry.key: entry.value.toJson()
      }
    });
    _clients[client.id] = client;
    notifyListeners();
  }

  Future<void> update(String id, Map patch) => _serial(() async {
        _load();
        final client = _clients[id];
        if (client == null) throw const FormatException('unknown_client');
        await _store(BrowserClient(id, client.browser,
            client.preferences.patch(patch), client.revision + 1));
      });

  Future<Map<String, dynamic>> command(Map<String, dynamic> request) async {
    final action = request['action'];
    if (action == 'openSettings') {
      await openSettings?.call();
      return {'opened': true};
    }
    if (action == 'submission') {
      final id = request['operationId'];
      if (id is! String || !_validId(id))
        throw const FormatException('invalid_request');
      if (request['decision'] != null) {
        if (request['decision'] is! String ||
            request['confirmToken'] is! String ||
            request['browserState'] is! Map) {
          throw const FormatException('invalid_request');
        }
        return decideSubmission?.call(id, request['confirmToken'],
                request['decision'], request['browserState']) ??
            {'state': 'unknown', 'taskIds': <String>[]};
      }
      return submission?.call(id) ??
          {'state': 'unknown', 'taskIds': <String>[]};
    }
    if (!const ['sync', 'settings', 'updateSettings'].contains(action)) {
      throw const FormatException('invalid_request');
    }
    final id = request['clientId'];
    final browser = request['browser'];
    if (id is! String ||
        !_validId(id) ||
        !const ['chrome', 'edge'].contains(browser)) {
      throw const FormatException('invalid_request');
    }
    return _serial(() async {
      _load();
      var client = _clients[id];
      if (client != null && client.browser != browser)
        throw const FormatException('invalid_request');
      if (client == null) {
        if (action != 'sync') throw const FormatException('unknown_client');
        final legacy = request['legacySettings'];
        client = BrowserClient(
            id,
            browser,
            legacy is Map
                ? BrowserPreferences.fromLegacy(legacy)
                : const BrowserPreferences(),
            1);
        await _store(client);
      }
      if (action == 'sync') {
        statuses[id] = BrowserClientStatus(
            DateTime.now(),
            request['downloadsPermission'] == true,
            request['version'] is String
                ? (request['version'] as String).substring(
                    0, (request['version'] as String).length.clamp(0, 64))
                : '');
        notifyListeners();
      } else if (action == 'updateSettings') {
        if (request['patch'] is! Map)
          throw const FormatException('invalid_request');
        client = BrowserClient(id, browser,
            client.preferences.patch(request['patch']), client.revision + 1);
        await _store(client);
      }
      return _data(client);
    });
  }

  Future<void> handle(RpcContext ctx) async {
    try {
      final result = await command(await ctx.readJSON());
      await ctx.writeJSON({'code': 0, 'msg': '', 'data': result});
    } on FormatException {
      await ctx.writeJSON({
        'code': 1000,
        'msg': '浏览器请求无效',
        'data': null,
        'error': 'invalid_request'
      }, statusCode: 400);
    } catch (_) {
      await ctx.writeJSON({
        'code': 1000,
        'msg': '浏览器设置未保存，请重试',
        'data': null,
        'error': 'request_failed'
      }, statusCode: 500);
    }
  }
}
