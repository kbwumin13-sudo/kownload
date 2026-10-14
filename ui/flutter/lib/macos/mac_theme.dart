import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'mac_widgets.dart';

ThemeData macTheme(Brightness brightness) {
  final c = MacColors(brightness == Brightness.dark);
  final base = ThemeData(
      brightness: brightness,
      useMaterial3: false,
      platform: TargetPlatform.macOS,
      fontFamily: '.AppleSystemUIFont',
      visualDensity: VisualDensity.standard,
      colorScheme:
          ColorScheme.fromSeed(seedColor: c.accent, brightness: brightness)
              .copyWith(
                  primary: c.accent,
                  secondary: c.accent,
                  surface: c.surface,
                  onSurface: c.text));
  final border = OutlineInputBorder(
      borderRadius: BorderRadius.circular(7),
      borderSide: BorderSide(color: c.border));
  return base.copyWith(
    scaffoldBackgroundColor: Colors.transparent,
    primaryColor: c.accent,
    canvasColor: c.surface,
    dividerColor: c.border,
    textTheme: base.textTheme
        .apply(bodyColor: c.text, displayColor: c.text)
        .copyWith(
            bodyMedium: TextStyle(fontSize: 13, height: 1.4, color: c.text),
            bodyLarge: TextStyle(fontSize: 13, color: c.text),
            titleMedium: TextStyle(
                fontSize: 14, fontWeight: FontWeight.w600, color: c.text),
            titleLarge: TextStyle(
                fontSize: 17, fontWeight: FontWeight.w600, color: c.text),
            labelLarge:
                const TextStyle(fontSize: 13, fontWeight: FontWeight.w500)),
    cupertinoOverrideTheme:
        CupertinoThemeData(brightness: brightness, primaryColor: c.accent),
    appBarTheme: AppBarTheme(
        elevation: 0,
        backgroundColor: c.background,
        foregroundColor: c.text,
        centerTitle: false,
        titleTextStyle:
            TextStyle(fontSize: 16, fontWeight: FontWeight.w600, color: c.text),
        toolbarHeight: 56),
    iconTheme: IconThemeData(color: c.secondary, size: 18),
    iconButtonTheme: IconButtonThemeData(
        style: IconButton.styleFrom(
            minimumSize: const Size(28, 28), padding: const EdgeInsets.all(5))),
    inputDecorationTheme: InputDecorationTheme(
        isDense: true,
        filled: true,
        fillColor: c.surface,
        contentPadding: const EdgeInsets.symmetric(horizontal: 10, vertical: 8),
        border: border,
        enabledBorder: border,
        focusedBorder: border.copyWith(
            borderSide: BorderSide(color: c.accent, width: 1.5)),
        labelStyle: TextStyle(fontSize: 12, color: c.secondary),
        hintStyle: TextStyle(fontSize: 12, color: c.secondary)),
    dialogTheme: DialogThemeData(
        backgroundColor: c.surface,
        elevation: 18,
        shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(12),
            side: BorderSide(color: c.border)),
        titleTextStyle:
            TextStyle(fontSize: 16, fontWeight: FontWeight.w600, color: c.text),
        contentTextStyle: TextStyle(fontSize: 13, color: c.text)),
    cardTheme: CardThemeData(
        elevation: 0,
        color: c.surface,
        margin: const EdgeInsets.symmetric(vertical: 4),
        shape: RoundedRectangleBorder(
            borderRadius: BorderRadius.circular(9),
            side: BorderSide(color: c.border))),
    listTileTheme: ListTileThemeData(
        dense: true,
        minVerticalPadding: 8,
        iconColor: c.secondary,
        textColor: c.text,
        contentPadding:
            const EdgeInsets.symmetric(horizontal: 16, vertical: 2)),
    textButtonTheme: TextButtonThemeData(
        style: TextButton.styleFrom(
            foregroundColor: c.accent,
            textStyle: const TextStyle(fontSize: 13),
            shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(7)))),
    outlinedButtonTheme: OutlinedButtonThemeData(
        style: OutlinedButton.styleFrom(
            foregroundColor: c.text,
            minimumSize: const Size(30, 30),
            side: BorderSide(color: c.border),
            shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(7)))),
    elevatedButtonTheme: ElevatedButtonThemeData(
        style: ElevatedButton.styleFrom(
            backgroundColor: c.accent,
            foregroundColor: c.dark ? const Color(0xff102b1c) : Colors.white,
            elevation: 0,
            minimumSize: const Size(30, 30),
            shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(7)))),
    tabBarTheme: TabBarThemeData(
        labelColor: c.accent,
        unselectedLabelColor: c.secondary,
        indicatorSize: TabBarIndicatorSize.label,
        labelStyle: const TextStyle(fontSize: 12, fontWeight: FontWeight.w500)),
    tooltipTheme:
        const TooltipThemeData(waitDuration: Duration(milliseconds: 600)),
    progressIndicatorTheme:
        ProgressIndicatorThemeData(color: c.accent, linearTrackColor: c.border),
    splashFactory: NoSplash.splashFactory,
    highlightColor: Colors.transparent,
  );
}
