import 'package:dio/dio.dart' show CancelToken;
import 'package:flutter/material.dart';
import 'package:get/get.dart';

import '../../../../api/model/media_inspection.dart';

class WebMediaSelection {
  final MediaFormatChoice? format;
  final String? candidateId, expectedMediaId;

  const WebMediaSelection.savePage()
      : format = null,
        candidateId = null,
        expectedMediaId = null;
  const WebMediaSelection.download(MediaFormatChoice this.format,
      {this.candidateId, this.expectedMediaId});
}

class WebMediaPreview extends StatefulWidget {
  final Future<MediaInspection> Function(
      String? candidateId, CancelToken cancel) inspect;
  final bool canSavePage;

  const WebMediaPreview(
      {super.key, required this.inspect, required this.canSavePage});

  @override
  State<WebMediaPreview> createState() => _WebMediaPreviewState();
}

class _WebMediaPreviewState extends State<WebMediaPreview> {
  CancelToken? _inspection;
  MediaInspection? _media;
  List<MediaPageCandidate> _candidates = const [];
  List<MediaFormatChoice> _choices = const [];
  MediaPageCandidate? _candidate;
  MediaFormatChoice? _format;

  @override
  void initState() {
    super.initState();
    _inspect(null);
  }

  Future<void> _inspect(MediaPageCandidate? candidate) async {
    _inspection?.cancel();
    final token = CancelToken();
    setState(() {
      _inspection = token;
      _candidate = candidate;
      _media = null;
      _choices = const [];
      _format = null;
    });
    MediaInspection? media;
    try {
      media = await widget.inspect(candidate?.id, token);
    } catch (_) {
      // Extractor errors can contain the full URL or authentication details.
    }
    if (!mounted || !identical(_inspection, token)) return;
    setState(() {
      _inspection = null;
      _media = media;
      if (candidate == null) _candidates = media?.candidates ?? const [];
      _choices = media?.choices ?? const [];
      _format = MediaFormatChoice.defaultVideo(_choices);
    });
  }

  @override
  void dispose() {
    _inspection?.cancel();
    super.dispose();
  }

  String _duration(double value) {
    if (!value.isFinite || value <= 0) return '';
    final seconds = value.round();
    return '${seconds ~/ 60}:${(seconds % 60).toString().padLeft(2, '0')}';
  }

  String _formatLabel(MediaFormatChoice choice) {
    final format = choice.format;
    final quality = switch (format) {
      _ when format.audioOnly => 'webMediaAudio'.tr,
      _ when format.height > 0 => '${format.height}p',
      _ when format.width > 0 => '${format.width}px',
      _ => 'webMediaVideo'.tr,
    };
    return [
      quality,
      if (choice.container.isNotEmpty) choice.container,
      if (choice.mergesTracks) 'webMediaMergeAudio'.tr,
      if (choice.noAudio) 'webMediaSilentVideo'.tr,
      if (!choice.noAudio && !choice.hasConfirmedTracks)
        'webMediaUnknownTracks'.tr,
      if (format.language.isNotEmpty) format.language,
      if (format.hasVideo) format.vcodec.split('.').first,
      if (_candidate != null &&
          format.label.isNotEmpty &&
          !format.label.startsWith(format.id))
        format.label,
    ].join(' · ');
  }

  String _unavailableLabel(MediaPreviewIssue? issue) => switch (issue) {
        MediaPreviewIssue.noFormats => 'webMediaNoFormats'.tr,
        MediaPreviewIssue.live => 'webMediaLive'.tr,
        MediaPreviewIssue.protected => 'webMediaProtected'.tr,
        MediaPreviewIssue.multiple => 'webMediaMultiple'.tr,
        MediaPreviewIssue.tools => 'webMediaToolsUnavailable'.tr,
        MediaPreviewIssue.sourceChanged => 'webMediaSourceChanged'.tr,
        MediaPreviewIssue.access => 'webMediaSourceAccess'.tr,
        _ => 'webMediaUnavailable'.tr,
      };

  @override
  Widget build(BuildContext context) {
    final media = _media;
    final selected = _format;
    final title = media?.title ?? '';
    return AlertDialog(
      key: const ValueKey('web-media-preview'),
      title: Text('webMediaPreview'.tr),
      content: SizedBox(
        width: 380,
        child: SingleChildScrollView(
          child: Column(
            mainAxisSize: MainAxisSize.min,
            crossAxisAlignment: CrossAxisAlignment.start,
            children: [
              if (_candidates.isNotEmpty)
                DropdownButton<MediaPageCandidate>(
                  key: const ValueKey('web-media-candidate'),
                  isExpanded: true,
                  value: _candidate,
                  hint: Text('webMediaChooseVideo'.tr),
                  items: [
                    for (final (index, candidate) in _candidates.indexed)
                      DropdownMenuItem(
                        value: candidate,
                        child: Text(
                          [
                            '${index + 1}. ${candidate.title.isEmpty ? 'webMediaVideo'.tr : candidate.title}',
                            if (candidate.duration > 0)
                              _duration(candidate.duration),
                          ].join(' · '),
                          overflow: TextOverflow.ellipsis,
                        ),
                      ),
                  ],
                  onChanged: (candidate) {
                    if (candidate != null) _inspect(candidate);
                  },
                ),
              if (_inspection != null) ...[
                const LinearProgressIndicator(),
                const SizedBox(height: 10),
                Text('webMediaInspecting'.tr),
              ] else if (_choices.isNotEmpty) ...[
                if (title.isNotEmpty)
                  Text(title,
                      maxLines: 3,
                      overflow: TextOverflow.ellipsis,
                      style: Theme.of(context).textTheme.titleMedium),
                if (media != null && media.duration > 0)
                  Text(_duration(media.duration)),
                const SizedBox(height: 8),
                DropdownButton<MediaFormatChoice>(
                  key: const ValueKey('web-media-format'),
                  isExpanded: true,
                  value: selected,
                  hint: Text('webMediaChooseFormat'.tr),
                  items: [
                    for (final choice in _choices)
                      DropdownMenuItem(
                        value: choice,
                        child: Text(_formatLabel(choice),
                            overflow: TextOverflow.ellipsis),
                      ),
                  ],
                  onChanged: (choice) => setState(() => _format = choice),
                ),
              ] else if (_candidates.isEmpty || _candidate != null)
                Text(_unavailableLabel(media?.issue)),
            ],
          ),
        ),
      ),
      actions: [
        TextButton(
          onPressed: () => Navigator.pop(context),
          child: Text('cancel'.tr),
        ),
        if (widget.canSavePage)
          TextButton(
            key: const ValueKey('web-media-save-page'),
            onPressed: () =>
                Navigator.pop(context, const WebMediaSelection.savePage()),
            child: Text('webMediaSavePage'.tr),
          ),
        FilledButton(
          key: const ValueKey('web-media-download'),
          onPressed: selected == null
              ? null
              : () => Navigator.pop(
                  context,
                  WebMediaSelection.download(selected,
                      candidateId: _candidate?.id,
                      expectedMediaId: _candidate == null ? media?.id : null)),
          child: Text('webMediaDownload'.tr),
        ),
      ],
    );
  }
}
