import 'dart:convert';
import 'dart:typed_data';
import 'package:flutter/material.dart';
import 'package:flutter/rendering.dart';
import 'mac_appearance.dart';

class MacMetrics {
  static const titlebarHeight = 0.0;
  static const toolbarHeight = 44.0;
  static const controlHeight = 28.0;
  static const controlRadius = 6.0;
  static const sidebarWidth = 208.0;
}

/// AppKit supplies the material; Flutter retains focus, semantics and actions.
class MacGlassSurface extends StatefulWidget {
  final Widget child;
  final double radius;
  final Color? tint;
  final bool shadow;
  const MacGlassSurface(
      {super.key,
      required this.child,
      this.radius = MacMetrics.controlRadius,
      this.tint,
      this.shadow = false});
  @override
  State<MacGlassSurface> createState() => _MacGlassSurfaceState();
}

class _MacGlassSurfaceState extends State<MacGlassSurface> {
  static final Map<int, _MacGlassSurfaceState> _surfaces = {};
  static int _sequence = 0;
  static bool _scheduled = false;
  static bool _tracking = false;
  static String _last = '';
  final _key = GlobalKey();
  late final int _id;
  @override
  void initState() {
    super.initState();
    _id = ++_sequence;
    _surfaces[_id] = this;
    if (!_tracking) {
      _tracking = true;
      WidgetsBinding.instance.addPersistentFrameCallback((_) {
        if (_surfaces.isNotEmpty) _schedule();
      });
    }
  }

  @override
  void dispose() {
    _surfaces.remove(_id);
    _schedule();
    super.dispose();
  }

  static void _schedule() {
    if (_scheduled) return;
    _scheduled = true;
    WidgetsBinding.instance.addPostFrameCallback((_) {
      _scheduled = false;
      final regions = <Map<String, Object>>[];
      if (MacAppearance.state.value.supportsGlass &&
          !MacAppearance.state.value.reduceTransparency) {
        for (final entry in _surfaces.entries) {
          final state = entry.value;
          final context = state._key.currentContext;
          final box = context?.findRenderObject();
          if (box is! RenderBox ||
              !box.attached ||
              !box.hasSize ||
              box.size.isEmpty) continue;
          var hidden = false;
          context!.visitAncestorElements((element) {
            if (element.widget is Offstage &&
                (element.widget as Offstage).offstage) hidden = true;
            return !hidden;
          });
          if (hidden) continue;
          final origin = box.localToGlobal(Offset.zero);
          final bounds = origin & box.size;
          var visible = bounds;
          RenderObject child = box;
          while (child.parent is RenderObject) {
            final parent = child.parent!;
            final clip = parent.describeApproximatePaintClip(child);
            if (clip != null) {
              visible = visible.intersect(
                  MatrixUtils.transformRect(parent.getTransformTo(null), clip));
            }
            child = parent;
          }
          if (visible.isEmpty) continue;
          regions.add({
            'id': entry.key,
            'x': origin.dx,
            'y': origin.dy,
            'width': box.size.width,
            'height': box.size.height,
            'clipX': visible.left,
            'clipY': visible.top,
            'clipWidth': visible.width,
            'clipHeight': visible.height,
            'radius': state.widget.radius,
            'tint': state.widget.tint?.toARGB32() ?? 0
          });
        }
      }
      final encoded = jsonEncode(regions);
      if (encoded != _last) {
        _last = encoded;
        MacAppearance.setGlassRegions(regions);
      }
    });
  }

  @override
  Widget build(BuildContext context) {
    _schedule();
    final dark = Theme.of(context).brightness == Brightness.dark;
    final opaque = MacAppearance.state.value.reduceTransparency;
    final native = MacAppearance.state.value.supportsGlass &&
        !MacAppearance.state.value.reduceTransparency;
    return Container(
      key: _key,
      decoration: BoxDecoration(
          color: opaque
              ? (dark ? const Color(0xff2a2a2d) : const Color(0xfff5f5f7))
              : null,
          borderRadius: BorderRadius.circular(widget.radius),
          gradient: native || opaque
              ? null
              : LinearGradient(
                  begin: Alignment.topLeft,
                  end: Alignment.bottomRight,
                  colors: [
                      Colors.white.withValues(alpha: dark ? .10 : .62),
                      Colors.white.withValues(alpha: dark ? .035 : .30)
                    ]),
          border: Border.all(
              color: Colors.white.withValues(alpha: dark ? .12 : .38),
              width: .7),
          boxShadow: widget.shadow
              ? [
                  BoxShadow(
                      color: Colors.black.withValues(alpha: .09),
                      blurRadius: 16,
                      offset: const Offset(0, 5))
                ]
              : null),
      child: widget.child,
    );
  }
}

class MacSymbol extends StatelessWidget {
  final String name;
  final IconData fallback;
  final double size;
  final Color? color;
  static final Map<String, Future<Uint8List?>> _cache = {};
  const MacSymbol(this.name,
      {super.key, required this.fallback, this.size = 18, this.color});
  @override
  Widget build(BuildContext context) {
    final request = _cache.putIfAbsent(
        '$name/$size', () => MacAppearance.symbol(name, size * 2));
    return SizedBox(
        width: size,
        height: size,
        child: FutureBuilder<Uint8List?>(
            future: request,
            builder: (context, result) {
              if (result.data == null)
                return Icon(fallback, size: size, color: color);
              return Image.memory(result.data!,
                  width: size,
                  height: size,
                  color: color,
                  colorBlendMode: BlendMode.srcIn,
                  filterQuality: FilterQuality.high);
            }));
  }
}
