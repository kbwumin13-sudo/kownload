import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/util/util.dart';

void main() {
  test('file sizes use binary unit labels for powers of 1024', () {
    expect(Util.fmtByte(1023), '1023 B');
    expect(Util.fmtByte(1024), '1.00 KiB');
    expect(Util.fmtByte(1024 * 1024), '1.00 MiB');
    expect(Util.fmtByte(8000000000), '7.45 GiB');
  });
}
