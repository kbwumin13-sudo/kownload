const byId = id => document.getElementById(id);
const t = (key, values) => chrome.i18n.getMessage(key, values);
const check = byId('check');
const syncSettings = byId('sync-settings');
const permission = byId('permission');
const status = byId('status');
const downloads = {permissions:['downloads']};

document.documentElement.lang = chrome.i18n.getUILanguage();
for (const el of document.querySelectorAll('[data-i18n]')) {
  el.textContent = t(el.dataset.i18n);
}
document.title = t('title');
byId('extension').textContent = t('extension', [chrome.runtime.getManifest().version]);

const hostLabels = {ready:'ready',missing:'hostMissing',forbidden:'hostForbidden',failed:'hostFailed',timeout:'hostTimeout',unsupported:'hostUnsupported'};
const appLabels = {reachable:'reachable',unreachable:'unreachable'};
const identityLabels = {matched:'matched',binding:'bindingMismatch',bindingUnknown:'bindingUnknown',mismatch:'routeMismatch',unsupported:'appUnsupported'};

function render(report) {
  byId('host').textContent = t(hostLabels[report?.host] || 'unchecked');
  byId('app').textContent = t(appLabels[report?.app] || 'unchecked');
  byId('identity').textContent = t(identityLabels[report?.identity] || 'unchecked');
  byId('sync').textContent = Number.isSafeInteger(report?.syncedAt)
    ? t('lastSync', [new Date(report.syncedAt).toLocaleString(chrome.i18n.getUILanguage())])
    : t(report?.syncKnown ? 'noSync' : 'unchecked');
  const repair = report?.host === 'ready'
    ? report.identity === 'binding' || report.identity === 'mismatch' ? 'repairRoute'
      : report.app === 'unreachable' ? 'repairApp'
        : report.identity === 'unsupported' ? 'repairVersion'
          : report.identity === 'bindingUnknown' ? 'repairStorage' : null
    : report?.host === 'unsupported' ? 'repairVersion' : report ? 'repairHost' : null;
  byId('repair').hidden = !repair;
  byId('repair').textContent = repair ? t(repair) : '';
  byId('checked-at').textContent = report
    ? t('checkedAt', [new Date().toLocaleTimeString(chrome.i18n.getUILanguage())]) : '';
  syncSettings.disabled = report?.identity !== 'matched';
}

function notify(key) {
  status.hidden = !key;
  status.textContent = key ? t(key) : '';
}
async function request(message) {
  const response = await chrome.runtime.sendMessage(message);
  if (!response?.ok) throw new Error('request_failed');
  return response.data;
}
async function refresh() {
  check.disabled = syncSettings.disabled = true;
  render(null);
  try {
    render(await request({type:'CONNECTION_CHECK'}));
  } catch {
    notify('actionFailed');
  } finally {
    check.disabled = false;
  }
}
check.addEventListener('click',() => { notify(null); void refresh(); });
syncSettings.addEventListener('click',async () => {
  check.disabled = syncSettings.disabled = true;
  notify(null);
  try {
    await request({type:'APP_BRIDGE',action:'settings'});
    notify('syncDone');
  } catch {
    notify('actionFailed');
  }
  await refresh();
});
async function open(message) {
  try { await request(message); window.close(); }
  catch { notify('actionFailed'); }
}
byId('batch-preview').addEventListener('click',async event => {
  const button = event.currentTarget;
  button.disabled = true;
  try {
    const result = await request({type:'BATCH_PREVIEW_SELECTION'});
    if (result === 'selectionOpened') window.close();
    else notify(result || 'actionFailed');
  } catch { notify('actionFailed'); }
  finally { button.disabled = false; }
});
byId('resources').addEventListener('click',() => open({type:'APP_MEDIA_OPEN'}));
byId('settings').addEventListener('click',() => open({type:'APP_BRIDGE',action:'openSettings'}));
permission.addEventListener('click',async () => {
  try {
    const granted = await chrome.permissions.request(downloads);
    permission.hidden = granted;
    notify(granted ? 'permissionGranted' : 'permissionDeclined');
    if (granted) await request({type:'APP_BRIDGE',action:'settings'}).catch(() => notify('permissionSync'));
  } catch { notify('actionFailed'); }
});
render(null);
void chrome.permissions.contains(downloads).then(granted => { permission.hidden = granted; }, () => {});
