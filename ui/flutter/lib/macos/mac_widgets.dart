import 'package:flutter/cupertino.dart';
import 'package:flutter/material.dart';
import 'mac_glass.dart';

class MacColors {
  final bool dark;
  const MacColors(this.dark);
  static MacColors of(BuildContext context) =>
      MacColors(Theme.of(context).brightness == Brightness.dark);
  Color get background =>
      dark ? const Color(0xff1f2022) : const Color(0xfffaf9f7);
  Color get surface => dark ? const Color(0xff292a2d) : Colors.white;
  Color get sidebar => dark ? const Color(0xff28292b) : const Color(0xffecebea);
  Color get text => dark ? const Color(0xfff0f0f2) : const Color(0xff242426);
  Color get secondary =>
      dark ? const Color(0xffa6a6ad) : const Color(0xff737378);
  Color get border =>
      (dark ? Colors.white : Colors.black).withValues(alpha: dark ? .12 : .10);
  Color get accent => dark ? const Color(0xff79cca0) : const Color(0xff258350);
  Color get primaryFill =>
      dark ? const Color(0xff79cca0) : const Color(0xff258350);
  Color get primaryText => dark ? const Color(0xff10291b) : Colors.white;
  Color get windowTop =>
      (dark ? const Color(0xff252629) : const Color(0xfff6f5f3))
          .withValues(alpha: .08);
  Color get windowBottom => windowTop;
  Color get selection => accent.withValues(alpha: dark ? .20 : .12);
}

class MacToolbar extends StatelessWidget {
  final String title;
  final List<Widget> actions;
  final Widget? leading;
  final Widget? bottom;
  const MacToolbar(
      {super.key,
      required this.title,
      this.actions = const [],
      this.leading,
      this.bottom});
  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return Container(
        decoration: BoxDecoration(
            color: c.background,
            border: Border(bottom: BorderSide(color: c.border, width: .7))),
        child: Column(mainAxisSize: MainAxisSize.min, children: [
          SizedBox(
              height: MacMetrics.toolbarHeight,
              child: Padding(
                  padding: const EdgeInsets.symmetric(horizontal: 16),
                  child: Row(children: [
                    if (leading != null) ...[
                      leading!,
                      const SizedBox(width: 8)
                    ],
                    Expanded(
                        child: title.isEmpty
                            ? const SizedBox.shrink()
                            : Text(title,
                                maxLines: 1,
                                overflow: TextOverflow.ellipsis,
                                style: TextStyle(
                                    fontSize: 14,
                                    fontWeight: FontWeight.w600,
                                    color: c.text))),
                    ...actions.expand((w) => [const SizedBox(width: 8), w]),
                  ]))),
          if (bottom != null)
            Padding(
                padding: const EdgeInsets.fromLTRB(16, 0, 16, 12),
                child: bottom!),
        ]));
  }
}

class MacPage extends StatelessWidget {
  final String title;
  final Widget child;
  final List<Widget> actions;
  final Widget? leading;
  final Widget? bottom;
  const MacPage(
      {super.key,
      required this.title,
      required this.child,
      this.actions = const [],
      this.leading,
      this.bottom});
  @override
  Widget build(BuildContext context) => Material(
      color: MacColors.of(context).background,
      child: Column(children: [
        MacToolbar(
            title: title, actions: actions, leading: leading, bottom: bottom),
        Expanded(child: child)
      ]));
}

class MacButton extends StatefulWidget {
  final Widget child;
  final VoidCallback? onPressed;
  final bool primary;
  final IconData? icon;
  const MacButton(
      {super.key,
      required this.child,
      this.onPressed,
      this.primary = false,
      this.icon});
  @override
  State<MacButton> createState() => _MacButtonState();
}

class _MacButtonState extends State<MacButton> {
  bool hover = false;
  bool focused = false;
  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    final foreground = widget.primary ? c.primaryText : c.text;
    final enabled = widget.onPressed != null;
    final fill = widget.primary && enabled ? c.primaryFill : c.surface;
    return SizedBox(
      height: MacMetrics.controlHeight,
      child: MouseRegion(
        cursor: enabled ? SystemMouseCursors.click : SystemMouseCursors.basic,
        onEnter: (_) => setState(() => hover = true),
        onExit: (_) => setState(() => hover = false),
        child: TextButton(
          onPressed: widget.onPressed,
          onFocusChange: (value) => setState(() => focused = value),
          style: TextButton.styleFrom(
            foregroundColor: foreground,
            disabledForegroundColor: c.secondary.withValues(alpha: .55),
            backgroundColor: hover && enabled
                ? Color.alphaBlend(c.text.withValues(alpha: .04), fill)
                : fill,
            minimumSize: const Size(32, MacMetrics.controlHeight),
            tapTargetSize: MaterialTapTargetSize.shrinkWrap,
            padding: const EdgeInsets.symmetric(horizontal: 11),
            side: BorderSide(
                color: focused
                    ? c.accent
                    : widget.primary && enabled
                        ? Colors.transparent
                        : c.border,
                width: focused ? 2 : .7),
            shape: RoundedRectangleBorder(
                borderRadius: BorderRadius.circular(MacMetrics.controlRadius)),
          ),
          child: DefaultTextStyle.merge(
            style: TextStyle(
                fontSize: 13,
                fontWeight: FontWeight.w500,
                color:
                    enabled ? foreground : c.secondary.withValues(alpha: .55)),
            child: Row(mainAxisSize: MainAxisSize.min, children: [
              if (widget.icon != null) ...[
                Icon(widget.icon,
                    size: 16,
                    color: enabled
                        ? foreground
                        : c.secondary.withValues(alpha: .55)),
                const SizedBox(width: 6),
              ],
              widget.child,
            ]),
          ),
        ),
      ),
    );
  }
}

class MacSearchField extends StatelessWidget {
  final TextEditingController controller;
  final String hint;
  final ValueChanged<String>? onChanged;
  final FocusNode? focusNode;
  const MacSearchField(
      {super.key,
      required this.controller,
      this.hint = '搜索',
      this.onChanged,
      this.focusNode});
  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return SizedBox(
        height: MacMetrics.controlHeight,
        child: CupertinoTextField(
          controller: controller,
          focusNode: focusNode,
          onChanged: onChanged,
          padding: const EdgeInsets.symmetric(horizontal: 8, vertical: 6),
          prefix: Padding(
              padding: const EdgeInsets.only(left: 9),
              child: MacSymbol('magnifyingglass',
                  fallback: CupertinoIcons.search,
                  size: 15,
                  color: c.secondary)),
          placeholder: hint,
          placeholderStyle: TextStyle(fontSize: 13, color: c.secondary),
          style: TextStyle(
              fontFamily: '.AppleSystemUIFont', fontSize: 13, color: c.text),
          cursorColor: c.accent,
          decoration: BoxDecoration(
              color: c.surface,
              border: Border.all(color: c.border, width: .7),
              borderRadius: BorderRadius.circular(MacMetrics.controlRadius)),
          clearButtonMode: OverlayVisibilityMode.editing,
        ));
  }
}

class MacEmptyState extends StatelessWidget {
  final IconData icon;
  final String? symbol;
  final String title;
  final String? message;
  final Widget? action;
  const MacEmptyState(
      {super.key,
      required this.icon,
      this.symbol,
      required this.title,
      this.message,
      this.action});
  @override
  Widget build(BuildContext context) {
    final c = MacColors.of(context);
    return Center(
        child: Padding(
            padding: const EdgeInsets.all(24),
            child: Column(mainAxisSize: MainAxisSize.min, children: [
              MacSymbol(symbol ?? _symbolForIcon(icon),
                  fallback: icon, size: 44, color: c.secondary),
              const SizedBox(height: 16),
              Text(title,
                  textAlign: TextAlign.center,
                  style: TextStyle(
                      fontSize: 17,
                      fontWeight: FontWeight.w600,
                      letterSpacing: -.4,
                      color: c.text)),
              if (message != null) ...[
                const SizedBox(height: 10),
                Text(message!,
                    textAlign: TextAlign.center,
                    style: TextStyle(
                        fontSize: 13, color: c.secondary, height: 1.5))
              ],
              if (action != null) ...[const SizedBox(height: 18), action!],
            ])));
  }

  static String _symbolForIcon(IconData icon) {
    if (icon == CupertinoIcons.search) return 'magnifyingglass';
    if (icon == CupertinoIcons.exclamationmark_circle ||
        icon == Icons.error_outline) return 'exclamationmark.triangle';
    if (icon == CupertinoIcons.square_stack ||
        icon == CupertinoIcons.square_stack_3d_up)
      return 'puzzlepiece.extension';
    return 'tray.and.arrow.down';
  }
}
