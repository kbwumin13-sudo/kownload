import 'dart:convert';
import 'dart:io';

import 'package:contentsize_tabbarview/contentsize_tabbarview.dart';
import 'package:desktop_drop/desktop_drop.dart';
import 'package:file_picker/file_picker.dart';
import 'package:flutter/material.dart';
import 'package:flutter/cupertino.dart';
import 'package:flutter/services.dart';
import 'package:get/get.dart';
import 'package:path/path.dart' as path;
import 'package:rounded_loading_button_plus/rounded_loading_button.dart';

import '../../../../api/api.dart';
import '../../../../api/model/create_task.dart';
import '../../../../api/model/create_task_batch.dart';
import '../../../../api/model/downloader_config.dart';
import '../../../../api/model/options.dart';
import '../../../../api/model/request.dart';
import '../../../../api/model/resolve_result.dart';
import '../../../../api/model/resolve_task.dart';
import '../../../../api/model/task.dart';
import '../../../../api/netdisk.dart';
import '../../../../database/database.dart';
import '../../../../util/input_formatter.dart';
import '../../../../util/message.dart';
import '../../../../util/log_util.dart';
import '../../../../util/util.dart';
import '../../../../util/netdisk_browser.dart';
import '../../../../macos/mac_widgets.dart';
import '../../../../macos/mac_glass.dart';
import '../../../../macos/mac_panel.dart';
import '../../task/controllers/task_controller.dart';
import '../../../routes/app_pages.dart';
import '../../../views/compact_checkbox.dart';
import '../../../views/directory_selector.dart';
import '../../../views/file_tree_view.dart';
import '../../app/controllers/app_controller.dart';
import '../../history/views/history_view.dart';
import '../controllers/create_controller.dart';
import 'netdisk_view.dart';

class CreateView extends StatefulWidget {
  final NetdiskApi? netdiskApi;
  final NetdiskBrowserTool? netdiskBrowserTool;
  final Future<String> Function(CreateTask)? taskCreator;
  const CreateView(
      {super.key, this.netdiskApi, this.netdiskBrowserTool, this.taskCreator});
  Protocol? parseProtocol(String url) => _parseProtocol(url);
  @override
  State<CreateView> createState() => _CreateViewState();
}

Protocol? _parseProtocol(String url) {
  if (netdiskProviderForUrl(url.trim()) != null) return Protocol.netdisk;
  final value = url.toUpperCase();
  if (value.startsWith('HTTP:') || value.startsWith('HTTPS:'))
    return Protocol.http;
  if (value.startsWith('MAGNET:') || value.endsWith('.TORRENT'))
    return Protocol.bt;
  if (value.startsWith('ED2K:')) return Protocol.ed2k;
  return null;
}

class _CreateViewState extends State<CreateView> {
  Future<String> _createTask(CreateTask task) =>
      (widget.taskCreator ?? createTask)(task);
  CreateController get controller => Get.find<CreateController>();
  NetdiskApi? get netdiskApi => widget.netdiskApi;
  NetdiskBrowserTool? get netdiskBrowserTool => widget.netdiskBrowserTool;
  final _inputFocus = FocusNode();
  final _panelError = RxnString();
  final _queuedInputs = <CreateTask>[];
  CreateTask? _panelRequest;
  Request? _originalRequest;
  Uri? _credentialOrigin;
  bool _clipboardChecked = false;
  Worker? _pendingWorker;
  @override
  void initState() {
    super.initState();
    if (Util.isMacos()) {
      MacPanel.focusRequests.addListener(_focusInput);
      MacPanel.incoming.addListener(_receiveInput);
      _panelRequest = MacPanel.incoming.value;
      _pendingWorker = ever(Get.find<AppController>().pendingUpdateTask, (_) {
        if (mounted) setState(() => _initialNetdiskHandled = false);
      });
    }
  }

  void _focusInput() {
    WidgetsBinding.instance.addPostFrameCallback((_) {
      if (mounted &&
          MacPanel.presentation.value == 'panel' &&
          !_netdiskOpen.value) {
        _inputFocus.requestFocus();
      }
    });
  }

  void _receiveInput() {
    final task = MacPanel.incoming.value;
    if (task == null) return;
    if (controller.isConfirming.value || _netdiskOpen.value) {
      _queuedInputs.add(task);
      return;
    }
    _resetTaskParameters();
    setState(() {
      _panelRequest = task;
      controller.pendingCreateHandled = false;
      _initialNetdiskHandled = false;
      _panelError.value = null;
    });
  }

  @override
  void dispose() {
    if (Util.isMacos()) MacPanel.cancelAutoHide();
    _pendingWorker?.dispose();
    MacPanel.focusRequests.removeListener(_focusInput);
    MacPanel.incoming.removeListener(_receiveInput);
    _inputFocus.dispose();
    for (final field in [
      _urlController,
      _renameController,
      _connectionsController,
      _pathController,
      _proxyIpController,
      _proxyPortController,
      _proxyUsrController,
      _proxyPwdController,
      _btTrackerController,
      _archivePasswordController,
      ..._httpHeaderControllers.expand((e) => [e.name, e.value])
    ]) {
      field.dispose();
    }
    super.dispose();
  }

  final _netdiskRequest = Rxn<Request>();
  final _netdiskOpen = false.obs;
  bool _initialNetdiskHandled = false;
  final _confirmFormKey = GlobalKey<FormState>();

  final _urlController = TextEditingController();
  final _renameController = TextEditingController();
  final _connectionsController = TextEditingController();
  final _pathController = TextEditingController();
  final _confirmController = RoundedLoadingButtonController();
  final _proxyIpController = TextEditingController();
  final _proxyPortController = TextEditingController();
  final _proxyUsrController = TextEditingController();
  final _proxyPwdController = TextEditingController();
  final _httpHeaderControllers = [
    (
      name: TextEditingController(text: "User-Agent"),
      value: TextEditingController()
    ),
    (
      name: TextEditingController(text: "Cookie"),
      value: TextEditingController()
    ),
    (
      name: TextEditingController(text: "Referer"),
      value: TextEditingController()
    ),
  ];
  final _btTrackerController = TextEditingController();
  final _archivePasswordController = TextEditingController();

  final _availableSchemes = ["http:", "https:", "magnet:", "ed2k:"];

  final _skipVerifyCertController = false.obs;
  final _autoTorrentController = Rxn<bool>();
  final _deleteTorrentAfterDownloadController = Rxn<bool>();
  final _autoExtractController = Rxn<bool>();
  final _deleteAfterExtractController = Rxn<bool>();

  Widget _macLabeledField(String label, Widget field) {
    if (!Util.isMacos()) return field;
    return Column(crossAxisAlignment: CrossAxisAlignment.start, children: [
      Text(label,
          style: const TextStyle(fontSize: 12, fontWeight: FontWeight.w500)),
      const SizedBox(height: 5),
      field is TextField ? SizedBox(height: 36, child: field) : field,
    ]);
  }

  double _macUrlHeight(BuildContext context, String text, double width) {
    final painter = TextPainter(
      text: TextSpan(
          text: text.isEmpty ? ' ' : text,
          style: Theme.of(context).textTheme.bodyMedium),
      textDirection: Directionality.of(context),
      maxLines: 5,
    )..layout(maxWidth: width > 149 ? width - 148 : 1);
    final lines = painter.computeLineMetrics().length.clamp(1, 5);
    painter.dispose();
    return 36.0 + 20.0 * (lines - 1);
  }

  Future<void> _cancelCreate() async {
    if (Util.isMacos()) {
      if (controller.showAdvanced.value) {
        controller.showAdvanced.value = false;
      } else {
        await MacPanel.hide();
      }
      return;
    }
    if (controller.isConfirming.value) return;
    if (Util.isMacos() && Get.rootDelegate.history.length > 1) {
      await Get.rootDelegate.popRoute(popMode: PopMode.History);
    } else {
      await _completeCreate();
    }
  }

  @override
  Widget build(BuildContext context) {
    final appController = Get.find<AppController>();

    if (_connectionsController.text.isEmpty) {
      _connectionsController.text = appController
          .downloaderConfig.value.protocolConfig.http.connections
          .toString();
    }
    if (_pathController.text.isEmpty) {
      // Render placeholders when initializing the path
      final downloadDir = appController.downloaderConfig.value.downloadDir;
      _pathController.text = renderPathPlaceholders(downloadDir);
    }

    // Handle pending create task from deep link
    final args = Get.rootDelegate.arguments();
    final CreateTask? routerParams = Util.isMacos()
        ? _panelRequest ?? (args is CreateTask ? args : null)
        : (args is CreateTask ? args : null);
    if (((routerParams?.req?.url.isNotEmpty ?? false) ||
            routerParams?.req?.protocol == 'netdisk') &&
        !controller.pendingCreateHandled) {
      controller.pendingCreateHandled = true;
      // get url from route arguments
      _originalRequest = routerParams!.req;
      _credentialOrigin = Uri.tryParse(routerParams.req!.url);
      _skipVerifyCertController.value = routerParams.req!.skipVerifyCert;
      controller.proxyConfig.value = routerParams.req!.proxy;
      final requestProxy = routerParams.req!.proxy;
      if (requestProxy?.mode == RequestProxyMode.custom) {
        final address =
            Uri.tryParse('${requestProxy!.scheme}://${requestProxy.host}');
        _proxyIpController.text = address?.host ?? '';
        _proxyPortController.text =
            address?.hasPort == true ? address!.port.toString() : '';
        _proxyUsrController.text = requestProxy.usr;
        _proxyPwdController.text = requestProxy.pwd;
      }
      final url = routerParams.req!.url;
      _urlController.text = url;
      _urlController.selection = TextSelection.fromPosition(
          TextPosition(offset: _urlController.text.length));
      final protocol = parseProtocol(url);
      if (routerParams.req!.protocol == 'netdisk') {
        _netdiskRequest.value = routerParams.req;
        if (routerParams.opts?.path.isNotEmpty ?? false) {
          _pathController.text = routerParams.opts!.path;
        }
      } else if (protocol != null) {
        final extraHandlers = {
          Protocol.http: () {
            final reqExtra = ReqExtraHttp.fromJson(
                jsonDecode(jsonEncode(routerParams.req!.extra)));
            _httpHeaderControllers.clear();
            reqExtra.header.forEach((key, value) {
              _httpHeaderControllers.add(
                (
                  name: TextEditingController(text: key),
                  value: TextEditingController(text: value),
                ),
              );
            });
            _skipVerifyCertController.value = routerParams.req!.skipVerifyCert;
          },
          Protocol.bt: () {
            final reqExtra = ReqExtraBt.fromJson(
                jsonDecode(jsonEncode(routerParams.req!.extra)));
            _btTrackerController.text = reqExtra.trackers.join("\n");
          },
          Protocol.ed2k: null,
        };
        if (routerParams.req?.extra != null) {
          extraHandlers[protocol]?.call();
        }

        // handle options
        if (routerParams.opts != null) {
          _renameController.text = routerParams.opts!.name;
          _pathController.text = routerParams.opts!.path;

          final optionsHandlers = {
            Protocol.http: () {
              final opt = routerParams.opts!;
              _renameController.text = opt.name;
              _pathController.text = opt.path;
              if (opt.extra != null) {
                final optsExtraHttp =
                    OptsExtraHttp.fromJson(jsonDecode(jsonEncode(opt.extra)));
                _connectionsController.text =
                    optsExtraHttp.connections.toString();
              }
            },
            Protocol.bt: null,
            Protocol.ed2k: null,
          };
          if (routerParams.opts?.extra != null) {
            optionsHandlers[protocol]?.call();
          }
        }
      }
    } else if (_urlController.text.isEmpty && !_clipboardChecked) {
      _clipboardChecked = true;
      // read clipboard
      Clipboard.getData('text/plain').then((value) {
        final clipboardText = value?.text ?? '';
        if (clipboardText.isNotEmpty) {
          if (parseNetdiskInputs(clipboardText).isNotEmpty ||
              _availableSchemes
                  .where((e) =>
                      clipboardText.startsWith(e) ||
                      clipboardText.startsWith(e.toUpperCase()))
                  .isNotEmpty) {
            _urlController.text = clipboardText;
            _urlController.selection = TextSelection.fromPosition(
                TextPosition(offset: _urlController.text.length));
            return;
          }

          recognizeMagnetUri(clipboardText);
        }
      });
    }

    if ((!Util.isMacos() || MacPanel.presentation.value == 'panel') &&
        !_initialNetdiskHandled &&
        (appController.pendingUpdateTask.value != null ||
            _netdiskRequest.value != null)) {
      _initialNetdiskHandled = true;
      WidgetsBinding.instance.addPostFrameCallback((_) async {
        if (!context.mounted) return;
        try {
          if (await _showPendingNetdiskTask()) return;
          final request = _netdiskRequest.value;
          if (request != null &&
              await _showNetdisk(NetdiskInput.fromRequest(request),
                  options: routerParams?.opts)) {
            _netdiskRequest.value = null;
            _completeCreate();
          }
        } catch (e) {
          _panelError.value = e.toString();
        } finally {
          _drainQueuedInputs();
        }
      });
    }

    final torrentButton = IconButton(
      constraints: Util.isMacos()
          ? const BoxConstraints.tightFor(width: 32, height: 32)
          : null,
      padding: Util.isMacos() ? EdgeInsets.zero : null,
      icon: Icon(Util.isMacos() ? CupertinoIcons.folder : Icons.folder_open),
      onPressed: () async {
        var pr = await MacPanel.transient(() => FilePicker.platform
            .pickFiles(type: FileType.custom, allowedExtensions: ["torrent"]));
        if (pr != null) {
          if (!Util.isWeb()) {
            _urlController.text = pr.files[0].path ?? "";
            return;
          }
          _urlController.text = pr.files[0].name;
          controller.setFileDataUri(pr.files[0].bytes!);
        }
      },
    );
    final historyButton = IconButton(
      constraints: Util.isMacos()
          ? const BoxConstraints.tightFor(width: 32, height: 32)
          : null,
      padding: Util.isMacos() ? EdgeInsets.zero : null,
      icon: Icon(Util.isMacos() ? CupertinoIcons.clock : Icons.history_rounded),
      onPressed: () async {
        List<String> resultOfHistories =
            Database.instance.getCreateHistory() ?? [];
        // show dialog box to list history
        if (context.mounted) {
          showGeneralDialog(
            barrierColor: Colors.black.withOpacity(0.5),
            transitionBuilder: (context, a1, a2, widget) {
              return Transform.scale(
                scale: a1.value,
                child: Opacity(
                  opacity: a1.value,
                  child: HistoryView(
                    isHistoryListEmpty: resultOfHistories.isEmpty,
                    historyList: ListView.builder(
                      itemCount: resultOfHistories.length,
                      itemBuilder: (context, index) {
                        return GestureDetector(
                          onTap: () {
                            _urlController.text = resultOfHistories[index];
                            Navigator.pop(context);
                          },
                          child: MouseRegion(
                            cursor: SystemMouseCursors.click,
                            child: Container(
                              padding: const EdgeInsets.symmetric(
                                horizontal: 8.0,
                                vertical: 8.0,
                              ),
                              margin: const EdgeInsets.symmetric(
                                horizontal: 10.0,
                                vertical: 8.0,
                              ),
                              decoration: BoxDecoration(
                                color: Theme.of(context).colorScheme.surface,
                                borderRadius: BorderRadius.circular(10.0),
                              ),
                              child: Text(
                                resultOfHistories[index],
                              ),
                            ),
                          ),
                        );
                      },
                    ),
                  ),
                ),
              );
            },
            transitionDuration: const Duration(milliseconds: 250),
            barrierDismissible: true,
            barrierLabel: '',
            context: context,
            pageBuilder: (context, animation1, animation2) {
              return const Text('PAGE BUILDER');
            },
          );
        }
      },
    );
    final clearUrlButton = IconButton(
      constraints: Util.isMacos()
          ? const BoxConstraints.tightFor(width: 32, height: 32)
          : null,
      padding: Util.isMacos() ? EdgeInsets.zero : null,
      onPressed: () {
        _urlController.clear();
        controller.clearFileDataUri();
      },
      icon:
          Icon(Util.isMacos() ? CupertinoIcons.xmark_circle_fill : Icons.clear),
    );

    final content = Scaffold(
      appBar: AppBar(
        leading: IconButton(
            icon:
                Icon(Util.isMacos() ? CupertinoIcons.xmark : Icons.arrow_back),
            tooltip: 'cancel'.tr,
            onPressed: _cancelCreate),
        // actions: [],
        title: Text('create'.tr),
        bottom: Util.isMacos()
            ? PreferredSize(
                preferredSize: const Size.fromHeight(1),
                child: Divider(height: 1, color: MacColors.of(context).border))
            : null,
      ),
      bottomNavigationBar: Util.isMacos()
          ? DecoratedBox(
              decoration: BoxDecoration(
                  border: Border(
                      top: BorderSide(color: MacColors.of(context).border))),
              child: Padding(
                padding: const EdgeInsets.fromLTRB(24, 8, 24, 12),
                child: Obx(() => Wrap(
                        alignment: WrapAlignment.end,
                        spacing: 10,
                        runSpacing: 8,
                        crossAxisAlignment: WrapCrossAlignment.center,
                        children: [
                          SizedBox(
                              width: double.infinity,
                              height: 32,
                              child: GestureDetector(
                                  behavior: HitTestBehavior.opaque,
                                  onTap: () =>
                                      controller.directDownload.toggle(),
                                  child: Row(children: [
                                    SizedBox(
                                        key: const ValueKey(
                                            'mac-create-direct-checkbox'),
                                        width: 18,
                                        height: 18,
                                        child: CupertinoCheckbox(
                                            value:
                                                controller.directDownload.value,
                                            activeColor:
                                                MacColors.of(context).accent,
                                            onChanged: (value) => controller
                                                .directDownload
                                                .value = value ?? false)),
                                    const SizedBox(width: 8),
                                    Text('directDownload'.tr),
                                  ]))),
                          TextButton(
                              onPressed: () => controller.showAdvanced.toggle(),
                              child: Row(
                                  mainAxisSize: MainAxisSize.min,
                                  children: [
                                    Icon(
                                        controller.showAdvanced.value
                                            ? CupertinoIcons.chevron_up
                                            : CupertinoIcons.chevron_down,
                                        size: 12),
                                    const SizedBox(width: 6),
                                    Text('advancedOptions'.tr),
                                  ])),
                          MacButton(
                              onPressed: controller.isConfirming.value
                                  ? null
                                  : _cancelCreate,
                              child: Text('cancel'.tr)),
                          MacButton(
                              primary: true,
                              onPressed: controller.isConfirming.value
                                  ? null
                                  : _doConfirm,
                              child: controller.isConfirming.value
                                  ? SizedBox(
                                      width: 14,
                                      height: 14,
                                      child: CircularProgressIndicator(
                                          strokeWidth: 2,
                                          color: MacColors.of(context).dark
                                              ? const Color(0xff102b1c)
                                              : Colors.white))
                                  : Text('confirm'.tr)),
                        ])),
              ))
          : null,
      body: DropTarget(
        onDragDone: (details) async {
          if (!Util.isWeb()) {
            _urlController.text = details.files[0].path;
            return;
          }
          _urlController.text = details.files[0].name;
          final bytes = await details.files[0].readAsBytes();
          controller.setFileDataUri(bytes);
        },
        child: GestureDetector(
          behavior: HitTestBehavior.opaque,
          onTap: () {
            FocusScope.of(context).requestFocus(FocusNode());
          },
          child: SingleChildScrollView(
            child: Padding(
              padding:
                  const EdgeInsets.symmetric(vertical: 16.0, horizontal: 24.0),
              child: Form(
                key: _confirmFormKey,
                autovalidateMode: AutovalidateMode.onUserInteraction,
                child: Column(
                  children: [
                    _macLabeledField(
                        'downloadLink'.tr,
                        Row(children: [
                          Expanded(
                            child: ValueListenableBuilder<TextEditingValue>(
                              valueListenable: _urlController,
                              builder: (context, value, _) => LayoutBuilder(
                                builder: (context, constraints) =>
                                    ConstrainedBox(
                                  constraints: BoxConstraints(
                                      minHeight: Util.isMacos()
                                          ? _macUrlHeight(context, value.text,
                                              constraints.maxWidth)
                                          : 0,
                                      maxHeight: Util.isMacos()
                                          ? 120
                                          : double.infinity),
                                  child: TextFormField(
                                    autofocus: !Util.isMobile(),
                                    controller: _urlController,
                                    minLines: 1,
                                    maxLines: Util.isMacos() ? null : 5,
                                    decoration: InputDecoration(
                                      isDense: Util.isMacos() ? true : null,
                                      hintText: _hitText(),
                                      hintStyle: const TextStyle(fontSize: 12),
                                      labelText: Util.isMacos()
                                          ? null
                                          : 'downloadLink'.tr,
                                      contentPadding: Util.isMacos()
                                          ? const EdgeInsets.symmetric(
                                              horizontal: 10, vertical: 8)
                                          : null,
                                      prefixIconConstraints: Util.isMacos()
                                          ? const BoxConstraints.tightFor(
                                              width: 32, height: 32)
                                          : null,
                                      suffixIconConstraints: Util.isMacos()
                                          ? const BoxConstraints.tightFor(
                                              height: 32)
                                          : null,
                                      icon: Util.isMacos()
                                          ? null
                                          : const Icon(Icons.link),
                                      prefixIcon: Util.isMacos()
                                          ? const Icon(CupertinoIcons.link,
                                              size: 18)
                                          : null,
                                      suffixIcon: Util.isMacos()
                                          ? Row(
                                              mainAxisSize: MainAxisSize.min,
                                              children: [
                                                  clearUrlButton,
                                                  torrentButton,
                                                  historyButton
                                                ])
                                          : clearUrlButton,
                                    ),
                                    validator: (v) {
                                      return v!.trim().isNotEmpty
                                          ? null
                                          : 'downloadLinkValid'.tr;
                                    },
                                    onChanged: (v) async {
                                      _clearCapturedHeadersFor(v);
                                      _netdiskRequest.value = null;
                                      controller.clearFileDataUri();
                                      if (controller.oldUrl.value.isEmpty) {
                                        recognizeMagnetUri(v);
                                      }
                                      controller.oldUrl.value = v;
                                    },
                                  ),
                                ),
                              ),
                            ),
                          ),
                          if (!Util.isMacos()) torrentButton,
                          if (!Util.isMacos()) historyButton,
                        ])),
                    Padding(
                      padding: Util.isMacos()
                          ? const EdgeInsets.only(top: 12)
                          : const EdgeInsets.only(left: 40),
                      child: Column(children: [
                        _macLabeledField(
                            'rename'.tr,
                            TextField(
                              controller: _renameController,
                              textAlignVertical: TextAlignVertical.center,
                              decoration: InputDecoration(
                                  isDense: Util.isMacos() ? true : null,
                                  labelText:
                                      Util.isMacos() ? null : 'rename'.tr,
                                  constraints: Util.isMacos()
                                      ? const BoxConstraints.tightFor(
                                          height: 36)
                                      : null,
                                  contentPadding: Util.isMacos()
                                      ? const EdgeInsets.symmetric(
                                          horizontal: 10, vertical: 8)
                                      : null),
                            )),
                        if (Util.isMacos()) const SizedBox(height: 12),
                        _macLabeledField(
                            'connections'.tr,
                            TextField(
                              controller: _connectionsController,
                              textAlignVertical: TextAlignVertical.center,
                              decoration: InputDecoration(
                                isDense: Util.isMacos() ? true : null,
                                labelText:
                                    Util.isMacos() ? null : 'connections'.tr,
                                constraints: Util.isMacos()
                                    ? const BoxConstraints.tightFor(height: 36)
                                    : null,
                                contentPadding: Util.isMacos()
                                    ? const EdgeInsets.symmetric(
                                        horizontal: 10, vertical: 8)
                                    : null,
                              ),
                              keyboardType: TextInputType.number,
                              inputFormatters: [
                                FilteringTextInputFormatter.digitsOnly,
                                NumericalRangeFormatter(min: 1, max: 256),
                              ],
                            )),
                        if (Util.isMacos()) const SizedBox(height: 12),
                        _macLabeledField(
                            'downloadDir'.tr,
                            DirectorySelector(
                              controller: _pathController,
                              inlineActions: Util.isMacos(),
                            )),
                        // Category selector
                        _buildCategorySelector(appController),
                        Obx(
                          () => Visibility(
                            visible: controller.showAdvanced.value,
                            child: Column(
                              mainAxisSize: MainAxisSize.min,
                              crossAxisAlignment: CrossAxisAlignment.start,
                              children: [
                                Column(
                                  mainAxisSize: MainAxisSize.min,
                                  crossAxisAlignment: CrossAxisAlignment.start,
                                  children: [
                                    Transform.translate(
                                      offset: const Offset(-40, 0),
                                      child: Row(
                                        children: [
                                          const Icon(
                                            Icons.wifi_2_bar,
                                            color: Colors.grey,
                                          ),
                                          const SizedBox(
                                            width: 15,
                                          ),
                                          SizedBox(
                                              width: 150,
                                              child: DropdownButton<
                                                  RequestProxyMode>(
                                                hint: Text('proxy'.tr),
                                                isExpanded: true,
                                                value: controller
                                                    .proxyConfig.value?.mode,
                                                onChanged: (value) async {
                                                  if (value != null) {
                                                    controller.proxyConfig
                                                        .value = RequestProxy()
                                                      ..mode = value;
                                                  }
                                                },
                                                items: [
                                                  DropdownMenuItem<
                                                      RequestProxyMode>(
                                                    value:
                                                        RequestProxyMode.follow,
                                                    child: Text(
                                                        'followSettings'.tr),
                                                  ),
                                                  DropdownMenuItem<
                                                      RequestProxyMode>(
                                                    value:
                                                        RequestProxyMode.none,
                                                    child: Text('noProxy'.tr),
                                                  ),
                                                  DropdownMenuItem<
                                                      RequestProxyMode>(
                                                    value:
                                                        RequestProxyMode.custom,
                                                    child:
                                                        Text('customProxy'.tr),
                                                  ),
                                                ],
                                              ))
                                        ],
                                      ),
                                    ),
                                    ...(controller.proxyConfig.value?.mode ==
                                            RequestProxyMode.custom
                                        ? [
                                            SizedBox(
                                              width: 150,
                                              child: DropdownButtonFormField<
                                                  String>(
                                                value: controller
                                                    .proxyConfig.value?.scheme,
                                                onChanged: (value) async {
                                                  if (value != null) {}
                                                },
                                                items: const [
                                                  DropdownMenuItem<String>(
                                                    value: 'http',
                                                    child: Text('HTTP'),
                                                  ),
                                                  DropdownMenuItem<String>(
                                                    value: 'https',
                                                    child: Text('HTTPS'),
                                                  ),
                                                  DropdownMenuItem<String>(
                                                    value: 'socks5',
                                                    child: Text('SOCKS5'),
                                                  ),
                                                ],
                                              ),
                                            ),
                                            Row(children: [
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyIpController,
                                                  decoration: InputDecoration(
                                                    labelText: 'server'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                ),
                                              ),
                                              const Padding(
                                                  padding: EdgeInsets.only(
                                                      left: 10)),
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyPortController,
                                                  decoration: InputDecoration(
                                                    labelText: 'port'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                  keyboardType:
                                                      TextInputType.number,
                                                  inputFormatters: [
                                                    FilteringTextInputFormatter
                                                        .digitsOnly,
                                                    NumericalRangeFormatter(
                                                        min: 0, max: 65535),
                                                  ],
                                                ),
                                              ),
                                            ]),
                                            Row(children: [
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyUsrController,
                                                  decoration: InputDecoration(
                                                    labelText: 'username'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                ),
                                              ),
                                              const Padding(
                                                  padding: EdgeInsets.only(
                                                      left: 10)),
                                              Flexible(
                                                child: TextFormField(
                                                  controller:
                                                      _proxyPwdController,
                                                  decoration: InputDecoration(
                                                    labelText: 'password'.tr,
                                                    contentPadding:
                                                        const EdgeInsets.all(
                                                            0.0),
                                                  ),
                                                ),
                                              ),
                                            ])
                                          ]
                                        : const []),
                                  ],
                                ),
                                const Divider(),
                                TabBar(
                                  controller: controller.advancedTabController,
                                  tabs: const [
                                    Tab(
                                      text: 'HTTP',
                                    ),
                                    Tab(
                                      text: 'BitTorrent',
                                    ),
                                  ],
                                ),
                                DefaultTabController(
                                  length: 2,
                                  child: ContentSizeTabBarView(
                                    controller:
                                        controller.advancedTabController,
                                    children: [
                                      Column(
                                        children: [
                                          ..._httpHeaderControllers.map((e) {
                                            return Row(
                                              children: [
                                                Flexible(
                                                  child: TextFormField(
                                                    controller: e.name,
                                                    decoration: InputDecoration(
                                                      hintText:
                                                          'httpHeaderName'.tr,
                                                    ),
                                                  ),
                                                ),
                                                const Padding(
                                                    padding: EdgeInsets.only(
                                                        left: 10)),
                                                Flexible(
                                                  child: TextFormField(
                                                    controller: e.value,
                                                    decoration: InputDecoration(
                                                      hintText:
                                                          'httpHeaderValue'.tr,
                                                    ),
                                                  ),
                                                ),
                                                const Padding(
                                                    padding: EdgeInsets.only(
                                                        left: 10)),
                                                IconButton(
                                                  icon: const Icon(Icons.add),
                                                  onPressed: () {
                                                    _httpHeaderControllers.add(
                                                      (
                                                        name:
                                                            TextEditingController(),
                                                        value:
                                                            TextEditingController(),
                                                      ),
                                                    );
                                                    controller.showAdvanced
                                                        .update((val) => val);
                                                  },
                                                ),
                                                IconButton(
                                                  icon:
                                                      const Icon(Icons.remove),
                                                  onPressed: () {
                                                    if (_httpHeaderControllers
                                                            .length <=
                                                        1) {
                                                      return;
                                                    }
                                                    _httpHeaderControllers
                                                        .remove(e);
                                                    controller.showAdvanced
                                                        .update((val) => val);
                                                  },
                                                ),
                                              ],
                                            );
                                          }),
                                          Padding(
                                            padding:
                                                const EdgeInsets.only(top: 10),
                                            child: CompactCheckbox(
                                              label: 'skipVerifyCert'.tr,
                                              value: _skipVerifyCertController
                                                  .value,
                                              onChanged: (bool? value) {
                                                _skipVerifyCertController
                                                    .value = value ?? false;
                                              },
                                              textStyle: const TextStyle(
                                                color: Colors.grey,
                                              ),
                                            ),
                                          ),
                                          // AutoTorrent options
                                          Padding(
                                            padding:
                                                const EdgeInsets.only(top: 10),
                                            child: CompactCheckbox(
                                              label: 'autoTorrentEnable'.tr,
                                              value: _autoTorrentController
                                                      .value ??
                                                  false,
                                              onChanged: (bool? value) {
                                                _autoTorrentController.value =
                                                    value ?? false;
                                              },
                                              textStyle: const TextStyle(
                                                color: Colors.grey,
                                              ),
                                            ),
                                          ),
                                          Obx(
                                            () => Visibility(
                                              visible: _autoTorrentController
                                                      .value ??
                                                  false,
                                              child: Padding(
                                                padding: const EdgeInsets.only(
                                                    top: 10, left: 20),
                                                child: CompactCheckbox(
                                                  label:
                                                      'autoTorrentDeleteAfterDownload'
                                                          .tr,
                                                  value:
                                                      _deleteTorrentAfterDownloadController
                                                              .value ??
                                                          false,
                                                  onChanged: (bool? value) {
                                                    _deleteTorrentAfterDownloadController
                                                        .value = value ?? false;
                                                  },
                                                  textStyle: const TextStyle(
                                                    color: Colors.grey,
                                                  ),
                                                ),
                                              ),
                                            ),
                                          ),
                                          // AutoExtract options
                                          Padding(
                                            padding:
                                                const EdgeInsets.only(top: 10),
                                            child: CompactCheckbox(
                                              label: 'autoExtract'.tr,
                                              value: _autoExtractController
                                                      .value ??
                                                  false,
                                              onChanged: (bool? value) {
                                                _autoExtractController.value =
                                                    value ?? false;
                                              },
                                              textStyle: const TextStyle(
                                                color: Colors.grey,
                                              ),
                                            ),
                                          ),
                                          Obx(
                                            () => Visibility(
                                              visible: _autoExtractController
                                                      .value ??
                                                  false,
                                              child: Column(
                                                children: [
                                                  Padding(
                                                    padding:
                                                        const EdgeInsets.only(
                                                            top: 10, left: 20),
                                                    child: TextFormField(
                                                      controller:
                                                          _archivePasswordController,
                                                      obscureText: true,
                                                      decoration:
                                                          InputDecoration(
                                                        labelText:
                                                            'archivePassword'
                                                                .tr,
                                                        hintText:
                                                            'archivePasswordHint'
                                                                .tr,
                                                      ),
                                                    ),
                                                  ),
                                                  Padding(
                                                    padding:
                                                        const EdgeInsets.only(
                                                            top: 10, left: 20),
                                                    child: CompactCheckbox(
                                                      label:
                                                          'deleteAfterExtract'
                                                              .tr,
                                                      value:
                                                          _deleteAfterExtractController
                                                                  .value ??
                                                              false,
                                                      onChanged: (bool? value) {
                                                        _deleteAfterExtractController
                                                                .value =
                                                            value ?? false;
                                                      },
                                                      textStyle:
                                                          const TextStyle(
                                                        color: Colors.grey,
                                                      ),
                                                    ),
                                                  ),
                                                ],
                                              ),
                                            ),
                                          ),
                                        ],
                                      ),
                                      Column(
                                        children: [
                                          TextFormField(
                                              controller: _btTrackerController,
                                              maxLines: 5,
                                              decoration: InputDecoration(
                                                labelText: 'Trackers',
                                                hintText: 'addTrackerHit'.tr,
                                              )),
                                        ],
                                      ),
                                    ],
                                  ),
                                )
                              ],
                            ).paddingOnly(top: 16),
                          ),
                        ),
                      ]),
                    ),
                    if (!Util.isMacos())
                      Center(
                        child: Padding(
                          padding: const EdgeInsets.only(top: 15),
                          child: Column(
                            children: [
                              Row(
                                mainAxisSize: MainAxisSize.min,
                                children: [
                                  CompactCheckbox(
                                      label: 'directDownload'.tr,
                                      value: controller.directDownload.value,
                                      onChanged: (bool? value) {
                                        controller.directDownload.value =
                                            value ?? false;
                                      }),
                                  TextButton(
                                    onPressed: () {
                                      controller.showAdvanced.value =
                                          !controller.showAdvanced.value;
                                    },
                                    child: Row(children: [
                                      Obx(() => Checkbox(
                                            value:
                                                controller.showAdvanced.value,
                                            onChanged: (bool? value) {
                                              controller.showAdvanced.value =
                                                  value ?? false;
                                            },
                                          )),
                                      Text('advancedOptions'.tr),
                                    ]),
                                  ),
                                ],
                              ),
                              SizedBox(
                                width: 150,
                                child: RoundedLoadingButton(
                                  color: Get.theme.colorScheme.secondary,
                                  onPressed: _doConfirm,
                                  controller: _confirmController,
                                  child: Text('confirm'.tr),
                                ),
                              ),
                            ],
                          ),
                        ),
                      ),
                  ],
                ),
              ),
            ),
          ),
        ),
      ),
    );
    if (!Util.isMacos()) return content;
    return _buildMacPanel(context, content, torrentButton);
  }

  Request _requestForUrl(String url) {
    _clearCapturedHeadersFor(url);
    final original = _originalRequest;
    if (original != null && original.url == url) {
      final request = Request.fromJson(original.toJson());
      request.proxy = parseProxy();
      request.skipVerifyCert = _skipVerifyCertController.value;
      if (original.protocol == null || original.protocol == 'http') {
        final edited = parseReqExtra(url);
        final preserved = jsonDecode(jsonEncode(original.extra));
        if (preserved is Map) {
          request.extra = {
            ...preserved,
            'header':
                edited is ReqExtraHttp ? edited.header : <String, String>{}
          };
        } else {
          request.extra = edited;
        }
      } else if (original.protocol == 'bt') {
        request.extra = parseReqExtra(url);
      }
      return request;
    }
    return Request(
        url: url,
        extra: parseReqExtra(url),
        proxy: parseProxy(),
        skipVerifyCert: _skipVerifyCertController.value);
  }

  Future<void> _completeCreate() async {
    if (!mounted) return;
    if (!Util.isMacos()) {
      await Get.rootDelegate.offNamed(Routes.TASK);
      return;
    }
    _urlController.clear();
    _originalRequest = null;
    _credentialOrigin = null;
    _panelRequest = null;
    _netdiskRequest.value = null;
    controller.clearFileDataUri();
    controller.showAdvanced.value = false;
    _resetTaskParameters();
    MacPanel.submitted();
    if (Get.isRegistered<MacTaskListController>()) {
      Get.find<MacTaskListController>().getTasksState();
    }
  }

  void _resetTaskParameters() {
    _netdiskRequest.value = null;
    _credentialOrigin = null;
    for (final row in _httpHeaderControllers) {
      row.name.dispose();
      row.value.dispose();
    }
    _httpHeaderControllers.clear();
    for (final name in ['User-Agent', 'Cookie', 'Referer']) {
      _httpHeaderControllers.add((
        name: TextEditingController(text: name),
        value: TextEditingController()
      ));
    }
    _originalRequest = null;
    _renameController.clear();
    _btTrackerController.clear();
    _archivePasswordController.clear();
    _proxyIpController.clear();
    _proxyPortController.clear();
    _proxyUsrController.clear();
    _proxyPwdController.clear();
    controller.proxyConfig.value = null;
    _skipVerifyCertController.value = false;
    _autoTorrentController.value = null;
    _deleteTorrentAfterDownloadController.value = null;
    _autoExtractController.value = null;
    _deleteAfterExtractController.value = null;
    final config = Get.find<AppController>().downloaderConfig.value;
    _connectionsController.text =
        config.protocolConfig.http.connections.toString();
    _pathController.text = renderPathPlaceholders(config.downloadDir);
    controller.directDownload.value = config.extra.defaultDirectDownload;
  }

  void _drainQueuedInputs() {
    if (!mounted ||
        controller.isConfirming.value ||
        _netdiskOpen.value ||
        _queuedInputs.isEmpty) return;
    final next = _queuedInputs.removeAt(0);
    MacPanel.cancelAutoHide();
    _resetTaskParameters();
    setState(() {
      _panelRequest = next;
      controller.pendingCreateHandled = false;
      _initialNetdiskHandled = false;
    });
  }

  void _clearCapturedHeadersFor(String value) {
    final origin = _credentialOrigin;
    if (origin == null) return;
    final next = Uri.tryParse(value.trim());
    if (next?.scheme == origin.scheme &&
        next?.host == origin.host &&
        next?.port == origin.port) return;
    for (final field in _httpHeaderControllers) {
      field.value.clear();
    }
    _credentialOrigin = null;
    _originalRequest = null;
  }

  Widget _buildMacPanel(
      BuildContext context, Widget fullForm, Widget torrentButton) {
    final c = MacColors.of(context);
    return Obx(() {
      final expanded = controller.showAdvanced.value;
      final taskList = Get.isRegistered<MacTaskListController>()
          ? Get.find<MacTaskListController>()
          : null;
      final tasks = taskList?.tasks
              .where((t) => t.status != Status.done)
              .take(3)
              .toList() ??
          <Task>[];
      final error = _panelError.value;
      WidgetsBinding.instance.addPostFrameCallback((_) {
        if (mounted && !_netdiskOpen.value && Get.isDialogOpen != true) {
          MacPanel.resize(expanded
              ? 700
              : 242 + tasks.length * 58 + (error == null ? 0 : 46));
        }
      });
      return CallbackShortcuts(
        bindings: {
          const SingleActivator(LogicalKeyboardKey.comma, meta: true): () =>
              MacPanel.showSettings(),
          const SingleActivator(LogicalKeyboardKey.escape): () {
            if (!_urlController.value.composing.isValid ||
                _urlController.value.composing.isCollapsed) {
              _cancelCreate();
            }
          },
        },
        child: Material(
            color: Colors.transparent,
            child: Column(children: [
              Padding(
                  padding: const EdgeInsets.fromLTRB(24, 20, 16, 12),
                  child: Row(children: [
                    MacSymbol('link',
                        fallback: CupertinoIcons.link,
                        size: 28,
                        color: c.secondary),
                    const SizedBox(width: 14),
                    Expanded(
                        child: Column(
                            crossAxisAlignment: CrossAxisAlignment.start,
                            children: [
                          Text('添加下载',
                              style: TextStyle(
                                  fontSize: 20,
                                  fontWeight: FontWeight.w600,
                                  color: c.text)),
                          const SizedBox(height: 3),
                          Text('kownload',
                              style:
                                  TextStyle(fontSize: 12, color: c.secondary)),
                        ])),
                    ValueListenableBuilder<String>(
                        valueListenable: MacPanel.hotKeyLabel,
                        builder: (_, label, __) => Text(label,
                            style:
                                TextStyle(fontSize: 12, color: c.secondary))),
                    const SizedBox(width: 8),
                    IconButton(
                        tooltip: '设置',
                        onPressed: () => MacPanel.showSettings(),
                        icon: const Icon(CupertinoIcons.gear, size: 18)),
                  ])),
              if (expanded && error != null)
                Padding(
                    padding:
                        const EdgeInsets.symmetric(horizontal: 24, vertical: 6),
                    child: Text(error,
                        maxLines: 2,
                        style: const TextStyle(
                            fontSize: 12, color: CupertinoColors.systemRed))),
              if (expanded)
                Expanded(child: fullForm)
              else
                Expanded(
                    child: SingleChildScrollView(
                        child: Padding(
                  padding: const EdgeInsets.fromLTRB(24, 0, 24, 18),
                  child: Column(
                      crossAxisAlignment: CrossAxisAlignment.stretch,
                      children: [
                        DropTarget(
                            onDragDone: (details) {
                              if (details.files.isNotEmpty) {
                                _urlController.text = details.files.first.path;
                                _originalRequest = null;
                                MacPanel.cancelAutoHide();
                              }
                            },
                            child: Form(
                                key: _confirmFormKey,
                                child: TextFormField(
                                  key: const ValueKey('panel-download-input'),
                                  controller: _urlController,
                                  focusNode: _inputFocus,
                                  autofocus: true,
                                  minLines: 1,
                                  maxLines: 3,
                                  textInputAction: TextInputAction.done,
                                  style: TextStyle(
                                      fontSize: 18,
                                      height: 1.35,
                                      color: c.text),
                                  decoration: InputDecoration(
                                    hintText: '粘贴下载链接，按回车开始…',
                                    hintStyle: TextStyle(
                                        fontSize: 16, color: c.secondary),
                                    fillColor: c.text.withValues(alpha: .035),
                                    contentPadding: const EdgeInsets.symmetric(
                                        horizontal: 16, vertical: 17),
                                    enabledBorder: OutlineInputBorder(
                                        borderRadius: BorderRadius.circular(12),
                                        borderSide:
                                            BorderSide(color: c.border)),
                                    focusedBorder: OutlineInputBorder(
                                        borderRadius: BorderRadius.circular(12),
                                        borderSide: BorderSide(
                                            color:
                                                c.accent.withValues(alpha: .75),
                                            width: 1.3)),
                                  ),
                                  validator: (v) => v?.trim().isNotEmpty == true
                                      ? null
                                      : '请填写下载链接或选择种子',
                                  onFieldSubmitted: (_) => _doConfirm(),
                                  onChanged: (value) {
                                    MacPanel.cancelAutoHide();
                                    _clearCapturedHeadersFor(value);
                                    _originalRequest = null;
                                    _netdiskRequest.value = null;
                                    _panelError.value = null;
                                    controller.clearFileDataUri();
                                    recognizeMagnetUri(value);
                                  },
                                ))),
                        const SizedBox(height: 10),
                        Row(children: [
                          _panelAction(c, CupertinoIcons.doc_on_clipboard, '粘贴',
                              () async {
                            final value = await Clipboard.getData('text/plain');
                            if (!mounted) return;
                            _urlController.text = value?.text ?? '';
                            _originalRequest = null;
                            MacPanel.cancelAutoHide();
                            _inputFocus.requestFocus();
                          }),
                          const SizedBox(width: 10),
                          _panelAction(c, CupertinoIcons.folder, '选择种子',
                              () async {
                            final result = await MacPanel.transient(() =>
                                FilePicker.platform.pickFiles(
                                    type: FileType.custom,
                                    allowedExtensions: ['torrent']));
                            if (mounted && result != null) {
                              _urlController.text =
                                  result.files.single.path ?? '';
                              _originalRequest = null;
                              MacPanel.cancelAutoHide();
                              _inputFocus.requestFocus();
                            }
                          }),
                          const SizedBox(width: 10),
                          _panelAction(
                              c,
                              CupertinoIcons.slider_horizontal_3,
                              '更多选项',
                              () => controller.showAdvanced.value = true),
                          const Spacer(),
                          if (controller.isConfirming.value)
                            const SizedBox(
                                width: 15,
                                height: 15,
                                child:
                                    CircularProgressIndicator(strokeWidth: 1.5))
                          else
                            IconButton(
                                tooltip: '开始下载',
                                onPressed: _doConfirm,
                                icon: Icon(CupertinoIcons.return_icon,
                                    size: 18, color: c.secondary)),
                        ]),
                        if (error != null)
                          Padding(
                              padding: const EdgeInsets.only(top: 8),
                              child: Text(error,
                                  maxLines: 2,
                                  overflow: TextOverflow.ellipsis,
                                  style: const TextStyle(
                                      fontSize: 12,
                                      color: CupertinoColors.systemRed))),
                        ValueListenableBuilder<String?>(
                            valueListenable: MacPanel.notice,
                            builder: (_, message, __) => message == null
                                ? const SizedBox.shrink()
                                : Padding(
                                    padding: const EdgeInsets.only(top: 8),
                                    child: Text(message,
                                        style: TextStyle(
                                            fontSize: 12, color: c.accent)))),
                        if (tasks.isNotEmpty) ...[
                          const SizedBox(height: 8),
                          Divider(color: c.border, height: 1),
                          const SizedBox(height: 4),
                          for (final task in tasks)
                            _compactTask(c, task, taskList!),
                        ],
                        const SizedBox(height: 4),
                        Align(
                            alignment: Alignment.centerRight,
                            child: TextButton(
                                onPressed: () =>
                                    MacPanel.showSettings(section: 'tasks'),
                                child: Text('全部任务',
                                    style: TextStyle(
                                        fontSize: 11, color: c.secondary)))),
                      ]),
                ))),
            ])),
      );
    });
  }

  Widget _panelAction(
          MacColors c, IconData icon, String label, VoidCallback action) =>
      TextButton.icon(
          onPressed: action,
          icon: Icon(icon, size: 15),
          label: Text(label, style: const TextStyle(fontSize: 12)),
          style: TextButton.styleFrom(
              foregroundColor: c.secondary,
              padding: const EdgeInsets.symmetric(horizontal: 8),
              minimumSize: const Size(0, 30),
              tapTargetSize: MaterialTapTargetSize.shrinkWrap));

  Widget _compactTask(MacColors c, Task task, MacTaskListController list) {
    final running = task.status == Status.running;
    final failed = task.status == Status.error;
    final total = task.meta.res?.size ?? 0;
    return SizedBox(
        height: 58,
        child: Row(children: [
          Icon(
              failed
                  ? CupertinoIcons.exclamationmark_circle
                  : CupertinoIcons.arrow_down_circle,
              size: 22,
              color: failed ? CupertinoColors.systemRed : c.secondary),
          const SizedBox(width: 10),
          Expanded(
              child: Column(
                  mainAxisAlignment: MainAxisAlignment.center,
                  crossAxisAlignment: CrossAxisAlignment.start,
                  children: [
                Text(task.name,
                    maxLines: 1,
                    overflow: TextOverflow.ellipsis,
                    style: TextStyle(
                        fontSize: 12,
                        fontWeight: FontWeight.w500,
                        color: c.text)),
                const SizedBox(height: 4),
                LinearProgressIndicator(
                    minHeight: 2,
                    value: total > 0
                        ? (task.progress.downloaded / total).clamp(0, 1)
                        : 0),
                const SizedBox(height: 4),
                Text(
                    failed
                        ? '下载失败 · 查看详情'
                        : running
                            ? '${Util.fmtByte(task.progress.downloaded)} · ${Util.fmtByte(task.progress.speed)}/s'
                            : task.status == Status.pause
                                ? '已暂停'
                                : '等待下载',
                    style: TextStyle(fontSize: 10, color: c.secondary)),
              ])),
          if (failed)
            IconButton(
                tooltip: '查看错误',
                onPressed: () => _showTaskError(task),
                icon: const Icon(CupertinoIcons.info_circle, size: 16)),
          IconButton(
              tooltip: running ? '暂停' : '继续',
              onPressed: () async {
                try {
                  running
                      ? await pauseTask(task.id)
                      : await continueTask(task.id);
                  await list.getTasksState();
                } catch (e) {
                  _panelError.value = e.toString();
                }
              },
              icon: Icon(running ? CupertinoIcons.pause : CupertinoIcons.play,
                  size: 16)),
        ]));
  }

  Future<void> _showTaskError(Task task) async {
    var reason = '下载失败。可重试；网盘任务可在任务管理中重新同步登录。';
    try {
      final file = File(path.join(logsDir(), 'core.log'));
      final handle = await file.open();
      try {
        final length = await handle.length();
        await handle.setPosition(length > 131072 ? length - 131072 : 0);
        final lines = utf8
            .decode(await handle.read(131072), allowMalformed: true)
            .split('\n');
        for (final line in lines.reversed) {
          if (!line.contains(task.id)) continue;
          try {
            final entry = jsonDecode(line);
            if (entry is Map && entry['error'] is String) {
              reason = (entry['error'] as String)
                  .replaceAll(RegExp(r'https?://\S+'), '[下载地址]')
                  .replaceAll(RegExp(r'Bearer\s+\S+', caseSensitive: false),
                      'Bearer …');
              break;
            }
          } catch (_) {}
        }
      } finally {
        await handle.close();
      }
    } catch (_) {}
    if (!mounted) return;
    MacPanel.resize(380);
    await showDialog<void>(
        context: context,
        builder: (dialogContext) => AlertDialog(
              title:
                  Text(task.name, maxLines: 1, overflow: TextOverflow.ellipsis),
              content: SelectableText(reason),
              actions: [
                TextButton(
                    onPressed: () => Navigator.of(dialogContext).pop(),
                    child: const Text('关闭'))
              ],
            ));
    if (mounted) setState(() {});
  }

  // parse protocol from url
  parseProtocol(String url) {
    if (netdiskProviderForUrl(url.trim()) != null) return Protocol.netdisk;
    final uppercaseUrl = url.toUpperCase();
    Protocol? protocol;
    if (uppercaseUrl.startsWith("HTTP:") || uppercaseUrl.startsWith("HTTPS:")) {
      protocol = Protocol.http;
    }
    if (uppercaseUrl.startsWith("MAGNET:") ||
        uppercaseUrl.endsWith(".TORRENT")) {
      protocol = Protocol.bt;
    }
    if (uppercaseUrl.startsWith("ED2K:")) {
      protocol = Protocol.ed2k;
    }
    return protocol;
  }

  // recognize magnet uri, if length == 40, auto add magnet prefix
  recognizeMagnetUri(String text) {
    if (text.length != 40) {
      return;
    }
    final exp = RegExp(r"[0-9a-fA-F]+");
    if (exp.hasMatch(text)) {
      final uri = "magnet:?xt=urn:btih:$text";
      _urlController.text = uri;
      _urlController.selection = TextSelection.fromPosition(
          TextPosition(offset: _urlController.text.length));
    }
  }

  Future<void> _doConfirm() async {
    if (controller.isConfirming.value) {
      return;
    }
    controller.isConfirming.value = true;
    _panelError.value = null;
    MacPanel.cancelAutoHide();
    try {
      _confirmController.start();
      // An existing netdisk task owns its file/source, including transferred
      // copies. Browser/native input must never turn recovery into a new batch.
      if (await _showPendingNetdiskTask()) return;
      if (_confirmFormKey.currentState!.validate()) {
        final isWebFileChosen =
            Util.isWeb() && controller.fileDataUri.isNotEmpty;
        var submitUrl = isWebFileChosen
            ? controller.fileDataUri.value
            : _urlController.text.trim();

        // Intercept before history, extension resolve, pending HTTP updates,
        // direct download, and the multiline shortcut.
        if (_netdiskRequest.value != null) {
          if (await _showNetdisk(
              NetdiskInput.fromRequest(_netdiskRequest.value!))) {
            _netdiskRequest.value = null;
            _completeCreate();
          }
          return;
        }
        final netdiskInputs = parseNetdiskInputs(submitUrl);
        if (netdiskInputs.isNotEmpty) {
          final ordinary = ordinaryNetdiskCompanionLinks(submitUrl);
          for (var i = 0; i < netdiskInputs.length; i++) {
            if (!await _showNetdisk(netdiskInputs[i])) return;
            // Keep only pending inputs so a later cancel cannot duplicate a
            // batch already created earlier in this mixed submission.
            _urlController.text = [
              for (final input in netdiskInputs.skip(i + 1))
                '${input.source.url}${input.source.password.isEmpty ? '' : ' 提取码：${input.source.password}'}',
              ...ordinary,
            ].join('\n');
          }
          if (ordinary.isEmpty) {
            _completeCreate();
            return;
          }
          submitUrl = ordinary.join('\n');
        }

        final urls = Util.textToLines(submitUrl);

        // Check if there is a pending update task (only for single URL)
        if (urls.length == 1) {
          final appController = Get.find<AppController>();
          final pendingTask = appController.pendingUpdateTask.value;
          if (pendingTask != null) {
            final shouldUpdate = await _showPendingUpdateDialog(
                pendingTask.id, pendingTask.name);
            if (shouldUpdate == true) {
              // Update the pending task instead of creating a new one
              await _updatePendingTask(pendingTask.id, urls.first);
              return;
            } else if (shouldUpdate == null) {
              // User cancelled, don't create task either
              return;
            }
            // shouldUpdate == false, continue to create new task
          }
        }

        // Add url to the history
        if (!isWebFileChosen) {
          for (final url in urls) {
            Database.instance.saveCreateHistory(url);
          }
        }

        /*
        Check if is direct download, there has two ways to direct download
        1. Direct download option is checked
        2. Muli line urls
        */
        final isMultiLine = urls.length > 1;
        final isDirect = controller.directDownload.value || isMultiLine;
        final opt = Options(
          name: isMultiLine ? "" : _renameController.text,
          path: _pathController.text,
          selectFiles: [],
          extra: parseReqOptsExtra(),
        );
        if (isDirect) {
          await Future.wait(urls.map((url) {
            return _createTask(CreateTask(
              req: _requestForUrl(url),
              opts: opt,
            ));
          }));
          _completeCreate();
        } else {
          final rr = await resolve(ResolveTask(
            req: _requestForUrl(submitUrl),
            opts: opt,
          ));
          await _showResolveDialog(rr);
        }
      }
    } catch (e) {
      _panelError.value = e.toString();
      if (!Util.isMacos()) showErrorMessage(e);
    } finally {
      _confirmController.reset();
      controller.isConfirming.value = false;
      _drainQueuedInputs();
    }
  }

  /// Returns true when this is a netdisk recovery, including cancellation.
  Future<bool> _showPendingNetdiskTask() async {
    final appController = Get.find<AppController>();
    final pending = appController.pendingUpdateTask.value;
    if (pending == null) return false;
    final api = netdiskApi ?? NetdiskApi();
    final task = await api.getTask(pending.id);
    if (task.meta.req.protocol != 'netdisk') return false;
    final resumed = await _showNetdisk(NetdiskInput.fromRequest(task.meta.req),
        options: Options.fromJson(task.meta.opts.toJson()),
        proxy: task.meta.req.proxy,
        onResume: () => api.continueTask(task.id));
    if (resumed) {
      if (appController.pendingUpdateTask.value?.id == task.id) {
        appController.pendingUpdateTask.value = null;
      }
      _netdiskRequest.value = null;
      _completeCreate();
    }
    return true;
  }

  Future<bool> _showNetdisk(NetdiskInput input,
      {Options? options,
      RequestProxy? proxy,
      Future<void> Function()? onResume}) async {
    if (_netdiskOpen.value) return false;
    _netdiskOpen.value = true;
    if (Util.isMacos()) MacPanel.resize(700);
    try {
      final opts = options ?? Options(extra: parseReqOptsExtra());
      if (opts.path.isEmpty) opts.path = _pathController.text;
      return await showDialog<bool>(
            context: Get.context!,
            barrierDismissible: false,
            builder: (_) => NetdiskView(
                input: input,
                options: opts,
                proxy: onResume != null
                    ? proxy
                    : _netdiskRequest.value?.proxy ?? parseProxy(),
                api: netdiskApi,
                browserTool: netdiskBrowserTool,
                onResume: onResume),
          ) ==
          true;
    } finally {
      _netdiskOpen.value = false;
      if (mounted) setState(() {});
    }
  }

  /// Shows a dialog to ask if user wants to update pending task or create new
  /// Returns true to update, false to create new, null if cancelled
  Future<bool?> _showPendingUpdateDialog(String taskId, String taskName) async {
    return showDialog<bool>(
      context: Get.context!,
      barrierDismissible: false,
      builder: (context) => AlertDialog(
        title: Text('pendingUpdateFound'.tr),
        content: Text('pendingUpdateConfirm'.trParams({'name': taskName})),
        actions: [
          TextButton(
            onPressed: () => Navigator.of(context).pop(null),
            child: Text('cancel'.tr),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(false),
            child: Text('pendingUpdateNo'.tr),
          ),
          TextButton(
            onPressed: () => Navigator.of(context).pop(true),
            child: Text('pendingUpdateYes'.tr),
          ),
        ],
      ),
    );
  }

  /// Updates the pending task with new URL and headers
  Future<void> _updatePendingTask(String taskId, String newUrl) async {
    try {
      // Build headers from current form
      final headers = <String, String>{};
      for (final c in _httpHeaderControllers) {
        final key = c.name.text.trim();
        final value = c.value.text.trim();
        if (key.isNotEmpty) {
          headers[key] = value;
        }
      }

      // Build ReqExtraHttp
      final reqExtra = ReqExtraHttp(header: headers);

      // Create patch request
      final patchData = ResolveTask(
        req: Request(
          url: newUrl,
          extra: reqExtra.toJson(),
          proxy: parseProxy(),
          skipVerifyCert: _skipVerifyCertController.value,
        ),
      );

      await patchTask(taskId, patchData);
      await continueTask(taskId);

      // Clear pending update state
      final appController = Get.find<AppController>();
      appController.pendingUpdateTask.value = null;

      _completeCreate();
    } catch (e) {
      showErrorMessage(e);
    }
  }

  RequestProxy? parseProxy() {
    if (controller.proxyConfig.value?.mode == RequestProxyMode.custom) {
      return RequestProxy()
        ..mode = RequestProxyMode.custom
        ..scheme = controller.proxyConfig.value?.scheme ?? 'http'
        ..host = "${_proxyIpController.text}:${_proxyPortController.text}"
        ..usr = _proxyUsrController.text
        ..pwd = _proxyPwdController.text;
    }
    return controller.proxyConfig.value;
  }

  Object? parseReqExtra(String url) {
    Object? reqExtra;
    final protocol = parseProtocol(url);
    switch (protocol) {
      case Protocol.http:
        final header = Map<String, String>.fromEntries(_httpHeaderControllers
            .map((e) => MapEntry(e.name.text, e.value.text)));
        header.removeWhere(
            (key, value) => key.trim().isEmpty || value.trim().isEmpty);
        if (header.isNotEmpty) {
          reqExtra = ReqExtraHttp()..header = header;
        }
        break;
      case Protocol.bt:
        if (_btTrackerController.text.trim().isNotEmpty) {
          reqExtra = ReqExtraBt()
            ..trackers = Util.textToLines(_btTrackerController.text);
        }
        break;
      case Protocol.ed2k:
      case Protocol.netdisk:
      case Protocol.media:
      case null:
        break;
    }
    return reqExtra;
  }

  Object? parseReqOptsExtra() {
    return OptsExtraHttp()
      ..connections = int.tryParse(_connectionsController.text) ?? 0
      ..autoTorrent = _autoTorrentController.value
      ..deleteTorrentAfterDownload = _deleteTorrentAfterDownloadController.value
      ..autoExtract = _autoExtractController.value
      ..archivePassword = _archivePasswordController.text
      ..deleteAfterExtract = _deleteAfterExtractController.value ?? false;
  }

  String _hitText() {
    return 'downloadLinkHit'.trParams({
      'append':
          Util.isDesktop() || Util.isWeb() ? 'downloadLinkHitDesktop'.tr : '',
    });
  }

  Future<void> _showResolveDialog(ResolveResult rr) async {
    if (Util.isMacos() &&
        rr.res.files.length == 1 &&
        parseProtocol(_urlController.text.trim()) == Protocol.http) {
      final file = rr.res.files.single;
      if (rr.id.isNotEmpty) {
        await _createTask(CreateTask(
            rid: rr.id,
            opts: Options(
                name: _renameController.text,
                path: _pathController.text,
                selectFiles: [0],
                extra: parseReqOptsExtra())));
      } else if (file.req != null) {
        await _createTask(CreateTask(
            req: file.req,
            opts: Options(
                name: file.name,
                path: path.join(_pathController.text, rr.res.name, file.path),
                selectFiles: [],
                extra: parseReqOptsExtra())));
      } else {
        throw StateError('解析结果缺少下载来源');
      }
      await _completeCreate();
      return;
    }
    if (Util.isMacos()) MacPanel.resize(700);
    final createFormKey = GlobalKey<FormState>();
    final downloadController = RoundedLoadingButtonController();
    return showDialog<void>(
        context: Get.context!,
        barrierDismissible: false,
        builder: (_) => AlertDialog(
              title: rr.res.name.isEmpty ? null : Text(rr.res.name),
              content: Builder(
                builder: (context) {
                  // Get available height and width of the build area of this widget. Make a choice depending on the size.
                  var height = MediaQuery.of(context).size.height;
                  var width = MediaQuery.of(context).size.width;

                  return SizedBox(
                    height: height * 0.75,
                    width: width,
                    child: Form(
                        key: createFormKey,
                        autovalidateMode: AutovalidateMode.always,
                        child: FileTreeView(
                          files: rr.res.files,
                          initialValues: rr.res.files.asMap().keys.toList(),
                          onSelectionChanged: (List<int> values) {
                            controller.selectedIndexes.value = values;
                          },
                        )),
                  );
                },
              ),
              actions: [
                ConstrainedBox(
                  constraints: BoxConstraints.tightFor(
                    width: Get.theme.buttonTheme.minWidth,
                    height: Get.theme.buttonTheme.height,
                  ),
                  child: ElevatedButton(
                    style:
                        ElevatedButton.styleFrom(shape: const StadiumBorder())
                            .copyWith(
                                backgroundColor: MaterialStateProperty.all(
                                    Get.theme.colorScheme.background)),
                    onPressed: () {
                      Get.back();
                    },
                    child: Text('cancel'.tr),
                  ),
                ),
                ConstrainedBox(
                  constraints: BoxConstraints.tightFor(
                    width: Get.theme.buttonTheme.minWidth,
                    height: Get.theme.buttonTheme.height,
                  ),
                  child: RoundedLoadingButton(
                      color: Get.theme.colorScheme.secondary,
                      onPressed: () async {
                        try {
                          downloadController.start();
                          if (controller.selectedIndexes.isEmpty) {
                            showMessage('tip'.tr, 'noFileSelected'.tr);
                            return;
                          }
                          final optExtra = parseReqOptsExtra();
                          if (createFormKey.currentState!.validate()) {
                            if (rr.id.isEmpty) {
                              // from extension resolve result
                              final reqs =
                                  controller.selectedIndexes.map((index) {
                                final file = rr.res.files[index];
                                return CreateTaskBatchItem(
                                    req: file.req!..proxy = parseProxy(),
                                    opts: Options(
                                        name: file.name,
                                        path: path.join(_pathController.text,
                                            rr.res.name, file.path),
                                        selectFiles: [],
                                        extra: optExtra));
                              }).toList();
                              await createTaskBatch(
                                  CreateTaskBatch(reqs: reqs));
                            } else {
                              await _createTask(CreateTask(
                                  rid: rr.id,
                                  opts: Options(
                                      name: _renameController.text,
                                      path: _pathController.text,
                                      selectFiles: controller.selectedIndexes,
                                      extra: optExtra)));
                            }
                            Get.back();
                            _completeCreate();
                          }
                        } catch (e) {
                          showErrorMessage(e);
                        } finally {
                          downloadController.reset();
                        }
                      },
                      controller: downloadController,
                      child: Text(
                        'download'.tr,
                        // style: controller.selectedIndexes.isEmpty
                        //     ? Get.textTheme.disabled
                        //     : Get.textTheme.titleSmall
                      )),
                ),
              ],
            ));
  }

  Widget _buildCategorySelector(AppController appController) {
    final categories =
        appController.downloaderConfig.value.extra.downloadCategories
            .where((c) => !c.isDeleted) // Filter out deleted categories
            .toList();
    if (categories.isEmpty) {
      return const SizedBox.shrink();
    }

    // Helper to get display name
    String getCategoryDisplayName(DownloadCategory category) {
      if (category.nameKey != null && category.nameKey!.isNotEmpty) {
        return category.nameKey!.tr;
      }
      return category.name;
    }

    return Padding(
      padding: const EdgeInsets.only(top: 8),
      child: Row(
        children: [
          Text(
            'selectCategory'.tr,
            style: TextStyle(
              color: Get.theme.hintColor,
              fontSize: 12,
            ),
          ),
          const SizedBox(width: 8),
          Expanded(
            child: SingleChildScrollView(
              scrollDirection: Axis.horizontal,
              child: Row(
                children: categories.map((category) {
                  return Padding(
                    padding: const EdgeInsets.only(right: 8),
                    child: OutlinedButton(
                      onPressed: () {
                        _pathController.text =
                            renderPathPlaceholders(category.path);
                      },
                      style: OutlinedButton.styleFrom(
                        padding: const EdgeInsets.symmetric(
                          horizontal: 12,
                          vertical: 4,
                        ),
                        minimumSize: Size.zero,
                      ),
                      child: Text(
                        getCategoryDisplayName(category),
                        style: const TextStyle(fontSize: 12),
                      ),
                    ),
                  );
                }).toList(),
              ),
            ),
          ),
        ],
      ),
    );
  }
}
