import 'package:get/get.dart';
import '../app/routes/app_pages.dart';

/// Keep the originating home page mounted beneath a create-task sheet.
GetNavConfig macContentRoute(GetNavConfig current) {
  if (current.uri.path != Routes.CREATE) return current;
  for (final previous in Get.rootDelegate.history.reversed.skip(1)) {
    if (previous.currentTreeBranch.any((page) => page.name == Routes.HOME)) {
      return previous;
    }
  }
  return GetNavConfig.fromRoute(Routes.TASK) ?? current;
}
