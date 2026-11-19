class MediaInspection {
  final bool supported;
  final String id, title;
  final double duration;
  final MediaPreviewIssue issue;
  final List<MediaPageCandidate> candidates;
  final List<MediaFormat> formats;

  MediaInspection.fromJson(Map<String, dynamic> json)
      : supported = json['supported'] == true,
        id = _text(json['mediaId']).isEmpty
            ? _text(json['id'])
            : _text(json['mediaId']),
        title = _text(json['title']),
        duration = _number(json['duration']).toDouble(),
        issue = json['isLive'] == true
            ? MediaPreviewIssue.live
            : MediaPreviewIssue.fromReason(json['reason']),
        candidates = [
          for (final entry in _objects(json['candidates']))
            if (_text(entry['id']).isNotEmpty)
              MediaPageCandidate(_text(entry['id']), _text(entry['title']),
                  _number(entry['duration']).toDouble()),
        ],
        formats = [
          for (final entry in _objects(json['formats']))
            if (_text(entry['id']).isNotEmpty) MediaFormat(entry),
        ];

  List<MediaFormatChoice> get choices {
    if (!supported || candidates.isNotEmpty) return const [];
    final audio = formats.reversed.where((format) => format.audioOnly);
    final choices = <String, MediaFormatChoice>{};
    for (final format in formats) {
      if (format.vcodec == 'none' && format.acodec == 'none') continue;
      if (format.hasVideo &&
          format.acodec == 'none' &&
          !format.id.contains('+') &&
          audio.isNotEmpty) {
        final compatible = format.ext == 'mp4'
            ? audio.where((track) => track.ext == 'm4a').firstOrNull
            : null;
        final track = compatible ?? audio.first;
        final id = '${format.id}+${track.id}';
        choices[id] = MediaFormatChoice(
            id, compatible == null ? 'mkv' : 'mp4', format,
            mergesTracks: true);
      } else {
        choices[format.id] = MediaFormatChoice(
            format.id, format.container, format,
            mergesTracks: format.id.contains('+'));
      }
    }
    return choices.values.toList();
  }
}

enum MediaPreviewIssue {
  noFormats,
  live,
  protected,
  multiple,
  tools,
  sourceChanged,
  access,
  unavailable;

  static MediaPreviewIssue fromReason(Object? reason) => switch (reason) {
        'no unprotected downloadable format' => noFormats,
        'live media is unsupported' => live,
        'DRM media is unsupported' || '此资源受保护，无法下载' => protected,
        'playlist or multi-video media is unsupported' => multiple,
        '网页多视频下载需要支持来源隔离的 Python yt-dlp' ||
        '视频解析工具无法启动，请检查设置中的工具路径' ||
        '音视频合并工具无法启动，请检查设置中的工具路径' =>
          tools,
        'selected media does not match the requested video' ||
        '网页上的所选视频或格式已变化，请重新预览网页' =>
          sourceChanged,
        '资源登录态或链接已失效，请返回网页重新捕获并更新任务' => access,
        _ => unavailable,
      };
}

class MediaPageCandidate {
  final String id, title;
  final double duration;
  const MediaPageCandidate(this.id, this.title, this.duration);
}

class MediaFormat {
  final String id, label, ext, container, vcodec, acodec, language;
  final int width, height, size;

  MediaFormat(Map<String, dynamic> json)
      : id = _text(json['id']),
        label = _text(json['label']),
        ext = _text(json['ext']),
        container = _text(json['container']).isEmpty
            ? _text(json['ext'])
            : _text(json['container']),
        vcodec = _text(json['vcodec']),
        acodec = _text(json['acodec']),
        language = _text(json['language']),
        width = _number(json['width']).toInt(),
        height = _number(json['height']).toInt(),
        size = _number(json['filesize']).toInt();

  bool get hasVideo => _hasCodec(vcodec);
  bool get hasAudio => _hasCodec(acodec);
  bool get audioOnly => vcodec == 'none' && hasAudio;
}

class MediaFormatChoice {
  final String id, container;
  final MediaFormat format;
  final bool mergesTracks;

  const MediaFormatChoice(this.id, this.container, this.format,
      {required this.mergesTracks});

  String get mediaKind => format.audioOnly ? 'audio' : 'video';
  bool get silentVideo =>
      !mergesTracks && format.hasVideo && format.acodec == 'none';
  bool get hasConfirmedTracks =>
      format.audioOnly ||
      silentVideo ||
      (format.hasVideo && (format.hasAudio || mergesTracks));

  static MediaFormatChoice? defaultVideo(List<MediaFormatChoice> choices) {
    final videos = choices
        .where((choice) =>
            choice.format.hasVideo &&
            choice.hasConfirmedTracks &&
            !choice.silentVideo)
        .toList();
    videos.sort((a, b) {
      final height = b.format.height.compareTo(a.format.height);
      if (height != 0) return height;
      final width = b.format.width.compareTo(a.format.width);
      if (width != 0) return width;
      if ((a.container == 'mp4') != (b.container == 'mp4')) {
        return a.container == 'mp4' ? -1 : 1;
      }
      return b.format.size.compareTo(a.format.size);
    });
    return videos.firstOrNull;
  }
}

String _text(Object? value) => value is String ? value : '';
num _number(Object? value) => value is num ? value : 0;
bool _hasCodec(String codec) =>
    codec.isNotEmpty && codec != 'none' && codec != 'unknown';
Iterable<Map<String, dynamic>> _objects(Object? value) =>
    value is List ? value.whereType<Map<String, dynamic>>() : const [];
