/// Provenance of this UI draft only. It is not a backend path decision or an
/// execution permission, and is never added to legacy request serialization.
enum PathOrigin {
  defaultPath,
  userSelected,
  manualCategory,
  sourceSuggested,
  unknown,
}

/// An immutable view of the current draft's path and the event that supplied it.
class PathIntent {
  final String path;
  final PathOrigin origin;

  const PathIntent({required this.path, required this.origin});
}
