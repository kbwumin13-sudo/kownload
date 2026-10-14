import 'package:json_annotation/json_annotation.dart';

part 'result.g.dart';

@JsonSerializable(genericArgumentFactories: true)
class Result<T> {
  int code;
  String? msg;
  T? data;

  Result({
    required this.code,
    this.msg,
    this.data,
  });

  @override
  String toString() => 'Result(code: $code, msg: ${msg ?? "未知错误"})';

  factory Result.fromJson(
    Map<String, dynamic> json,
    T Function(dynamic json) fromJsonT,
  ) =>
      _$ResultFromJson(json, fromJsonT);
  Map<String, dynamic> toJson() => {
        'code': code,
        'msg': msg,
        'data': data is List
            ? (data as dynamic)?.map((e) => e.toJson()).toList()
            : (data as dynamic)?.toJson(),
      };
}
