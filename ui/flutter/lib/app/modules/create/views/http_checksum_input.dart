import 'package:flutter/material.dart';

/// Transient user input. This is not a request DTO or a verification result.
String? normalizedExpectedSha256(String raw) {
  final value = raw.trim();
  if (value.isEmpty) return null;
  if (!RegExp(r'^[0-9a-fA-F]{64}$').hasMatch(value)) {
    throw const FormatException('请输入完整的 64 位十六进制 SHA-256。');
  }
  return value.toLowerCase();
}

class HttpChecksumInput extends StatelessWidget {
  final TextEditingController controller;
  final ValueChanged<String> onChanged;
  final String? helperText;

  const HttpChecksumInput({
    super.key,
    required this.controller,
    required this.onChanged,
    this.helperText,
  });

  @override
  Widget build(BuildContext context) => TextFormField(
        key: const ValueKey('http-expected-sha256'),
        controller: controller,
        autocorrect: false,
        enableSuggestions: false,
        autovalidateMode: AutovalidateMode.onUserInteraction,
        decoration: InputDecoration(
          isDense: true,
          labelText: 'SHA-256（可选）',
          helperText: helperText ?? '仅支持手动单链接 HTTP(S)；留空保持普通下载。',
          hintText: '64 位十六进制摘要',
        ),
        validator: (value) {
          try {
            normalizedExpectedSha256(value ?? '');
            return null;
          } on FormatException catch (error) {
            return error.message;
          }
        },
        onChanged: onChanged,
      );
}
