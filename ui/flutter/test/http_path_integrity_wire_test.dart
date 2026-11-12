import 'dart:convert';
import 'dart:io';

import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/model/create_task_batch.dart';
import 'package:gopeed/api/model/http_path_create.dart';
import 'package:gopeed/api/model/options.dart';
import 'package:gopeed/api/model/request.dart';

void main() {
  test('path SHA serializer emits the complete strict integrity intent', () {
    const path = '/tmp/kownload-ui-path-sha';
    final request = HttpPathCreateRequest(
      CreateTaskBatch(reqs: [
        CreateTaskBatchItem(
          req: Request(
            url: 'https://fixture.invalid/ui-path-sha.bin',
            protocol: 'http',
            extra: ReqExtraHttp(),
          ),
          opts: Options(
            path: path,
            name: 'ui-path-sha.bin',
            extra: OptsExtraHttp(autoExtract: false, autoTorrent: false),
          ),
        ),
      ]),
      pathIntent: HttpPathIntent(path: path, origin: 'user_selected'),
      expectedSHA256: 'a' * 64,
    );
    final wire = request.toJson('ui-path-sha-serializer');
    // Optional test-only export: the Go receiver consumes these exact bytes,
    // rather than a separately handwritten copy of the wire contract.
    final fixturePath = Platform.environment['K16_PATH_INTEGRITY_JSON'];
    if (fixturePath != null) {
      File(fixturePath).writeAsBytesSync(utf8.encode('${jsonEncode(wire)}\n'));
    }
    expect(wire['integrity'], {
      'version': 1,
      'requiredCapability': 'http-sha256-completion/v1',
      'algorithm': 'sha256',
      'expectedSHA256': 'a' * 64,
    });
    expect(wire['requiredCapabilities'],
        [httpPathCapability, 'http-sha256-completion/v1']);
    expect(wire['request']['opts']['path'], wire['pathIntent']['path']);
    expect(wire['operationId'], 'ui-path-sha-serializer');
    expect(jsonDecode(jsonEncode(wire))['integrity'], wire['integrity']);
  });
}
