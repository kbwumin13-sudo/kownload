import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/meta.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';
import 'package:gopeed/api/model/task.dart';
import 'package:gopeed/util/startup_tasks.dart';

Task task(String id, Status status,
        {Protocol? protocol, String? requestProtocol}) =>
    Task(
        id: id,
        name: id,
        meta: Meta(
            req: Request(
                url: 'https://example.com/file', protocol: requestProtocol),
            opts: Options()),
        status: status,
        uploading: false,
        progress: Progress(
            used: 0, speed: 0, downloaded: 0, uploadSpeed: 0, uploaded: 0),
        createdAt: DateTime.utc(2026),
        updatedAt: DateTime.utc(2026))
      ..protocol = protocol;

void main() {
  test(
      'restart resumes ordinary downloads and leaves cloud login recovery to UI',
      () async {
    final calls = <List<String>>[];
    await resumeStartupTasks(
        load: () async => [
              task('cloud', Status.pause, protocol: Protocol.netdisk),
              task('legacy-cloud', Status.error, requestProtocol: 'netdisk'),
              task('http', Status.pause, protocol: Protocol.http),
              task('media', Status.error, protocol: Protocol.media),
              task('done', Status.done),
              task('running', Status.running),
            ],
        resume: (ids) async {
          calls.add(ids);
        });
    expect(calls, [
      ['http', 'media']
    ]);
  });
  test('cloud-only and empty queues never send a wildcard resume', () async {
    for (final tasks in [
      <Task>[],
      [task('cloud', Status.pause, protocol: Protocol.netdisk)]
    ]) {
      var called = false;
      await resumeStartupTasks(
          load: () async => tasks,
          resume: (_) async {
            called = true;
          });
      expect(called, isFalse);
    }
  });
}
