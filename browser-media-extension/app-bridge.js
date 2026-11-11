import {nativeData,bridgeFailure} from './lib.js';
import {browserClientId} from './app-identity.js';
import {createNativeSession} from './native-session.js';
import {nativeBinding} from './native-identity.js';

const sites = value => Object.fromEntries(Object.entries(value && typeof value === 'object' && !Array.isArray(value) ? value : {}).filter(([host,disabled]) => host.length <= 253 && typeof disabled === 'boolean'));
function canonical(data) {
  const capture=data?.settings?.downloadCapture,sniffing=data?.settings?.sniffing;
  if (!Number.isSafeInteger(data?.revision) || data.revision < 0 || typeof capture?.enabled !== 'boolean' || typeof capture?.confirm !== 'boolean' || typeof sniffing?.enabled !== 'boolean' || typeof sniffing?.showAll !== 'boolean' || !sniffing.disabledSites || typeof sniffing.disabledSites !== 'object' || Array.isArray(sniffing.disabledSites)) throw bridgeFailure('invalid_response');
  return {revision:data.revision,settings:{downloadCapture:{enabled:capture.enabled,confirm:capture.confirm},sniffing:{enabled:sniffing.enabled,showAll:sniffing.showAll,disabledSites:sites(sniffing.disabledSites)}}};
}

export function createAppBridge(browser,send,{browserName=/Edg\//.test(globalThis.navigator?.userAgent || '') ? 'edge' : 'chrome'}={}) {
  let latest,writing=Promise.resolve(),channel,leases=0;
  function acquire() {
    if ((!channel || channel.closed) && browser.runtime.connectNative) channel=createNativeSession(browser);
    leases++;
    let released=false;
    return ()=>{if(released)return;released=true;if(--leases===0){channel?.close();channel=null;}};
  }
  const transmit=message=>{
    if (leases && browser.runtime.connectNative && (!channel || channel.closed)) channel=createNativeSession(browser);
    return channel ? channel.send(message) : send(message);
  };
  const identity=async()=>({clientId:await browserClientId(browser),browser:browserName});
  const binding=expected=>channel ? channel.binding(expected) : nativeBinding(browser,expected);
  async function request(action,params={},expectedBinding) {
    const reply=await transmit({method:'browser',params:{...params,action,...await identity()},...(expectedBinding?{bridgeBinding:expectedBinding}:{})});
    if (reply?.code !== 0) throw bridgeFailure(reply?.error);
    return nativeData(reply);
  }
  async function apply(data) {
    const value=canonical(data);
    if (latest && value.revision < latest.revision) return latest.settings;
    latest=value;
    writing=writing.catch(()=>{}).then(async()=>{
      if (value !== latest) return;
      // These are read caches of the App's canonical values. Preference edits
      // go through updateSettings; no extension UI writes a second authority.
      await browser.storage.local.set({browserSettingsCache:value,downloadCapture:value.settings.downloadCapture,disabledSites:value.settings.sniffing.disabledSites,showAll:value.settings.sniffing.showAll,sniffingEnabled:value.settings.sniffing.enabled});
    });
    await writing;
    return latest.settings;
  }
  async function settings(expectedBinding) {
    const old=await browser.storage.local.get(['downloadCapture','showAll','disabledSites']);
    const legacySettings={downloadCapture:{enabled:old.downloadCapture?.enabled === true,confirm:old.downloadCapture?.confirm !== false},showAll:old.showAll === true,disabledSites:sites(old.disabledSites)};
    const downloadsPermission=await browser.permissions?.contains({permissions:['downloads']}).catch(()=>false) || false;
    return apply(await request('sync',{legacySettings,downloadsPermission,version:browser.runtime.getManifest?.().version || '0.5.0'},expectedBinding));
  }
  async function wake(expectedBinding) {
    const reply=await transmit({method:'wakeup',meta:{silent:true},...(expectedBinding?{bridgeBinding:expectedBinding}:{})});
    if (reply?.code !== 0) throw bridgeFailure(reply?.error);
  }
  async function openSettings() { await wake();await settings();return request('openSettings'); }
  async function updateSettings(patch) { await wake();await settings();return apply(await request('updateSettings',{patch})); }
  return {identity,binding,request,settings,openSettings,updateSettings,wake,acquire,send:transmit};
}
