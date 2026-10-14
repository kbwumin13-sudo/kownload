import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:gopeed/app/routes/app_pages.dart';
import 'package:gopeed/macos/mac_navigation.dart';

void main() {
  test('new download at startup retains the default task page underneath', () {
    Get.addPages(AppPages.routes);
    addTearDown(Get.reset);
    final create = GetNavConfig.fromRoute(Routes.CREATE)!;
    final retained = macContentRoute(create);
    expect(retained.uri.path, Routes.TASK);
    expect(retained.currentTreeBranch.any((page) => page.name == Routes.HOME), isTrue);
  });
}
