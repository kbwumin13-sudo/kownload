import 'dart:convert';

const _maxSafeInteger = 9007199254740991;
bool _integer(Object? value, {int minimum = 0}) =>
    value is int && value >= minimum && value <= _maxSafeInteger;
bool _text(Object? value) =>
    value is String &&
    value.isNotEmpty &&
    utf8.encode(value).length <= 128 &&
    !RegExp(r'[\x00-\x1f\x7f]').hasMatch(value);

/// An explicit user's RAM-only action. It is not a retry or replay permission.
class HttpRetryAction {
  final String actionId, expectedEpisode;
  final int expectedSourceRevision;
  HttpRetryAction(
      this.actionId, this.expectedEpisode, this.expectedSourceRevision) {
    if (!_text(actionId) ||
        !_text(expectedEpisode) ||
        !_integer(expectedSourceRevision, minimum: 1)) {
      throw const FormatException('invalid_retry_action');
    }
  }
  factory HttpRetryAction.fromJson(Map json) {
    if (json.length != 5 ||
        json['version'] is! int ||
        json['version'] != 1 ||
        json['decision'] != 'retry' ||
        !_text(json['actionId']) ||
        !_text(json['expectedEpisode']) ||
        !_integer(json['expectedSourceRevision'], minimum: 1)) {
      throw const FormatException('invalid_retry_action');
    }
    return HttpRetryAction(json['actionId'], json['expectedEpisode'],
        json['expectedSourceRevision']);
  }
  Map<String, dynamic> toJson() => {
        'version': 1,
        'actionId': actionId,
        'expectedEpisode': expectedEpisode,
        'expectedSourceRevision': expectedSourceRevision,
        'decision': 'retry'
      };
  Map<String, dynamic> toQuery() => {
        'actionId': actionId,
        'expectedEpisode': expectedEpisode,
        'expectedSourceRevision': '$expectedSourceRevision'
      };
}

class HttpRetryReceipt {
  final String state;
  final String? newEpisode;
  final bool queryRequired;
  HttpRetryReceipt._(this.state, this.newEpisode, this.queryRequired);
  factory HttpRetryReceipt.parse(Map json, HttpRetryAction action) {
    if (json['version'] is! int ||
        json['version'] != 1 ||
        json['actionId'] != action.actionId ||
        json['expectedEpisode'] != action.expectedEpisode ||
        json['expectedSourceRevision'] is! int ||
        json['expectedSourceRevision'] != action.expectedSourceRevision ||
        json['decision'] != 'retry' ||
        !const ['accepted', 'replayed', 'stale', 'not_recorded']
            .contains(json['state']) ||
        json['queryRequired'] is! bool ||
        json['queryRequired'] != (json['state'] == 'not_recorded')) {
      throw const FormatException('invalid_retry_receipt');
    }
    final accepted = const ['accepted', 'replayed'].contains(json['state']);
    if (accepted ? !_text(json['newEpisode']) : json['newEpisode'] != null) {
      throw const FormatException('invalid_retry_receipt');
    }
    return HttpRetryReceipt._(
        json['state'], json['newEpisode'], json['queryRequired']);
  }
}

/// A versioned read view; no fields are persisted into Task or Hive.
class HttpRecovery {
  final String taskId, status, management, reason;
  final bool retryAllowed;
  final String? episode;
  final int? sourceRevision, generation, totalUsed, transportUsed;
  final HttpRetryReceipt? receipt;
  HttpRecovery._(
      this.taskId,
      this.status,
      this.management,
      this.reason,
      this.retryAllowed,
      this.episode,
      this.sourceRevision,
      this.generation,
      this.totalUsed,
      this.transportUsed,
      this.receipt);
  factory HttpRecovery.parse(Map json, String taskId,
      {HttpRetryAction? action}) {
    final management = json['management'];
    if (json['version'] is! int ||
        json['version'] != 1 ||
        json['taskId'] != taskId ||
        !const ['ready', 'running', 'pause', 'wait', 'error', 'done']
            .contains(json['status']) ||
        !const ['managed', 'legacy', 'held'].contains(management) ||
        json['retryAllowed'] is! bool ||
        !const [
          'explicit_retry_available',
          'not_error',
          'not_managed',
          'recovery_held',
          'cooldown'
        ].contains(json['reason'])) {
      throw const FormatException('invalid_recovery_view');
    }
    if (management != 'managed') {
      if (json['retryAllowed'] != false ||
          json['reason'] !=
              (management == 'legacy' ? 'not_managed' : 'recovery_held') ||
          const [
            'episode',
            'sourceRevision',
            'generation',
            'totalUsed',
            'transportUsed',
            'lastExplicitRetry',
            'receipt'
          ].any(json.containsKey)) {
        throw const FormatException('invalid_recovery_view');
      }
      return HttpRecovery._(taskId, json['status'], management, json['reason'],
          false, null, null, null, null, null, null);
    }
    if (!_text(json['episode']) ||
        !_integer(json['sourceRevision'], minimum: 1) ||
        !_integer(json['generation']) ||
        !_integer(json['totalUsed']) ||
        !_integer(json['transportUsed']) ||
        (json['reason'] == 'cooldown' && json['retryAllowed'] != false) ||
        (json['retryAllowed'] == true &&
            (json['status'] != 'error' ||
                json['reason'] != 'explicit_retry_available'))) {
      throw const FormatException('invalid_recovery_view');
    }
    final last = json['lastExplicitRetry'];
    if (last != null &&
        (last is! Map ||
            last['version'] is! int ||
            last['version'] != 1 ||
            !_text(last['actionId']) ||
            !_text(last['fromEpisode']) ||
            !_text(last['toEpisode']) ||
            !_integer(last['sourceRevision'], minimum: 1))) {
      throw const FormatException('invalid_recovery_history');
    }
    HttpRetryReceipt? receipt;
    if (json['receipt'] != null) {
      if (action == null || json['receipt'] is! Map) {
        throw const FormatException('unassociated_retry_receipt');
      }
      receipt = HttpRetryReceipt.parse(json['receipt'], action);
      if (const ['accepted', 'replayed'].contains(receipt.state) &&
          (last is! Map ||
              last['actionId'] != action.actionId ||
              last['fromEpisode'] != action.expectedEpisode ||
              last['sourceRevision'] != action.expectedSourceRevision ||
              last['toEpisode'] != receipt.newEpisode)) {
        throw const FormatException('unassociated_retry_receipt');
      }
    } else if (action != null) {
      throw const FormatException('missing_retry_receipt');
    }
    return HttpRecovery._(
        taskId,
        json['status'],
        management,
        json['reason'],
        json['retryAllowed'],
        json['episode'],
        json['sourceRevision'],
        json['generation'],
        json['totalUsed'],
        json['transportUsed'],
        receipt);
  }
}
