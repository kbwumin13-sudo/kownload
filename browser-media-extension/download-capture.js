// Independently implemented against Chrome downloads and our Native contract.
import {newOperationId,safeHeader,protectedHeaders,createAppBridge} from './lib.js';
import {validBridgeBinding} from './native-identity.js';

const PENDING='downloadHandoffPendingV1';
const httpURL=value=>{try{return /^https?:$/.test(new URL(value).protocol);}catch{return false;}};
const validId=value=>typeof value==='string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value);
const validTaskId=value=>typeof value==='string' && /^[A-Za-z0-9_-]{1,128}$/.test(value);
const sha256=async value=>[...new Uint8Array(await crypto.subtle.digest('SHA-256',new TextEncoder().encode(value)))].map(n=>n.toString(16).padStart(2,'0')).join('');
const sameBinding=(a,b)=>a && b && a.id===b.id && a.startTime===b.startTime && a.urlHash===b.urlHash;
const validBinding=value=>Number.isInteger(value?.id) && value.id>=0 && Number.isFinite(Date.parse(value.startTime)) && /^[a-f0-9]{64}$/.test(value.urlHash);

export function installDownloadCapture(browser,send,{appBridge=createAppBridge(browser,send),pollMs=1000}={}) {
  if (!browser.permissions) return;
  const jobs=new Map(),requests=new Map(),earlyPauses=new Map();
  let listening=false,granted=false,activeConfig,requestsBlockedUntil=0,writing=Promise.resolve();
  browser.storage.local.get('downloadCapture').then(saved=>{activeConfig=saved.downloadCapture;});
  browser.storage.onChanged?.addListener((changes,area)=>{
    if(area==='local' && changes.downloadCapture) activeConfig=changes.downloadCapture.newValue;
  });
  function persist() {
    // Protocol metadata survives a browser/worker restart; URLs and credentials
    // remain in this one live invocation and never enter the recovery journal.
    const records=Object.fromEntries([...jobs].filter(([,job])=>!job.resolved).map(([id,job])=>[id,{
      version:1,id,name:job.name,operationId:job.operationId,state:job.state,stage:job.stage,
      posted:!!job.posted,download:job.download,clientId:job.clientId,browser:job.browser,
      browserRetained:!!job.browserRetained,cancelConfirmed:!!job.cancelConfirmed,
      ...(job.bridgeBinding ? {bridgeBinding:job.bridgeBinding} : {}),
      ...(job.taskId ? {taskId:job.taskId} : {})
    }]));
    writing=writing.catch(()=>{}).then(()=>browser.storage.local.set({[PENDING]:records}));
    return writing;
  }
  function release(job) {
    clearTimeout(job.timer);delete job.timer;
    job.releaseFilename?.();delete job.releaseFilename;
    job.releaseNative?.();delete job.releaseNative;
  }
  async function state(job,value,{resolved=false}={}) {
    job.state=value;job.resolved=resolved;
    if(value!=='preparing' && value!=='sending'){delete job.url;delete job.header;}
    await persist();
    if(resolved || value==='uncertain') release(job);
    if(value==='uncertain') {
      await browser.action.setBadgeText({text:'!'});
      await browser.action.setTitle({title:'浏览器交接待检查；不会重新创建或自动启动待交接任务'});
    }
    const finished=[...jobs].filter(([,item])=>item.resolved);
    for(const [id] of finished.slice(0,-20))jobs.delete(id);
  }
  async function current(job) {
    const item=(await browser.downloads.search({id:job.id}))[0];
    const binding=item && {id:item.id,startTime:item.startTime,urlHash:await sha256(item.finalUrl || item.url || '')};
    return {item,matches:sameBinding(binding,job.download),report:{state:item?.state || 'unknown',error:item?.error || '',paused:item?.paused===true}};
  }
  async function returnToBrowser(job,{pending=false}={}) {
    // Durable retained flag prevents a late prepared reply from taking ownership.
    job.browserRetained=true;
    try {await persist();} catch(error) {if(job.posted)throw error;}
    try {
      let snapshot;
      try {snapshot=await current(job);} catch(error) {
        if(job.posted)throw error;
        await browser.downloads.resume(job.id);
      }
      if(snapshot?.matches && snapshot.item.state==='in_progress' && snapshot.item.paused!==false)await browser.downloads.resume(job.id);
      await state(job,pending ? 'uncertain' : 'browser',{resolved:!pending});
    } finally {release(job);}
  }
  async function abort(job,reason) {
    return appBridge.request('handoffAbort',{operationId:job.operationId,...(job.taskId?{taskId:job.taskId}:{}),reason},job.bridgeBinding);
  }
  async function retain(job,reason='browser_retained') {
    job.browserRetained=true;await persist();
    let acknowledged=false;
    try {const data=await abort(job,reason);acknowledged=data?.version===1 && ['aborted','unknown'].includes(data.state);} catch {}
    await returnToBrowser(job,{pending:!acknowledged});
  }
  async function uncertain(job) {await state(job,'uncertain').catch(()=>{release(job);});}
  function schedule(job) {
    clearTimeout(job.timer);
    if(pollMs>0 && !job.resolved && job.state==='accepted')job.timer=setTimeout(()=>{void poll(job);},pollMs);
  }
  function validate(job,data) {
    if(data?.version!==1 || data.operationId!==job.operationId || !Array.isArray(data.taskIds))throw Error('invalid_receipt');
    if(data.state==='aborted' && !data.taskId && data.taskIds.length===0 && ['user_cancelled','browser_completed','browser_retained'].includes(data.reason) && !job.taskId)return;
    if(['prepared','committed','aborted'].includes(data.state)) {
      if(!validTaskId(data.taskId) || data.taskIds.length!==1 || data.taskIds[0]!==data.taskId || !sameBinding(data.download,job.download) || job.taskId && job.taskId!==data.taskId)throw Error('invalid_receipt');
      job.taskId=data.taskId;
    } else if(data.taskIds.length)throw Error('invalid_receipt');
  }
  async function commit(job) {
    if(!job.cancelConfirmed || !job.taskId)throw Error('unconfirmed_cancellation');
    job.stage='commit';await state(job,'committing');
    const data=await appBridge.request('handoffCommit',{operationId:job.operationId,taskId:job.taskId,download:job.download,cancelConfirmed:true,browserState:{state:'interrupted',error:'USER_CANCELED'}},job.bridgeBinding);
    validate(job,data);
    if(data.state!=='committed')throw Error('commit_unconfirmed');
    await state(job,'committed',{resolved:true});
  }
  async function prepared(job) {
    await state(job,'prepared');
    if(job.browserRetained){await retain(job);return;}
    if(job.cancelConfirmed){await commit(job);return;}
    // A worker crash between cancel and its durable confirmation must not infer
    // success from the later state or repeat an unjournaled ownership transfer.
    if(job.stage==='cancel_intent'){await uncertain(job);return;}
    const before=await current(job);
    if(!before.matches){await retain(job);return;}
    if(before.report.state==='complete'){await retain(job,'browser_completed');return;}
    if(before.report.state!=='in_progress' || !before.report.paused){await retain(job);return;}
    job.stage='cancel_intent';await state(job,'cancelling');
    try {await browser.downloads.cancel(job.id);} catch {}
    // Chrome cancel() completion is not proof of ownership: inspect a fresh item.
    const after=await current(job);
    if(after.matches && after.report.state==='complete'){await retain(job,'browser_completed');return;}
    if(after.matches && after.report.state==='in_progress'){await retain(job);return;}
    if(!after.matches || after.report.state!=='interrupted' || after.report.error!=='USER_CANCELED'){await uncertain(job);return;}
    job.cancelConfirmed=true;job.stage='cancel_confirmed';await persist();
    await commit(job);
  }
  async function receipt(job,data) {
    validate(job,data);
    if(data.state==='committed'){await state(job,'committed',{resolved:true});return;}
    if(data.state==='prepared'){await prepared(job);return;}
    if(data.state==='aborted') {
      if(data.reason==='user_cancelled') {
        const {item,matches}=await current(job);
        if(matches && item.state==='in_progress'){
          job.stage='cancel_intent';await persist();
          await browser.downloads.cancel(job.id);
        }
        await state(job,'aborted',{resolved:true});
      } else await returnToBrowser(job);
      return;
    }
    if(data.state==='accepted') {
      if(job.browserRetained){await retain(job);return;}
      const {report,matches}=await current(job);
      if(matches && report.state==='complete'){await retain(job,'browser_completed');return;}
      await state(job,'accepted');schedule(job);return;
    }
    if(data.state!=='unknown')throw Error('invalid_receipt');
    if(job.cancelConfirmed || job.stage==='cancel_intent' || job.stage==='commit')await uncertain(job);
    else await returnToBrowser(job,{pending:true});
  }
  async function poll(job) {
    if(job.polling || job.resolved)return;
    if(!validBridgeBinding(job.bridgeBinding)){await uncertain(job);return;}
    job.polling=true;
    try {
      const data=await appBridge.request('handoffQuery',{operationId:job.operationId},job.bridgeBinding);
      await receipt(job,data);
    } catch {
      if(job.cancelConfirmed || job.stage==='cancel_intent' || job.stage==='commit')await uncertain(job);
      else await returnToBrowser(job,{pending:true}).catch(()=>uncertain(job));
    } finally {job.polling=false;}
  }
  async function submit(job) {
    const capability=await appBridge.request('handoffCapabilities',{},job.bridgeBinding);
    if(capability?.version!==1){await returnToBrowser(job);return;}
    const {item,matches}=await current(job);
    if(!matches || item.state!=='in_progress' || !item.paused){await returnToBrowser(job);return;}
    job.stage='prepare';job.posted=true;await state(job,'sending');
    try {
      const data=await appBridge.request('handoffPrepare',{operationId:job.operationId,download:job.download,req:{url:job.url,extra:{header:job.header}},opts:{name:job.name}},job.bridgeBinding);
      await receipt(job,data);
    } catch {await poll(job);}
  }
  const restored=browser.storage.local.get(PENDING).then(saved=>{
    for(const item of Object.values(saved[PENDING] || {})) {
      if(item.version!==1 || !validBinding(item.download) || item.id!==item.download.id || !validId(item.operationId) || !validId(item.clientId) || !['chrome','edge'].includes(item.browser))continue;
      jobs.set(item.id,{...item,resolved:false});
    }
  });
  void restored.then(async()=>{
    const identity=await appBridge.identity();
    for(const job of [...jobs.values()]) {
      if(job.clientId!==identity.clientId || job.browser!==identity.browser)continue;
      try {
        if(!job.posted){await returnToBrowser(job);continue;}
        if(!validBridgeBinding(job.bridgeBinding)){await uncertain(job);continue;}
        await appBridge.binding(job.bridgeBinding);
        job.releaseNative=appBridge.acquire();
        await poll(job);
      } catch {await uncertain(job);}
    }
    // Core can retain a prepared task even if the helper journal was removed.
    // Without our durable cancel proof it remains held; never guess or recreate.
    if([...jobs.values()].some(job=>job.posted && (!validBridgeBinding(job.bridgeBinding) || job.state==='uncertain')))return;
    try {
      const data=await appBridge.request('handoffQuery',{pending:true});
      if(data?.version!==1 || !Array.isArray(data.items))return;
      for(const record of data.items) {
        if(record.state!=='prepared' || !validId(record.operationId) || !validBinding(record.download) || !validTaskId(record.taskId) || jobs.has(record.download.id))continue;
        const job={id:record.download.id,download:record.download,operationId:record.operationId,taskId:record.taskId,...identity,bridgeBinding:await appBridge.binding(),posted:true,browserRetained:true,stage:'prepare',state:'prepared',resolved:false};
        jobs.set(job.id,job);await persist();await retain(job);
      }
    } catch {}
  }).catch(()=>{});
  async function capture(item,releaseFilename) {
    await restored;
    const url=item.finalUrl || item.url,observed=requests.get(url);
    if(!httpURL(url) || item.incognito || item.byExtensionId || jobs.has(item.id) || !observed || observed.method!=='GET' || observed.ambiguous || observed.expires<Date.now() || requestsBlockedUntil>Date.now())return;
    requests.delete(url);
    const early=earlyPauses.get(item.id);earlyPauses.delete(item.id);
    try {if(early){if(!await early)return;}else await browser.downloads.pause(item.id);}catch{return;}
    const job={id:item.id,name:(item.filename || '').split(/[\\/]/).pop() || 'download',url,header:observed.header,operationId:newOperationId(),state:'preparing',stage:'prepare',posted:false,resolved:false,releaseFilename};
    jobs.set(item.id,job);
    try {
      job.download={id:item.id,startTime:item.startTime,urlHash:await sha256(url)};
      Object.assign(job,await appBridge.identity());
      if(!validBinding(job.download))throw Error('download_identity_unavailable');
      job.bridgeBinding=Object.freeze(await appBridge.binding());
      await persist();
      job.releaseNative=appBridge.acquire();
      if(activeConfig?.enabled!==false)await appBridge.wake(job.bridgeBinding);
      const settings=await appBridge.settings(job.bridgeBinding);activeConfig=settings.downloadCapture;
      if(!activeConfig.enabled){await returnToBrowser(job);return;}
      await persist();await submit(job);
    } catch {
      if(!job.posted)await returnToBrowser(job).catch(()=>{release(job);});
      else await uncertain(job);
    } finally {if(job.resolved || job.state==='uncertain')release(job);}
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
