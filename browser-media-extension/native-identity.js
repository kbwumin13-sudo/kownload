import {browserClientId} from './app-identity.js';
import {NATIVE_HOST} from './native-host-identity.js';

export {NATIVE_HOST};
export const NATIVE_BINDING_KEY='nativeBridgeBindingV1';
const routes=new WeakMap();
const routeError=category=>Object.assign(new Error(`下载器连接需要检查（${category}）`),{category});
export const validBridgeBinding=b=>b!=null && typeof b==='object' && !Array.isArray(b) && Object.keys(b).length===3 &&
  b.version===1 && b.hostName===NATIVE_HOST && typeof b.routeScope==='string' && /^[a-f0-9]{64}$/.test(b.routeScope);
export const sameBridgeBinding=(a,b)=>validBridgeBinding(a) && validBridgeBinding(b) && a.hostName===b.hostName && a.routeScope===b.routeScope;

export function sendNativeRaw(browser,message) {
  return new Promise((resolve,reject)=>{
    try {browser.runtime.sendNativeMessage(NATIVE_HOST,message,reply=>{
      if(browser.runtime.lastError)reject(routeError('ipc_unavailable'));else resolve(reply);
    });}catch{reject(routeError('ipc_unavailable'));}
  });
}
function registry(browser) {
  if(!routes.has(browser)) {
    let savedBinding;
    const loaded=browser.storage.local.get(NATIVE_BINDING_KEY).then(value=>{
      const saved=value[NATIVE_BINDING_KEY];
      if(saved!=null && !validBridgeBinding(saved))throw routeError('binding_required');
      savedBinding=saved ? {...saved} : null;
    });
    let writing=Promise.resolve();
    routes.set(browser,{remember(candidate){
      const next=writing.then(async()=>{
        await loaded;
        if(savedBinding && !sameBridgeBinding(savedBinding,candidate))throw routeError('route_mismatch');
        if(!savedBinding) {
          await browser.storage.local.set({[NATIVE_BINDING_KEY]:candidate});
          savedBinding={...candidate};
        }
        return Object.freeze({...candidate});
      });
      // A failed persistence does not authorize a write, and a failed route
      // check never erases the original durable binding.
      writing=next.then(()=>{},()=>{});
      return next;
    }});
  }
  return routes.get(browser);
}
function hostInfo(browser,reply) {
  const info=reply?.data;
  if(reply?.code!==0 || info?.version!==1 || info.product!=='kownload' || info.nativeProtocol!==1 || info.identityProtocol!==1 ||
    !Array.isArray(info.supportedHostNames) || !info.supportedHostNames.includes(NATIVE_HOST) || info.callerOrigin!==`chrome-extension://${browser.runtime.id}/` ||
    typeof info.routeScope!=='string' || !/^[a-f0-9]{64}$/.test(info.routeScope) || typeof info.build?.preview!=='boolean' || typeof info.app?.reachable!=='boolean')throw routeError('identity_unsupported');
  if(info.app?.identity!=null) {
    const app=info.app.identity;
    if(app.version!==1 || app.product!=='kownload' || app.identityProtocol!==1)throw routeError('identity_unsupported');
    if(app.routeScope!==info.routeScope || app.build?.preview!==info.build.preview)throw routeError('route_mismatch');
  }
  return info;
}

/// Each short-lived Native process or real Port is probed independently. The
/// actual writing host also validates the envelope's expected routeScope.
export function createBoundTransport(browser,raw) {
  let initialized,info,binding;
  async function probe() {
    const clientId=await browserClientId(browser);
    const reportedBrowser=/Edg\//.test(globalThis.navigator?.userAgent || '') ? 'edge' : 'chrome';
    info=hostInfo(browser,await raw({method:'hostInfo',params:{version:1,reportedBrowser,clientId}}));
    return info;
  }
  function initialize(expected) {
    if(!initialized)initialized=(async()=>{
      await probe();
      const candidate={version:1,hostName:NATIVE_HOST,routeScope:info.routeScope};
      if(expected && !sameBridgeBinding(expected,candidate))throw routeError('route_mismatch');
      binding=await registry(browser).remember(candidate);
      return binding;
    })();
    return initialized.then(value=>{
      if(expected && !sameBridgeBinding(expected,value))throw routeError('route_mismatch');
      return {...value};
    });
  }
  const envelope=message=>({...message,identityProtocol:1,bridgeBinding:{...binding}});
  async function requireApp() {
    if(!info.app?.reachable)throw routeError('ipc_unavailable');
    if(!info.app.identity)throw routeError('identity_unsupported');
  }
  return {
    binding:initialize,
    async send(message) {
      await initialize(message.bridgeBinding);
      if(message.method==='hostInfo')return {code:0,data:info};
      if(message.method==='create' && !info.app?.reachable) {
        const wake=await raw(envelope({method:'wakeup',meta:message.meta}));
        if(wake?.code!==0)return wake;
        await probe();
      }
      if(!['wakeup','ping'].includes(message.method))await requireApp();
      const reply=await raw(envelope(message));
      if(message.method==='wakeup' && reply?.code===0) {await probe();await requireApp();}
      return reply;
    },
  };
}

export async function nativeBinding(browser,expected) {
  return createBoundTransport(browser,message=>sendNativeRaw(browser,message)).binding(expected);
}
export function sendNativeBound(browser,message) {
  return createBoundTransport(browser,request=>sendNativeRaw(browser,request)).send(message);
}
