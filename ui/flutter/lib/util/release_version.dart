/// kownload's release sequence is separate from the three-part app version.
/// Legacy public tags used the same sequence, e.g. v1.9.3-kownload.8.
class ReleaseVersion implements Comparable<ReleaseVersion> {
  final int major;
  final int minor;
  final int patch;
  final int revision;

  const ReleaseVersion(this.major, this.minor, this.patch, this.revision);

  static ReleaseVersion? tryParse(String text) {
    final match = RegExp(r'^v?(\d+)\.(\d+)\.(\d+)(?:-kownload\.(\d+))?$')
        .firstMatch(text);
    if (match == null) return null;
    final values = [for (var i = 1; i <= 4; i++) int.tryParse(match[i] ?? '0')];
    if (values.any((value) => value == null)) return null;
    return ReleaseVersion(values[0]!, values[1]!, values[2]!, values[3]!);
  }

  @override
  int compareTo(ReleaseVersion other) {
    final left = [major, minor, patch, revision];
    final right = [other.major, other.minor, other.patch, other.revision];
    for (var i = 0; i < left.length; i++) {
      final result = left[i].compareTo(right[i]);
      if (result != 0) return result;
    }
    return 0;
  }

  @override
  String toString() => '$major.$minor.$patch-kownload.$revision';
}

class VersionInfo {
  final String version;
  final String changeLog;
  final Uri releasePage;

  const VersionInfo(this.version, this.changeLog, this.releasePage);
}

/// Only a final release with a recognized, strictly newer tag can be offered.
VersionInfo? newerRelease(Map<String, dynamic> json, String current) {
  if (json['draft'] == true || json['prerelease'] == true) return null;
  final tag = json['tag_name'];
  if (tag is! String) return null;
  final installed = ReleaseVersion.tryParse(current);
  final published = ReleaseVersion.tryParse(tag);
  if (installed == null ||
      published == null ||
      published.compareTo(installed) <= 0) {
    return null;
  }
  return VersionInfo(
      published.toString(),
      json['body'] is String ? json['body'] : '',
      Uri.https('github.com', '/kbwumin13-sudo/kownload/releases/tag/$tag'));
}
