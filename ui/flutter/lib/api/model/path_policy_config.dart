import 'dart:convert';

Object? _copy(Object? value) => jsonDecode(jsonEncode(value));

/// The single Config value stored in Extra.pathPolicyV1. Unknown values and
/// fields survive unrelated settings saves; they do not become executable UI
/// defaults. This is the settings schema, not the internal R Config snapshot.
class PathPolicyConfig {
  final Object? _raw;
  PathPolicyConfig.fromJson(Object? value) : _raw = _copy(value);
  PathPolicyConfig.disabled()
      : _raw = {'enabled': false, 'version': 1, 'rules': <Object>[]};

  Map? get _map => _raw is Map ? _raw as Map : null;
  bool get enabled => _map?['enabled'] == true;
  int? get version => _map?['version'] is int ? _map!['version'] : null;
  List<PathPolicyRule> get rules => List.unmodifiable(
      (_map?['rules'] is List ? _map!['rules'] as List : const [])
          .map(PathPolicyRule.fromJson));
  bool get editable =>
      _map != null &&
      _map!.keys.every(const {'version', 'enabled', 'rules'}.contains) &&
      _map!['enabled'] is bool &&
      version == 1 &&
      _map!['rules'] is List &&
      rules.every((rule) => rule.editable);
  Object? toJson() => _copy(_raw);

  PathPolicyConfig changed({bool? enabled, List<PathPolicyRule>? rules}) {
    if (!editable) throw StateError('unsupported_path_policy_config');
    return PathPolicyConfig.fromJson({
      ..._map!,
      'enabled': enabled ?? this.enabled,
      'version': 1,
      'rules': (rules ?? this.rules).map((r) => r.toJson()).toList(),
    });
  }
}

class PathPolicyRule {
  final Object? _raw;
  PathPolicyRule.fromJson(Object? value) : _raw = _copy(value);
  PathPolicyRule.empty(String id)
      : _raw = {
          'id': id,
          'kind': 'extension',
          'match': <String, dynamic>{},
          'root': ''
        };
  Map? get _map => _raw is Map ? _raw as Map : null;
  Map? get _match => _map?['match'] is Map ? _map!['match'] : null;
  String get id => _map?['id'] is String ? _map!['id'] : '';
  String get kind => _map?['kind'] is String ? _map!['kind'] : '';
  String get root => _map?['root'] is String ? _map!['root'] : '';
  String hint(String name) => _match?[name] is String ? _match![name] : '';
  bool get editable =>
      _map != null &&
      _map!['id'] is String &&
      _map!.keys.every(const {'id', 'kind', 'match', 'root'}.contains) &&
      const {'user', 'domain', 'mime', 'extension'}.contains(kind) &&
      _map!['root'] is String &&
      _match != null &&
      _match!.keys.every(const {'domain', 'mime', 'extension'}.contains) &&
      const ['domain', 'mime', 'extension']
          .every((k) => !_match!.containsKey(k) || _match![k] is String);
  Object? toJson() => _copy(_raw);
  PathPolicyRule changed(
      {String? kind,
      String? root,
      String? domain,
      String? mime,
      String? extension}) {
    if (!editable) throw StateError('unsupported_path_policy_rule');
    final match = Map<String, dynamic>.from(_match!);
    for (final entry
        in {'domain': domain, 'mime': mime, 'extension': extension}.entries) {
      if (entry.value == null) continue;
      if (entry.value!.isEmpty) {
        match.remove(entry.key);
      } else {
        match[entry.key] = entry.value;
      }
    }
    return PathPolicyRule.fromJson({
      ..._map!,
      'kind': kind ?? this.kind,
      'root': root ?? this.root,
      'match': match,
    });
  }
}
