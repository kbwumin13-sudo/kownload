import 'package:args/args.dart';
import 'package:flutter/foundation.dart';
import 'package:flutter/material.dart';
import 'package:flutter/services.dart';
import 'package:flutter_foreground_task/flutter_foreground_task.dart';
import 'package:get/get.dart';
import 'package:gopeed/util/analytics.dart';
import 'package:hotkey_manager/hotkey_manager.dart';
import 'package:window_manager/window_manager.dart';

import 'api/api.dart' as api;
import 'api/model/task.dart' show Status;
import 'app/modules/app/controllers/app_controller.dart';
import 'app/modules/app/views/app_view.dart';
import 'core/libgopeed_boot.dart';
import 'database/database.dart';
import 'i18n/message.dart';
import 'util/browser_extension_host/browser_extension_host.dart';
import 'util/locale_manager.dart';
import 'util/log_util.dart';
import 'util/package_info.dart';
import 'util/scheme_register/scheme_register.dart';
import 'util/updater.dart';
import 'util/util.dart';
import 'util/startup_tasks.dart';
import 'macos/mac_appearance.dart';
import 'macos/mac_panel.dart';
import 'app/modules/task/controllers/task_controller.dart';

class StartupArgs {
  static const flagHidden = "hidden";

  /// Command line --hidden flag (for auto-start)
  bool hiddenFromArgs = false;
  String? previewStorage;

  StartupArgs._();

  /// Parse from command line arguments only
  static StartupArgs parse(List<String> arguments) {
    final args = StartupArgs._();
    try {
      final parser = ArgParser()
        ..addFlag(flagHidden)
        ..addOption('preview-storage');
      final results = parser.parse(arguments);
      args.hiddenFromArgs = results.flag(flagHidden);
      args.previewStorage = results.option('preview-storage');
    } catch (e) {
      // ignore parse errors
    }
    return args;
  }
}

void main(List<String> arguments) async {
  WidgetsFlutterBinding.ensureInitialized();

  final args = StartupArgs.parse(arguments);

  await init(args);
  onStart();

  runApp(const AppView());
}

Future<void> init(StartupArgs args) async {
  // Note: WidgetsFlutterBinding.ensureInitialized() is already called in main()
  if (Util.isMobile()) {
    FlutterForegroundTask.initCommunicationPort();
  }
  await Util.initStorageDir(previewStorage: args.previewStorage);
  await Database.instance.init();
  if (Util.isDesktop()) {
    await windowManager.ensureInitialized();
    if (Util.isMacos()) await MacAppearance.initialize();
    final windowState = Database.instance.getWindowState();

    // Check if menubar mode is enabled (only for macOS)
    final runAsMenubarApp = Util.isMacos();

    final windowOptions = WindowOptions(
      size: Util.isMacos()
          ? const Size(640, 260)
          : Size(windowState?.width ?? 800, windowState?.height ?? 600),
      center: true,
      skipTaskbar: runAsMenubarApp,
    );
    await windowManager.waitUntilReadyToShow(windowOptions, () async {
      await windowManager.setPreventClose(true);
      if (Util.isMacos()) {
        await MacPanel.initialize();
        Database.instance.saveRunAsMenubarApp(true);
      }
    });

    // Register Cmd+W hotkey on macOS to close window
    if (Util.isMacos()) {
      await hotKeyManager.unregisterAll();
      HotKey hotKey = HotKey(
        key: PhysicalKeyboardKey.keyW,
        modifiers: [HotKeyModifier.meta],
        scope: HotKeyScope.inapp,
      );
      await hotKeyManager.register(
        hotKey,
        keyDownHandler: (hotKey) {
          MacPanel.hide();
        },
      );
    }
  }

  initLogger();

  try {
    await initPackageInfo();
  } catch (e) {
    logger.e("init package info fail", e);
  }

  final controller =
      Get.put(AppController(hiddenFromArgs: args.hiddenFromArgs));
  try {
    await controller.loadStartConfig();
    final startCfg = controller.startConfig.value;
    controller.runningPort.value = await LibgopeedBoot.instance.start(startCfg);
    api.init(startCfg.network, controller.runningAddress(), startCfg.apiToken);
  } catch (e) {
    logger.e("libgopeed init fail", e);
  }

  try {
    await controller.loadDownloaderConfig();
  } catch (e) {
    logger.e("load config fail", e);
  }

  if (Util.isMacos()) {
    final tasks = Get.put(MacTaskListController(), permanent: true);
    tasks.stop();
    MacPanel.visible.addListener(() {
      MacPanel.visible.value ? tasks.start() : tasks.stop();
    });
  }

  // Auto-start incomplete tasks if enabled
  if (controller.downloaderConfig.value.extra.autoStartTasks) {
    try {
      await resumeStartupTasks(
          load: () => api.getTasks(Status.values),
          resume: (ids) => api.continueAllTasks(ids));
      logger.i("auto-start tasks completed");
    } catch (e) {
      logger.w("auto-start tasks fail", e);
    }
  }

  () async {
    if (Util.isDesktop() && !Util.isIsolatedPreview) {
      try {
        registerUrlScheme("gopeed");
        if (controller.downloaderConfig.value.extra.defaultBtClient) {
          registerDefaultTorrentClient();
        }
      } catch (e) {
        logger.e("register scheme fail", e);
      }

      try {
        await installHost();
      } catch (e) {
        logger.e("browser extension host binary install fail", e);
      }
      for (final browser in Browser.values) {
        try {
          await installManifest(browser);
        } catch (e) {
          logger.e(
              "browser [${browser.name}] extension host integration fail", e);
        }
      }

      try {
        await installUpdater();
      } catch (e) {
        logger.e("updater install fail", e);
      }
    }
  }();
}

Future<void> onStart() async {
  // if is debug mode, check language message is complete,change debug locale to your comfortable language if you want
  if (kDebugMode) {
    final debugLang = getLocaleKey(debugLocale);
    final fullMessages = messages.keys[debugLang];
    messages.keys.keys.where((e) => e != debugLang).forEach((lang) {
      final langMessages = messages.keys[lang];
      if (langMessages == null) {
        logger.w("missing language: $lang");
        return;
      }
      final missingKeys =
          fullMessages!.keys.where((key) => langMessages[key] == null);
      if (missingKeys.isNotEmpty) {
        logger.w("missing language: $lang, keys: $missingKeys");
      }
    });
  }

  if (Config.isConfigured && Database.instance.getAnalyticsEnabled()) {
    try {
      await Analytics.instance.init();
      await Analytics.instance.logAppOpen();
    } catch (e) {
      logger.w("GA4 init failed: $e");
    }
  }
}
