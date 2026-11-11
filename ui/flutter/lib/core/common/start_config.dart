import 'package:json_annotation/json_annotation.dart';

part 'start_config.g.dart';

@JsonSerializable()
class StartConfig {
  late String network;
  late String address;
  late String storage;
  late String storageDir;
  late int refreshInterval;
  late String apiToken;

  // A launch-only admission option. It is not part of saved DownloaderConfig.
  @JsonKey(defaultValue: 'normal')
  String startupTaskPolicy = 'normal';

  StartConfig();

  factory StartConfig.fromJson(Map<String, dynamic> json) =>
      _$StartConfigFromJson(json);

  Map<String, dynamic> toJson() => _$StartConfigToJson(this);
}
