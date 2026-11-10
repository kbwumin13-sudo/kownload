// Independently implemented against Chrome downloads and our Native contract.
import {newOperationId,nativeSubmission,safeHeader,protectedHeaders,createAppBridge} from './lib.js';

const PENDING='downloadCapturePending';
const httpURL=value=>{try{return /^https?:$/.test(new URL(value).protocol);}catch{return false;}};
const validId=value=>typeof value==='string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value);
const rejected=reply=>reply?.code !== 0 && (
  ['invalid_request','unknown_method','wakeup_failed','ipc_unavailable'].includes(reply?.error) ||
  reply?.error==='business_error' && Number.isInteger(reply.businessCode) && reply.businessCode !== 0
);

export function installDownloadCapture(browser,send,{appBridge=createAppBridge(browser,send),pollMs=1000}={}) {
  if (!browser.permissions) return;
  const jobs=new Map(),requests=new Map(),earlyPauses=new Map();
  let listening=false,granted=false,activeConfig,requestsBlockedUntil=0;
  browser.storage.local.get('downloadCapture').then(saved=>{activeConfig=saved.downloadCapture;});
  browser.storage.onChanged?.addListener((changes,area)=>{
    if(area==='local' && changes.downloadCapture) activeConfig=changes.downloadCapture.newValue;
  });
  const persist=()=>browser.storage.session.set({[PENDING]:Object.fromEntries([...jobs].filter(([,job])=>!job.resolved).map(([id,job])=>[id,{
    id,name:job.name,operationId:job.operationId,state:job.state,posted:!!job.posted,...(job.taskIds ? {taskIds:job.taskIds} : {})
  }]))});
  function release(job) {
    clearTimeout(job.timer);delete job.timer;
    job.releaseFilename?.();delete job.releaseFilename;
    job.releaseNative?.();delete job.releaseNative;
  }
  async function state(job,value,{resolved=false}={}) {
    job.state=value;job.resolved=resolved;
    if (value !== 'preparing' && value !== 'sending') {delete job.url;delete job.header;}
    await persist();
    if(resolved || value==='uncertain') release(job);
    if(value==='uncertain') {
      await browser.action.setBadgeText({text:'!'});
      await browser.action.setTitle({title:'下载状态未确认，请在 kownload 检查；不会重复提交'});
    }
    const finished=[...jobs].filter(([,item])=>item.resolved);
    for(const [id] of finished.slice(0,-20)) jobs.delete(id);
  }
  async function current(job) {
    const item=(await browser.downloads.search({id:job.id}))[0];
    return {item,report:{state:item?.state || 'unknown',paused:item?.paused===true,received:item?.bytesReceived ?? 0,total:item?.totalBytes ?? -1}};
  }
  async function returnToBrowser(job) {
    job.posted=false;job.state='preparing';delete job.url;delete job.header;
    try {
      let item;
      try {({item}=await current(job));} catch {await browser.downloads.resume(job.id);}
      if(item?.state==='in_progress' && item.paused !== false) await browser.downloads.resume(job.id);
      await state(job,'browser',{resolved:true});
    } catch {release(job);await persist().catch(()=>{});await browser.action.setBadgeText({text:'!'});}
  }
  async function cancelFromApp(job) {
    job.state='cancelled';job.posted=false;delete job.url;delete job.header;
    await persist();
    try {
      const {item}=await current(job);
      // A genuine App cancellation never deletes a file the browser completed.
      if(item?.state==='in_progress') await browser.downloads.cancel(job.id);
      await state(job,'cancelled',{resolved:true});
    } catch {release(job);await persist().catch(()=>{});}
  }
  async function created(job,ids) {
    if(!Array.isArray(ids) || !ids.length || ids.some(id=>typeof id!=='string' || !id.trim())) throw new Error('invalid_receipt');
    job.taskIds=[...new Set(ids)];
    // Persist the authoritative creation before attempting browser cleanup.
    job.state='created';delete job.url;delete job.header;await persist().catch(()=>{});
    try {
      const {item,report}=await current(job);
      if(item?.state==='in_progress') await browser.downloads.cancel(job.id);
      else if(item?.state==='complete') {
        // The App task already exists; preserve both facts and the browser file.
        await appBridge.request('submission',{operationId:job.operationId,browserState:report}).catch(()=>{});
        job.browserCompleted=true;
      }
      await state(job,'created',{resolved:true});
    } catch {
      // Cancellation failure cannot turn a created task back into "unknown".
      release(job);await persist().catch(()=>{});
      await browser.action.setBadgeText({text:'!'});
    }
  }
  function schedule(job) {
    clearTimeout(job.timer);
    if(pollMs>0 && !job.resolved && ['accepted','authorizing'].includes(job.state)) job.timer=setTimeout(()=>{void poll(job);},pollMs);
  }
  async function receipt(job,data) {
    if(data?.operationId && data.operationId!==job.operationId) throw new Error('invalid_receipt');
    if(data?.state==='created') {await created(job,data.taskIds);return;}
    if(!Array.isArray(data?.taskIds) || data.taskIds.length) throw new Error('invalid_receipt');
    if(data.state==='cancelled') {await cancelFromApp(job);return;}
    if(['browser_completed','browser_retained'].includes(data.state)) {await returnToBrowser(job);return;}
    if(data.state==='unknown') {await state(job,'uncertain');return;}
    if(data.state!=='accepted') throw new Error('invalid_receipt');
    await state(job,data.commitStarted ? 'authorizing' : 'accepted');
    if(data.commitRequested) {
      if(!validId(data.confirmToken)) throw new Error('invalid_receipt');
      const {report}=await current(job);
      const decision=report.state==='complete' ? 'browser_completed' : report.state==='in_progress' && report.paused ? 'commit' : 'browser_retained';
      // Approval only releases the App's existing CreateView submission. It
      // never repeats create or sends an alternate task request.
      await state(job,'authorizing');
      const answer=await appBridge.request('submission',{operationId:job.operationId,confirmToken:data.confirmToken,decision,browserState:report});
      if(answer?.state!=='accepted') {await receipt(job,answer);return;}
    }
    schedule(job);
  }
  async function poll(job) {
    if(job.polling || job.resolved) return;
    job.polling=true;
    try {
      const {report}=await current(job);
      const data=await appBridge.request('submission',{operationId:job.operationId,browserState:report});
      await receipt(job,data);
    } catch {await state(job,'uncertain').catch(()=>{});}
    finally {job.polling=false;}
  }
  async function submit(job) {
    try {
      const ping=await appBridge.send({method:'ping'});
      if(ping?.code!==0 || ping.data!==true) {await returnToBrowser(job);return;}
    } catch {await returnToBrowser(job);return;}
    const {item}=await current(job);
    if(!item || item.state!=='in_progress' || item.paused===false) {await returnToBrowser(job);return;}
    await state(job,'sending');job.posted=true;await persist();
    let reply;
    try {
      reply=await appBridge.send({method:'create',operationId:job.operationId,meta:{silent:!job.confirm,source:'browser-capture',...await appBridge.identity()},params:{req:{url:job.url,extra:{header:job.header}},opts:{name:job.name}}});
      const answer=nativeSubmission(reply,job.operationId);
      if(answer.state==='created') await created(job,answer.taskIds);
      else {await state(job,'accepted');schedule(job);}
    } catch {
      if(rejected(reply)) await returnToBrowser(job);
      else {
        // A lost acknowledgement can be queried once, never replayed.
        await state(job,'recovering');
        await poll(job);
        if(job.state==='uncertain') release(job);
      }
    }
  }
  const restored=browser.storage.session.get(PENDING).then(saved=>{
    for(const item of Object.values(saved[PENDING] || {})) {
      if(!Number.isInteger(item.id) || !validId(item.operationId)) continue;
      jobs.set(item.id,{...item,resolved:false,posted:typeof item.posted==='boolean' ? item.posted : ['sending','accepted','authorizing','recovering','uncertain','created'].includes(item.state)});
    }
  });
  void restored.then(async()=>{
    for(const job of [...jobs.values()]) {
      try {
        if(job.state==='created') {await created(job,job.taskIds);continue;}
        if(job.state==='cancelled') {await cancelFromApp(job);continue;}
        if(!job.posted) {await returnToBrowser(job);continue;}
        job.releaseNative=appBridge.acquire();
        await appBridge.settings();
        await poll(job);
      } catch {await state(job,'uncertain').catch(()=>{});}
    }
  });
  async function capture(item,releaseFilename) {
    await restored;
    const url=item.finalUrl || item.url,observed=requests.get(url);
    if(!httpURL(url) || item.incognito || item.byExtensionId || jobs.has(item.id) || !observed || observed.method!=='GET' || observed.ambiguous || observed.expires<Date.now() || requestsBlockedUntil>Date.now()) return;
    requests.delete(url);
    const early=earlyPauses.get(item.id);earlyPauses.delete(item.id);
    try {if(early){if(!await early)return;}else await browser.downloads.pause(item.id);}catch{return;}
    const job={id:item.id,name:(item.filename || '').split(/[\\/]/).pop() || 'download',url,header:observed.header,operationId:newOperationId(),state:'preparing',posted:false,resolved:false,releaseFilename};
    jobs.set(item.id,job);
    try {
      job.releaseNative=appBridge.acquire();
      // This is an actual eligible download, not passive media discovery. An
      // enabled capture can wake the App before asking its canonical mode.
      if(activeConfig?.enabled !== false) await appBridge.wake();
      const settings=await appBridge.settings();
      activeConfig=settings.downloadCapture;
      if(!activeConfig.enabled){await returnToBrowser(job);return;}
      job.confirm=activeConfig.confirm;
      await persist();
      await submit(job);
    } catch {
      if(!job.posted) await returnToBrowser(job).catch(()=>{});
      else await state(job,'uncertain').catch(()=>{});
    } finally {
      if(job.resolved || job.state==='uncertain') release(job);
    }
  }
  function attach() {
    if(listening || !browser.downloads?.onDeterminingFilename) return;
    listening=true;
    browser.downloads.onCreated?.addListener(item=>{
      if(activeConfig?.enabled===false || !httpURL(item.finalUrl || item.url) || item.incognito || item.byExtensionId || item.state && item.state!=='in_progress') return;
      try {earlyPauses.set(item.id,Promise.resolve(browser.downloads.pause(item.id)).then(()=>true,()=>false));}catch{}
    });
    browser.downloads.onChanged?.addListener(delta=>{
      if(delta.state?.current && delta.state.current!=='in_progress') earlyPauses.delete(delta.id);
    });
    browser.downloads.onDeterminingFilename.addListener((item,suggest)=>{
      let released=false;
      const releaseFilename=()=>{if(!released){released=true;suggest();}};
      void capture(item,releaseFilename).finally(async()=>{
        const job=jobs.get(item.id),early=earlyPauses.get(item.id);earlyPauses.delete(item.id);
        if(early && await early && !job) await browser.downloads.resume(item.id).catch(()=>{});
        if(!job || job.resolved || job.state==='uncertain') releaseFilename();
      }).catch(()=>{releaseFilename();});
      return true;
    });
  }
  const permissionsReady=browser.permissions.contains({permissions:['downloads']}).then(value=>{granted=value;if(value)attach();return value;});
  browser.permissions.onAdded.addListener(value=>{if(value.permissions?.includes('downloads')){granted=true;attach();}});
  browser.permissions.onRemoved?.addListener(value=>{if(value.permissions?.includes('downloads')){granted=false;requests.clear();}});
  browser.webRequest.onBeforeSendHeaders.addListener(event=>{void permissionsReady.then(()=>{
    if(!granted || event.tabId<0 || event.incognito || !httpURL(event.url) || typeof event.requestId!=='string' || !event.requestId || requestsBlockedUntil>Date.now()) return;
    // Navigation/file requests can become downloads. Image/XHR/script traffic
    // has its own media pipeline and must not displace ordinary request records.
    if(event.type && !['main_frame','sub_frame','other'].includes(event.type)) return;
    const header={...safeHeader(event.requestHeaders),...protectedHeaders(event.requestHeaders)};
    const cookie=event.requestHeaders?.find(h=>h.name.toLowerCase()==='cookie')?.value;
    if(cookie && !/[\r\n]/.test(cookie)) header.Cookie=cookie;
    header['Accept-Encoding']='identity';
    for(const [url,request] of requests) if(request.expires<Date.now()) requests.delete(url);
    const previous=requests.get(event.url),same=previous && previous.requestId===event.requestId && previous.tabId===event.tabId && previous.frameId===event.frameId && previous.documentId===event.documentId;
    const ambiguous=previous && (previous.ambiguous || !same);
    if(!previous && requests.size>=200){requests.clear();requestsBlockedUntil=Date.now()+60000;return;}
    requests.set(event.url,{requestId:event.requestId,tabId:event.tabId,frameId:event.frameId,documentId:event.documentId,method:event.method,...(!ambiguous?{header}:{}),ambiguous:!!ambiguous,expires:Date.now()+60000});
  });},{urls:['<all_urls>']},['requestHeaders','extraHeaders']);
  return {capture,poll,jobs,ready:restored};
}
