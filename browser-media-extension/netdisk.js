import {readNetdiskPage} from './netdisk-page.js';

const entry = (id,name,hosts,apiHosts,roots) => ({id,name,hosts,apiHosts,roots});
export const NETDISK_PROVIDERS = [
  entry('baidu','百度网盘',['pan.baidu.com','yun.baidu.com'],['pan.baidu.com','openapi.baidu.com'],['baidu.com']),
  entry('aliyun','阿里云盘',['www.aliyundrive.com','www.alipan.com','alipan.com'],['api.aliyundrive.com','api.alipan.com'],['aliyundrive.com','alipan.com']),
  entry('mcloud','中国移动云盘',['yun.139.com','caiyun.139.com'],['personal-kd-njs.yun.139.com','caiyun.139.com'],['139.com']),
  entry('tcloud','天翼云盘',['cloud.189.cn'],['api.cloud.189.cn'],['189.cn']),
  entry('xunlei','迅雷云盘',['pan.xunlei.com'],['api-pan.xunlei.com','xluser-ssl.xunlei.com'],['xunlei.com']),
  entry('quark','夸克网盘',['pan.quark.cn'],['drive-pc.quark.cn','drive-h.quark.cn'],['quark.cn']),
  entry('uc','UC网盘',['drive.uc.cn'],['pc-api.uc.cn'],['uc.cn']),
  entry('123pan','123云盘',['123pan.com','123pan.cn','123684.com','123865.com','123952.com','123912.com'],['api.123pan.cn','yun.123pan.com'],['123pan.com','123pan.cn','123684.com','123865.com','123952.com','123912.com']),
  entry('guangya','光鸭云盘',['www.guangyapan.com'],['api.guangyapan.com'],['guangyapan.com'])
];
const httpURL = raw => {try {const u=new URL(raw); return ['https:','http:'].includes(u.protocol) && !u.username && !u.password && !u.port ? u : null;} catch {return null;}};
export function detectNetdisk(raw) {
  const u=httpURL(raw);
  return u && NETDISK_PROVIDERS.find(p => p.hosts.some(h => u.hostname===h || p.id==='123pan' && u.hostname.endsWith('.'+h))) || null;
}
export function netdiskSourceIdentity(raw) {
  const u=httpURL(raw),provider=detectNetdisk(raw);
  if (!u || !provider) return null;
  const decode=v=>{try {return decodeURIComponent(v);} catch {return '';}};
  const parts=u.pathname.split('/').filter(Boolean).map(decode);
  const index=parts.findIndex(x=>['s','share','t'].includes(x));
  let shareId=index>=0 ? parts[index+1] || '' : '';
  const fragment=u.hash.slice(1),queryIndex=fragment.indexOf('?');
  const fragmentPath=queryIndex<0 ? fragment : fragment.slice(0,queryIndex);
  if (provider.id==='baidu' && parts.join('/')==='share/init') shareId=u.searchParams.get('surl') ? '1'+u.searchParams.get('surl') : '';
  if (provider.id==='mcloud' && parts.join('/')==='shareweb') {
    const route=fragmentPath.split('/').filter(Boolean).map(decode);
    if (route[0]==='w' && route[1]==='i') shareId=route[2] || '';
  }
  shareId ||= ['shareId','shareid','linkID','code'].map(k=>u.searchParams.get(k)).find(Boolean) || '';
  const passwordKeys=['pwd','password','pass_code','receive_code'];
  const password=passwordKeys.map(k=>u.searchParams.get(k)).find(Boolean) || (fragment.startsWith('pwd=') ? new URLSearchParams(fragment).get('pwd') : '') || '';
  return {provider:provider.id,shareId,password};
}
// Keep personal redirects explicit: an arbitrary path or SPA route is not home.
function personalHomeURL(provider,u) {
  const route=u.hash.slice(1).split('?')[0];
  if(provider==='quark' || provider==='uc') return u.host===(provider==='quark'?'pan.quark.cn':'drive.uc.cn') && (u.pathname==='/' && !route || u.pathname==='/list' && (!route || route==='/list/all'));
  if(provider==='mcloud') return ['yun.139.com','caiyun.139.com'].includes(u.host) && (u.pathname==='/' && !route || u.pathname==='/w/' && ['/index','/'].includes(route));
  if(provider==='tcloud') return u.host==='cloud.189.cn' && !route && ['/web/main','/web/main/','/web/main/file/folder/-11'].includes(u.pathname);
  if(provider==='aliyun') return ['www.alipan.com','alipan.com'].includes(u.host) && u.pathname==='/drive/file/all' && !route;
  return false;
}
function samePersonalSource(provider,expected,current) {
  const expectedHome=personalHomeURL(provider,expected),currentHome=personalHomeURL(provider,current);
  if(expected.host!==current.host && !(provider==='aliyun' && expectedHome && currentHome)) return false;
  return expectedHome || currentHome ? expectedHome && currentHome : expected.pathname===current.pathname;
}
export function netdiskHandoffRequestId(raw) {
  const u=httpURL(raw),provider=detectNetdisk(raw);
  if(!u || !provider) return '';
  const requestId=u.searchParams.get('gopeed_request');
  if(requestId) return requestId;
  // Mobile's / -> /w/#/index redirect moves the handoff into the SPA query.
  if(provider.id==='mcloud' && u.pathname==='/w/' && u.hash.includes('?')) {
    return new URLSearchParams(u.hash.slice(u.hash.indexOf('?')+1)).get('gopeed_request') || '';
  }
  return '';
}
const text = (v,max=4096) => (typeof v==='string' || typeof v==='number') && !/[\r\n\x00]/.test(String(v)) ? String(v).slice(0,max) : '';
const tokensAllowed = new Set(['access_token','accessToken','authorToken','token_type','user_id','default_drive_id','resource_drive_id','driveId','share_token','shareToken','stoken','bdstoken','sign','timestamp','jsToken','sekey','shareid','captcha_token','deviceid','client_id','LoginUuid','browserId']);
const shareAllowed = new Set(['shareId','password','parentId','uk','shareCode']);
const metaAllowed = new Set(['driveId','parentId','category','S3KeyFlag','StorageNode','share_fid_token']);
const pick = (raw,keys) => Object.fromEntries(Object.entries(raw || {}).filter(([k,v]) => keys.has(k) && text(v)).map(([k,v])=>[k,text(v)]));
export function cleanNetdiskFiles(raw, includeAuth=false) {
  if (!Array.isArray(raw) || raw.length>10000) throw new Error('invalid_capture');
  const seen=new Set();
  return raw.map(f => {
    const id=text(f?.id,256),name=text(f?.name,512),size=Number(f?.size);
    if (!id || !name || /[/\\]/.test(name) || ['.','..'].includes(name) || !Number.isSafeInteger(size) || size<0 || typeof f.isDir!=='boolean') throw new Error('invalid_capture');
    const path=text(f.path,2048);
    if (path && (path.startsWith('/') || path.includes('\\') || path.split('/').includes('..'))) throw new Error('invalid_capture');
    const meta=pick(f.meta,metaAllowed);
    if (!includeAuth) delete meta.share_fid_token;
    for (const [k,v] of Object.entries(meta)) if (/https?:\/\//i.test(v)) delete meta[k];
    return {id,name,size,isDir:f.isDir,...(path?{path}:{}),...(text(f.hash,256)?{hash:text(f.hash,256)}:{}),...(Object.keys(meta).length?{meta}:{})};
  }).filter(f => !seen.has(f.id) && seen.add(f.id));
}
export function sanitizeNetdiskCapture(raw,context) {
  if (!raw || raw.provider!==context.provider.id || raw.sourcePage!==context.url) throw new Error('stale_page');
  const share=pick(raw.share,shareAllowed);
  const identity=netdiskSourceIdentity(context.url);
  if (!identity || identity.provider!==context.provider.id) throw new Error('stale_page');
  // A URL's share identity cannot be replaced by page-supplied fields.
  if (identity.shareId) share.shareId=identity.shareId;
  if (identity.password) share.password=identity.password;
  const tokens=pick(raw.tokens,tokensAllowed);
  const headers={Referer:context.url};
  const access=tokens.access_token || tokens.authorToken;
  if(access) headers.Authorization=`Bearer ${access}`;
  if(tokens.captcha_token) headers['X-Captcha-Token']=tokens.captcha_token;
  if(tokens.deviceid) headers['X-Device-Id']=tokens.deviceid;
  if(tokens.client_id) headers['X-Client-Id']=tokens.client_id;
  if(tokens.accessToken) headers.AccessToken=tokens.accessToken;
  if(tokens.browserId) headers['Browser-Id']=tokens.browserId;
  if(tokens.LoginUuid) headers.LoginUuid=tokens.LoginUuid;
  return {provider:context.provider.id,sourcePage:context.url,browser:context.browser,instanceId:context.instanceId,contextId:context.contextId,
    ...(text(raw.accountId || tokens.user_id,256)?{accountId:text(raw.accountId || tokens.user_id,256)}:{}),headers,tokens,share,files:cleanNetdiskFiles(raw.files,true)};
}
// Only fixed messages can enter the bridge. Never accept a path, tabId, URL or cookies from content.
const messageTypes = new Set(['NETDISK_PROBE','NETDISK_CLICK']);
const errorMessages = {
  stale_page:'来源页面已变化，请回到网盘重新点击',
  selected_empty:'请先勾选要下载的文件',
  invalid_capture:'未识别选中文件，请刷新网盘页面后重试',
  cookie_context:'无法确认当前窗口的登录状态，请刷新页面后重试',
  timeout:'同步超时，请重试', native_error:'kownload 连接或同步失败，请打开应用后重试',
  invalid_message:'无效的网盘请求'
};
function restData(response) {
  if (response?.code!==0 || response.data?.code!==0) throw new Error('native_error');
  return response.data.data;
}
function taskPort(chrome,timeoutMs) {
  const port=chrome.runtime.connectNative('com.gopeed.gopeed');
  let pending,closed=false,closedReason='stale_page',resolveClosed;
  const done=new Promise(resolve=>{resolveClosed=resolve;});
  function close(reason='stale_page') {
    if (closed) return;
    closed=true;closedReason=reason;clearTimeout(timer);resolveClosed(reason);
    if (pending) {const p=pending;pending=null;p.reject(new Error(reason));}
    port.disconnect();
  }
  const timer=setTimeout(()=>close('timeout'),timeoutMs);
  port.onMessage.addListener(response=>{
    if (!pending) return;
    const p=pending;pending=null;
    if (response?.code!==0) p.reject(new Error('native_error')); else p.resolve(response);
  });
  port.onDisconnect.addListener(()=>{void chrome.runtime.lastError;close('native_error');});
  const ensureOpen=()=>{if(closed) throw new Error(closedReason);};
  return {close,ensureOpen,guard(operation) {
    ensureOpen();
    return Promise.race([operation,done.then(reason=>{throw new Error(reason);})]);
  },send(message) {
    if (closed) return Promise.reject(new Error(closedReason));
    if (pending) return Promise.reject(new Error('native_error'));
    return new Promise((resolve,reject)=>{pending={resolve,reject};try {port.postMessage(message);} catch {close('native_error');}});
  }};
}
export function installNetdiskBridge(chrome,{timeoutMs=45000,readPage=readNetdiskPage}={}) {
  const jobs=new Map(),epochs=new Map();
  let identity;
  const browser=/Edg\//.test(globalThis.navigator?.userAgent || '') ? 'edge' : 'chrome';
  async function instance() {
    if (!identity) identity=(async()=>{
      const saved=await chrome.storage.local.get('netdiskInstanceId');
      const id=text(saved.netdiskInstanceId,128) || globalThis.crypto.randomUUID();
      if (!saved.netdiskInstanceId) await chrome.storage.local.set({netdiskInstanceId:id});
      return id;
    })();
    return identity;
  }
  function invalidate(tabId) {
    epochs.set(tabId,(epochs.get(tabId)||0)+1);
    jobs.get(tabId)?.port?.close();
    jobs.delete(tabId);
  }
  for (const event of [chrome.webNavigation.onCommitted,chrome.webNavigation.onHistoryStateUpdated,chrome.webNavigation.onReferenceFragmentUpdated]) event.addListener(d=>{if(d.frameId===0) invalidate(d.tabId);});
  chrome.tabs.onRemoved.addListener(invalidate);
  const documentURL=raw=>{const u=httpURL(raw);if(!u)return '';u.searchParams.delete('gopeed_request');if(detectNetdisk(u.href)?.id==='tcloud' && personalHomeURL('tcloud',u))u.pathname='/web/main/';u.hash='';return u.href;};
  async function live(context) {
    const [tab,frame]=await Promise.all([chrome.tabs.get(context.tabId),chrome.webNavigation.getFrame({tabId:context.tabId,frameId:0})]);
    if (!frame || tab.id!==context.tabId || frame.documentId!==context.documentId || documentURL(frame.url)!==documentURL(context.url) || tab.url!==context.url || frame.documentLifecycle && frame.documentLifecycle!=='active' || (epochs.get(context.tabId)||0)!==context.epoch) throw new Error('stale_page');
  }
  async function senderContext(sender) {
    if (sender.id!==chrome.runtime.id || sender.frameId!==0 || sender.documentLifecycle && sender.documentLifecycle!=='active' || !Number.isInteger(sender.tab?.id) || !text(sender.documentId,256) || !detectNetdisk(sender.url)) throw new Error('stale_page');
    const tab=await chrome.tabs.get(sender.tab.id);
    const provider=detectNetdisk(tab.url);
    if (!provider || provider.id!==detectNetdisk(sender.url).id || documentURL(tab.url)!==documentURL(sender.url)) throw new Error('stale_page');
    const context={provider,url:tab.url,tabId:sender.tab.id,documentId:sender.documentId,epoch:epochs.get(sender.tab.id)||0,browser};
    await live(context);
    return context;
  }
  async function bindContext(context,sender) {
    const stores=await chrome.cookies.getAllCookieStores().catch(()=>[]);
    context.storeId=stores.find(s=>s.tabIds?.includes(context.tabId))?.id;
    if (!context.storeId) throw new Error('cookie_context');
    context.instanceId=`${await instance()}:${context.storeId}:${sender.tab.incognito ? 'private' : 'normal'}`;
    context.contextId=[context.tabId,0,context.documentId,context.epoch,context.storeId].join(':');
    await live(context);
  }
  async function cookiesFor(context,check) {
    await check();
    let partitionKey;
    if (chrome.cookies.getPartitionKey) {
      const result=await chrome.cookies.getPartitionKey({tabId:context.tabId,frameId:0,documentId:context.documentId});
      await check();
      partitionKey=result?.partitionKey || result;
      if (!partitionKey?.topLevelSite) throw new Error('cookie_context');
    }
    const cookies=[];
    for (const domain of context.provider.roots) {
      await check();
      const query={domain,storeId:context.storeId};
      cookies.push(...await chrome.cookies.getAll(query));
      await check();
      if (partitionKey) {
        cookies.push(...await chrome.cookies.getAll({...query,partitionKey}));
        await check();
      }
    }
    const hosts=[new URL(context.url).hostname,...context.provider.apiHosts];
    const unique=new Map();
    for (const c of cookies) {
      const domain=c.domain?.replace(/^\./,'').toLowerCase();
      if (!domain || !hosts.some(h=>h===domain || h.endsWith('.'+domain))) continue;
      if (c.storeId && c.storeId!==context.storeId || c.partitionKey && (c.partitionKey.topLevelSite!==partitionKey?.topLevelSite || !!c.partitionKey.hasCrossSiteAncestor!==!!partitionKey?.hasCrossSiteAncestor)) continue;
      const value={name:text(c.name,256),value:text(c.value,8192),domain:c.domain,path:text(c.path,2048)||'/',secure:!!c.secure,...(Number.isFinite(c.expirationDate)&&c.expirationDate>0?{expires:c.expirationDate}:{})};
      if (value.name) unique.set([value.name,value.domain,value.path].join('\u001f'),value);
    }
    if(unique.size>512) throw new Error('cookie_context');
    return [...unique.values()];
  }
  async function run(type,sender) {
    const context=await senderContext(sender);
    if (jobs.has(context.tabId)) return {state:'busy'};
    const job={};jobs.set(context.tabId,job);
    try {
      await live(context);
      const port=job.port=taskPort(chrome,timeoutMs);
      const check=async()=>{port.ensureOpen();await live(context);port.ensureOpen();};
      const forward=async(path,data)=>restData(await port.send({method:'forward',params:{method:'POST',path:'/api/v1'+path,data}}));
      let claim;
      if(type==='NETDISK_PROBE') {
        let ping;
        try {ping=await port.send({method:'ping'});} catch(error) {if(error.message==='native_error') return {state:'idle'};throw error;}
        if (ping.data!==true) return {state:'idle'};
        await check();
        await port.guard(bindContext(context,sender));
        await check();
        const requestId=netdiskHandoffRequestId(context.url);
        claim=await forward('/netdisk/browser/claim',{sourcePage:context.url,instanceId:context.instanceId,browser,contextId:context.contextId,...(requestId?{requestId}:{})});
        if(!claim) return {state:'idle'};
        const request=claim.request;
        if (!text(request?.id,128) || !text(claim.nonce,256) || request.instanceId && request.instanceId!==context.instanceId || request.source?.provider!==context.provider.id || detectNetdisk(request.source?.url)?.id!==context.provider.id || Date.parse(request.expiresAt)<=Date.now() || !Number.isFinite(Date.parse(request.expiresAt))) throw new Error('stale_page');
        const expected=httpURL(request.source.url),current=httpURL(context.url);
        const expectedIdentity=netdiskSourceIdentity(request.source.url),currentIdentity=netdiskSourceIdentity(context.url);
        const expectedShare=request.source.shareId || expectedIdentity.shareId,currentShare=currentIdentity.shareId;
        if (request.source.shareId && expectedIdentity.shareId && request.source.shareId!==expectedIdentity.shareId || (expectedShare || currentShare ? expectedShare!==currentShare : !samePersonalSource(context.provider.id,expected,current))) throw new Error('stale_page');
      } else {
        await port.send({method:'wakeup',meta:{silent:false}});
        await port.guard(bindContext(context,sender));
      }
      await check();
      const results=await port.guard(chrome.scripting.executeScript({target:{tabId:context.tabId,documentIds:[context.documentId]},world:'MAIN',func:readPage,args:[context.provider.id,context.url]}));
      await check();
      const result=results.find(r=>r.frameId===0 && r.documentId===context.documentId);
      const capture=sanitizeNetdiskCapture(result?.result,context);
      // The site's own SDK initializes these after document_start. Keep the
      // existing bounded handoff polling alive until its current auth is ready.
      if(type==='NETDISK_PROBE' && !netdiskSourceIdentity(context.url).shareId && (
        context.provider.id==='xunlei' && (!capture.tokens.access_token || !capture.tokens.captcha_token || !capture.tokens.deviceid) ||
        context.provider.id==='tcloud' && !capture.tokens.browserId
      )) return {state:'waiting'};
      if(type==='NETDISK_CLICK' && !capture.files.length) throw new Error('selected_empty');
      capture.cookies=await port.guard(cookiesFor(context,check));
      if(context.provider.id==='mcloud') {
        const authorization=capture.cookies.find(c=>c.name.toLowerCase()==='authorization')?.value;
        if(authorization) capture.headers.Authorization=authorization;
      }
      await check();
      const data=await forward('/netdisk/browser/capture',{...(claim?{requestId:claim.request.id,nonce:claim.nonce}:{}),capture});
      await check();
      if (!text(data?.sessionId,256) || data.source?.provider!==context.provider.id) throw new Error('native_error');
      if(type==='NETDISK_CLICK') {
        const source={provider:context.provider.id,url:context.url,...pick(capture.share,new Set(['shareId','password','parentId']))};
        // Use our selected snapshot, never a server-returned whole listing.
        await port.send({method:'create',meta:{silent:false},params:{req:{url:context.url,protocol:'netdisk',extra:{provider:context.provider.id,source,sessionId:data.sessionId,files:cleanNetdiskFiles(capture.files)}},opts:{}}});
      }
      return {state:type==='NETDISK_CLICK'?'confirm':'synced',count:capture.files.length};
    } finally {
      job.port?.close();
      if(jobs.get(context.tabId)===job) jobs.delete(context.tabId);
    }
  }
  chrome.runtime.onMessage.addListener((msg,sender,respond)=>{
    if (typeof msg?.type!=='string' || !msg.type.startsWith('NETDISK_')) return false;
    if (!messageTypes.has(msg.type) || Object.keys(msg).some(k=>k!=='type')) {respond({error:errorMessages.invalid_message});return false;}
    run(msg.type,sender).then(respond,error=>respond({error:errorMessages[error?.message] || errorMessages.native_error}));
    return true;
  });
}
