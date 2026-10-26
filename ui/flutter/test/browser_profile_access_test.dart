import 'package:flutter/services.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:gopeed/api/netdisk.dart';
import 'package:gopeed/util/browser_profile_access.dart';

void main() {
  TestWidgetsFlutterBinding.ensureInitialized();
  final messenger =
      TestDefaultBinaryMessengerBinding.instance.defaultBinaryMessenger;
  tearDown(() =>
      messenger.setMockMethodCallHandler(BrowserProfileAccess.channel, null));
  test('profile metadata is read through the app native permission scope',
      () async {
    messenger.setMockMethodCallHandler(BrowserProfileAccess.channel,
        (call) async {
      expect(call.method, 'read');
      expect(call.arguments, {'path': '/chosen/Local State'});
      return '{"profile":{"info_cache":{"Default":{"name":"学习"}}}}';
    });
    expect(
        await BrowserProfileAccess.read('/chosen/Local State'), contains('学习'));
  });
  test('native picker receives the existing path and supports cancellation',
      () async {
    messenger.setMockMethodCallHandler(BrowserProfileAccess.channel,
        (call) async {
      expect(call.method, 'choose');
      expect(call.arguments, {'title': '允许读取', 'path': '/chosen/Local State'});
      return null;
    });
    expect(
        await BrowserProfileAccess.choose(
            title: '允许读取', path: '/chosen/Local State'),
        isNull);
  });
  test('native file permission errors retain an actionable reason', () async {
    messenger.setMockMethodCallHandler(BrowserProfileAccess.channel, (_) async {
      throw PlatformException(
          code: 'profile_permission_denied', message: '请选择资料文件允许读取');
    });
    await expectLater(
        BrowserProfileAccess.read('/chosen/Local State'),
        throwsA(isA<NetdiskException>().having(
            (e) => e.reasonCode, 'reason', 'profile_permission_denied')));
  });
}
