import 'package:json_annotation/json_annotation.dart';

import 'options.dart';
import 'browser_handoff.dart';
import 'request.dart';

part 'create_task.g.dart';

@JsonSerializable(explicitToJson: true)
class CreateTask {
  /// Transient App confirmation identity; never sent to the download engine.
  @JsonKey(includeFromJson: false, includeToJson: false)
  String? browserOperationId;
  @JsonKey(includeFromJson: false, includeToJson: false)
  BrowserHandoffContext? browserHandoff;
  @JsonKey(includeFromJson: false, includeToJson: false)
  bool browserCapture = false;
  String? rid;
  Request? req;
  Options? opts;

  CreateTask({
    this.rid,
    this.req,
    this.opts,
  });

  factory CreateTask.fromJson(
    Map<String, dynamic> json,
  ) =>
      _$CreateTaskFromJson(json);

  Map<String, dynamic> toJson() => _$CreateTaskToJson(this);
}
