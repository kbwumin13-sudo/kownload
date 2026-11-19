import 'package:json_annotation/json_annotation.dart';

import 'resource.dart';

part 'resolve_result.g.dart';

@JsonSerializable(explicitToJson: true)
class ResolveResult {
  String id;
  Resource res;
  @JsonKey(includeToJson: false)
  final bool httpInlineHtml;

  ResolveResult({
    this.id = "",
    required this.res,
    this.httpInlineHtml = false,
  });

  factory ResolveResult.fromJson(Map<String, dynamic> json) =>
      _$ResolveResultFromJson(json);
  Map<String, dynamic> toJson() => _$ResolveResultToJson(this);
}
