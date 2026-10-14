import 'dart:io';

import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter_test/flutter_test.dart';
import 'package:get/get.dart';
import 'package:gopeed/api/model/store_extension.dart';
import 'package:gopeed/api/model/extension.dart';
import 'package:gopeed/app/modules/extension/controllers/extension_controller.dart';
import 'package:gopeed/app/modules/extension/views/extension_card.dart';
import 'package:gopeed/macos/mac_widgets.dart';

void main() {
  testWidgets('macOS extension row keeps install and detail actions',
      (tester) async {
    if (!Platform.isMacOS) return;
    var installs = 0;
    var details = 0;
    final item = ExtensionListItem(
      store: StoreExtension(
        id: 'sample',
        repoFullName: 'example/sample',
        repoUrl: 'https://github.com/example/sample',
        name: 'sample',
        author: 'Example',
        title: 'Sample Extension',
        description: 'A sample extension',
        version: '1.0.0',
        installCount: 4,
        stars: 2,
        topics: const [],
      ),
    );

    await tester.pumpWidget(GetMaterialApp(
      home: Scaffold(
          body: ExtensionCard(
        item: item,
        busy: false,
        canUpdate: false,
        onTap: () => details++,
        onInstall: () => installs++,
      )),
    ));

    expect(find.text('Sample Extension'), findsOneWidget);
    expect(find.textContaining('A sample extension'), findsOneWidget);
    await tester.tap(find.text('Sample Extension'));
    expect(details, 1);
    await tester.tap(find.byType(MacButton));
    expect(installs, 1);
  });

  testWidgets('macOS installed row keeps switch, settings and delete actions',
      (tester) async {
    if (!Platform.isMacOS) return;
    bool? switchedTo;
    var settings = 0;
    var deletes = 0;
    final extension = Extension(
      identity: 'sample',
      name: 'sample',
      author: 'Example',
      title: 'Sample Extension',
      description: 'Installed',
      icon: '',
      version: '1.0.0',
      homepage: '',
      repository: null,
      disabled: false,
      devMode: false,
      devPath: '',
    );
    extension.settings = [
      Setting(
        name: 'token',
        title: 'Token',
        description: '',
        required: false,
        type: SettingType.string,
      )
    ];

    await tester.pumpWidget(GetMaterialApp(
      home: Scaffold(
          body: ExtensionCard(
        item: ExtensionListItem(installed: extension),
        busy: false,
        canUpdate: false,
        onToggle: (value) async {
          switchedTo = value;
        },
        onOpenSetting: () => settings++,
        onDelete: () => deletes++,
      )),
    ));

    await tester.tap(find.byType(Switch));
    expect(switchedTo, false);
    await tester.tap(find.byIcon(CupertinoIcons.gear));
    expect(settings, 1);
    await tester.tap(find.byIcon(CupertinoIcons.trash));
    expect(deletes, 1);
  });
}
