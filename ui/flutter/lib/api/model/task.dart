import 'package:json_annotation/json_annotation.dart';

import 'meta.dart';
import 'browser_handoff.dart';
import 'http_completion_projection.dart';

part 'task.g.dart';

enum Status { ready, running, pause, wait, error, done }

enum Protocol { http, bt, ed2k, media, netdisk }

// ExtractStatus enum matching Go backend
enum ExtractStatus {
  @JsonValue('')
  none,
  @JsonValue('extracting')
  extracting,
  @JsonValue('done')
  done,
  @JsonValue('error')
  error,
  @JsonValue('waitingParts')
  waitingParts
}

@JsonSerializable(explicitToJson: true)
class Task {
  @JsonKey(includeFromJson: false, includeToJson: false)
  TaskHandoff? handoff;
  @JsonKey(includeFromJson: false, includeToJson: false)
  HttpCompletionProjection? completionView;
  String id;
  String name;
  Protocol? protocol;
  Meta meta;
  Status status;
  bool uploading;
  Progress progress;
  DateTime createdAt;
  DateTime updatedAt;

  Task({
    this.handoff,
    required this.id,
    required this.name,
    required this.meta,
    required this.status,
    required this.uploading,
    required this.progress,
    required this.createdAt,
    required this.updatedAt,
  });

  // Keep the existing generated model intact while accepting the new protocol.
  factory Task.fromJson(Map<String, dynamic> json) {
    final task = json['protocol'] == 'netdisk'
        ? (_$TaskFromJson({...json, 'protocol': null})
          ..protocol = Protocol.netdisk)
        : _$TaskFromJson(json);
    if (json['handoff'] is Map)
      task.handoff = TaskHandoff.fromJson(json['handoff']);
    if (task.protocol == Protocol.http &&
        (json.containsKey('completionView') ||
            json.containsKey('completion'))) {
      task.completionView = HttpCompletionProjection.fromJson(
          json['completionView'],
          taskId: task.id);
    }
    return task;
  }

  Map<String, dynamic> toJson() => {
        ..._$TaskToJson(this),
        if (protocol == Protocol.netdisk) 'protocol': 'netdisk',
        if (handoff != null) 'handoff': handoff!.toJson(),
      };
}

extension TaskTorrentInfo on Task {
  /// A magnet task runs before any peer has sent its metadata; until then it
  /// has no files or size to show, and it waits without a deadline.
  bool get fetchingTorrentInfo =>
      protocol == Protocol.bt && status == Status.running && meta.res == null;
}

@JsonSerializable()
class Progress {
  int used;
  int speed;
  int downloaded;
  String mediaPhase;
  int uploadSpeed;
  int uploaded;
  ExtractStatus extractStatus;
  int extractProgress;

  Progress({
    required this.used,
    required this.speed,
    required this.downloaded,
    this.mediaPhase = '',
    required this.uploadSpeed,
    required this.uploaded,
    this.extractStatus = ExtractStatus.none,
    this.extractProgress = 0,
  });

  factory Progress.fromJson(Map<String, dynamic> json) =>
      _$ProgressFromJson(json);

  Map<String, dynamic> toJson() => _$ProgressToJson(this);
}
