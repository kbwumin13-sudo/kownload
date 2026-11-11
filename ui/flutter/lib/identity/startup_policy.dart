import 'dart:convert';

/// Launch admission is separate from the user's persisted auto-start choice.
/// It never grants a task permission that its protocol/state guard rejects.
class IdentityStartupPolicy {
  final String mode;
  final String reason;

  const IdentityStartupPolicy.normal()
      : mode = 'normal',
        reason = 'fresh';
  const IdentityStartupPolicy._(this.mode, this.reason);

  bool get requiresReview => mode == 'imported-review';

  factory IdentityStartupPolicy.fromMarker(String marker) {
    final value = jsonDecode(marker);
    if (value is! Map || value['version'] is! int || value['version'] != 1) {
      throw const FormatException('unsupported_startup_policy');
    }
    final policy = value['startupPolicy'];
    if (policy == null) {
      return const IdentityStartupPolicy._('imported-review', 'legacy_marker');
    }
    if (policy is Map && policy['version'] is int && policy['version'] == 1) {
      final mode = policy['mode'], reason = policy['reason'];
      if (mode == 'normal' &&
          ((reason == 'fresh' && value['imported'] == false) ||
              reason == 'confirmed_compatible')) {
        return IdentityStartupPolicy._(mode, reason);
      }
      if (mode == 'imported-review' &&
          const ['imported_data', 'legacy_marker'].contains(reason)) {
        return IdentityStartupPolicy._(mode, reason);
      }
    }
    throw const FormatException('unsupported_startup_policy');
  }
}

Future<bool> resumeIdentityTasks({
  required IdentityStartupPolicy policy,
  required bool autoStartTasks,
  required bool isolated,
  required Future<void> Function() resume,
}) async {
  if (policy.requiresReview || !autoStartTasks || isolated) return false;
  await resume();
  return true;
}
