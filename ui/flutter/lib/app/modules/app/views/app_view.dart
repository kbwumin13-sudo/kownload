import 'package:flutter/material.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:flutter_localizations/flutter_localizations.dart';
import 'package:get/get.dart';
import 'package:window_manager/window_manager.dart'; // Import the required packages

import '../../../../i18n/message.dart';
import '../../../../theme/theme.dart';
import '../../../../macos/mac_theme.dart';
import '../../../../macos/mac_appearance.dart';
import '../../../../util/locale_manager.dart';
import '../../../../util/util.dart'; // Import the required packages
import '../../../routes/app_pages.dart';
import '../controllers/app_controller.dart';

class AppView extends GetView<AppController> {
  const AppView({Key? key}) : super(key: key);

  @override
  Widget build(BuildContext context) {
    final config = controller.downloaderConfig.value;
    return WithForegroundTask(
      child: GetMaterialApp.router(
        useInheritedMediaQuery: true,
        debugShowCheckedModeBanner: false,
        theme: Util.isMacos() ? macTheme(Brightness.light) : GopeedTheme.light,
        darkTheme:
            Util.isMacos() ? macTheme(Brightness.dark) : GopeedTheme.dark,
        themeMode: ThemeMode.values.byName(config.extra.themeMode),
        translations: messages,
        locale: toLocale(config.extra.locale),
        fallbackLocale: fallbackLocale,
        localizationsDelegates: const [
          GlobalMaterialLocalizations.delegate,
          GlobalWidgetsLocalizations.delegate,
          GlobalCupertinoLocalizations.delegate,
        ],
        supportedLocales: messages.keys.keys.map((e) => toLocale(e)).toList(),
        getPages: AppPages.routes,

        // Add listening to theme changes, set the title bar color according to the current system theme.
        builder: (context, child) {
          // if platform is desktop
          if (Util.isDesktop()) {
            // actual brightness of the UI
            Brightness brightness = Theme.of(context).brightness;
            // Set the title bar to use the actual brightness of the UI
            windowManager.setBrightness(brightness);
          }
          // Fix for GetX Overlay issue with Flutter 3.38.1+
          // Reference: https://github.com/jonataslaw/getx/issues/3425
          final overlay = Overlay(
            initialEntries: [OverlayEntry(builder: (_) => child!)],
          );
          if (!Util.isMacos()) return overlay;
          return ValueListenableBuilder<MacAppearanceState>(
            valueListenable: MacAppearance.state,
            builder: (context, appearance, _) => MediaQuery(
              data: MediaQuery.of(context)
                  .copyWith(disableAnimations: appearance.reduceMotion),
              child: overlay,
            ),
          );
        },
      ),
    );
  }
}
