import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/util/release_version.dart';

void main() {
  Map<String, dynamic> release(String tag) =>
      {'tag_name': tag, 'body': 'Changes'};
  test('build25 never offers public build19 or an equal release', () {
    expect(newerRelease(release('v1.9.3-kownload.8'), '1.9.3-kownload.14'),
        isNull);
    expect(newerRelease(release('v1.9.3-kownload.14'), '1.9.3-kownload.14'),
        isNull);
  });
  test('release revision and base version have ordered numeric semantics', () {
    expect(
        newerRelease(release('v1.9.3-kownload.15'), '1.9.3-kownload.14')!
            .releasePage,
        Uri.parse(
            'https://github.com/kbwumin13-sudo/kownload/releases/tag/v1.9.3-kownload.15'));
    expect(newerRelease(release('v1.10.0-kownload.1'), '1.9.99-kownload.100'),
        isNotNull);
    expect(newerRelease(release('v1.8.99-kownload.100'), '1.9.3-kownload.14'),
        isNull);
  });
  test('unrecognized, malformed, draft and prerelease tags are skipped', () {
    for (final tag in [
      'v1.9.4-beta.1',
      'v1.9',
      'v1.9.3-kownload.x',
      '../../wrong',
      ''
    ]) {
      expect(newerRelease(release(tag), '1.9.3-kownload.14'), isNull);
    }
    expect(
        newerRelease(
            {...release('v2.0.0'), 'draft': true}, '1.9.3-kownload.14'),
        isNull);
    expect(
        newerRelease(
            {...release('v2.0.0'), 'prerelease': true}, '1.9.3-kownload.14'),
        isNull);
    expect(newerRelease(release('v2.0.0'), 'unknown'), isNull);
    expect(newerRelease({'tag_name': 42}, '1.9.3-kownload.14'), isNull);
  });
}
