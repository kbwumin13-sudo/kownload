import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/task.dart';

Task task({
  String protocol = 'bt',
  String status = 'running',
  Map<String, dynamic>? res,
}) =>
    Task.fromJson({
      'id': 't',
      'name': '8a55cfbd5ca5d11507364765936c4f9e55b253ed',
      'protocol': protocol,
      'meta': {
        'req': {'url': 'magnet:?xt=urn:btih:8a55cfbd5ca5d11507364765936c4f9e55b253ed'},
        'opts': {'path': '/Users/me/Downloads', 'selectFiles': <int>[]},
        'res': res,
      },
      'status': status,
      'uploading': true,
      'progress': {
        'used': 0,
        'speed': 0,
        'downloaded': 0,
        'uploadSpeed': 0,
        'uploaded': 0,
      },
      'createdAt': '2026-11-08T12:00:00Z',
      'updatedAt': '2026-11-08T12:00:00Z',
    });

void main() {
  test('a running magnet without metadata is fetching torrent info', () {
    expect(task().fetchingTorrentInfo, isTrue);
  });

  test('metadata, another state or another protocol is not', () {
    final res = {
      'name': '',
      'size': 1466714112,
      'range': true,
      'files': [
        {'name': 'ubuntu-22.04-live-server-amd64.iso', 'path': '', 'size': 1466714112},
      ],
      'hash': '8a55cfbd5ca5d11507364765936c4f9e55b253ed',
    };
    expect(task(res: res).fetchingTorrentInfo, isFalse);
    expect(task(status: 'pause').fetchingTorrentInfo, isFalse);
    expect(task(status: 'error').fetchingTorrentInfo, isFalse);
    expect(task(protocol: 'http').fetchingTorrentInfo, isFalse);
  });
}
