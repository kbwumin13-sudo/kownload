import 'package:flutter/material.dart';
import 'package:get/get.dart';
import '../../../routes/app_pages.dart';
import '../controllers/root_controller.dart';
import '../../create/controllers/create_controller.dart';
import '../../create/views/create_view.dart';
import '../../home/controllers/home_controller.dart';
import '../../home/views/home_view.dart';
import '../../task/bindings/task_binding.dart';
import '../../../../util/util.dart';
import '../../../../macos/mac_panel.dart';

class RootView extends GetView<RootController> {
  const RootView({super.key});
  @override
  Widget build(BuildContext context) {
    if (Util.isMacos()) return const _MacRoot();
    return GetRouterOutlet(initialRoute: Routes.HOME);
  }
}

class _MacRoot extends StatefulWidget {
  const _MacRoot();
  @override
  State<_MacRoot> createState() => _MacRootState();
}

class _MacRootState extends State<_MacRoot> {
  late final CreateView panel;
  @override
  void initState() {
    super.initState();
    if (!Get.isRegistered<CreateController>()) {
      Get.put(CreateController(), permanent: true);
    }
    if (!Get.isRegistered<HomeController>()) {
      Get.put(HomeController(), permanent: true);
    }
    TaskBinding().dependencies();
    panel = CreateView();
  }

  @override
  Widget build(BuildContext context) {
    return ValueListenableBuilder<String>(
      valueListenable: MacPanel.presentation,
      builder: (context, mode, _) => Stack(fit: StackFit.expand, children: [
        Offstage(offstage: mode != 'settings', child: const HomeView()),
        Offstage(offstage: mode == 'settings', child: panel),
      ]),
    );
  }
}
