import 'package:json_annotation/json_annotation.dart';

import 'meta.dart';

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
    if (json['protocol'] == 'netdisk') {
      return _$TaskFromJson({...json, 'protocol': null})
        ..protocol = Protocol.netdisk;
    }
    return _$TaskFromJson(json);
  }

  Map<String, dynamic> toJson() => {
        ..._$TaskToJson(this),
        if (protocol == Protocol.netdisk) 'protocol': 'netdisk',
      };
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
