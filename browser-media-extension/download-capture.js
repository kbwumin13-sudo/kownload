// Independently implemented against Chrome downloads and our Native contract.
import {newOperationId,nativeSubmission,safeHeader,protectedHeaders} from './lib.js';

const SETTING = 'downloadCapture';
const PENDING = 'downloadCapturePending';
const httpURL = value => { try { return /^https?:$/.test(new URL(value).protocol); } catch { return false; } };

export function installDownloadCapture(browser,send) {
  if (!browser.permissions) return;
  const jobs = new Map(),requests = new Map();
  const restored = browser.storage.session.get(PENDING).then(saved => {
    for (const job of Object.values(saved[PENDING] || {})) {
      // Credentials deliberately do not survive a worker restart. Requests
      // known to be unsubmitted can still resume/cancel in their browser.
      jobs.set(job.id,{...job,state:['waiting','expired','deciding'].includes(job.state) ? 'expired' : 'uncertain'});
    }
  });
  let configReady = browser.storage.local.get(SETTING).then(value => value[SETTING]);
  browser.storage.onChanged?.addListener((changes,area) => {
    if (area === 'local' && changes[SETTING]) {
      configReady = Promise.resolve(changes[SETTING].newValue);
      if (!changes[SETTING].newValue?.enabled) requests.clear();
    }
  });
  let listening = false;
  const persist = async () => {
    const pending = Object.fromEntries([...jobs].filter(([,j]) => ['waiting','expired','deciding','sending','uncertain','accepted'].includes(j.state)).map(([id,j]) => [id,{id,name:j.name,state:j.state,operationId:j.operationId,...(Number.isInteger(j.tabId) ? {tabId:j.tabId} : {})}]));
    await browser.storage.session.set({[PENDING]:pending});
  };
  const changed = async job => {
    await persist();
    if (['uncertain','accepted'].includes(job.state)) {
      await browser.action.setBadgeText({text:'!'});
      await browser.action.setTitle({title:'下载器尚未确认接收，请打开下载接管页面查看'});
    }
  };
  const terminal = async (job,state) => {
    job.state=state;delete job.header;delete job.url;
    await changed(job);
  };
  async function finish(job,action) {
    if (!['waiting','expired'].includes(job.state) || !['submit','cancel','resume'].includes(action) || job.state === 'expired' && action === 'submit') return;
    job.state = 'deciding';
    if (action === 'cancel') {
      await browser.downloads.cancel(job.id);
      await terminal(job,'cancelled');
      return;
    }
    if (action === 'resume') {
      await browser.downloads.resume(job.id);
      await terminal(job,'browser');
      return;
    }
    // A failed preflight cannot have submitted a task. Keep the browser's
    // original request (including POST data/cookies) available for fallback.
    let current;
    try {
      const ping = await send({method:'ping'});
      if (ping?.code !== 0 || ping.data !== true) throw new Error('unavailable');
      current = (await browser.downloads.search({id:job.id}))[0];
    } catch {
      await browser.downloads.resume(job.id);
      await terminal(job,'browser');
      return;
    }
    if (!current || current.state !== 'in_progress') { await terminal(job,'cancelled');return; }
    job.state = 'sending';
    await changed(job);
    let reply;
    try {
      job.posted = true;
      reply = await send({method:'forward',operationId:job.operationId,params:{method:'POST',path:'/api/v1/tasks',data:{req:{url:job.url,extra:{header:job.header}},opts:{name:job.name}}}});
      const receipt = nativeSubmission(reply,job.operationId);
      if (receipt.state === 'accepted') { await terminal(job,'accepted');return; }
      job.taskIds = receipt.taskIds;
      // Browser cancellation happens only after an authoritative created ACK.
      await browser.downloads.cancel(job.id);
      await terminal(job,'created');
    } catch {
      // These host outcomes reject before a task can be submitted, or are
      // explicit business rejections. A lost/malformed reply is not a failure
      // receipt: resuming it blindly could download the same file twice.
      const rejected = reply?.code !== 0 && (
        ['invalid_request','unknown_method','wakeup_failed','ipc_unavailable'].includes(reply?.error) ||
        reply?.error === 'business_error' && Number.isInteger(reply.businessCode) && reply.businessCode !== 0
      );
      if (rejected) {
        await browser.downloads.resume(job.id);
        await terminal(job,'browser');
      } else await terminal(job,'uncertain');
    }
  }
  async function capture(item) {
    await restored;
    const config = await configReady;
    const url = item.finalUrl || item.url;
    if (!config?.enabled || !httpURL(url) || item.incognito || item.byExtensionId || jobs.has(item.id)) return;
    const observed = requests.get(url);
    if (observed && observed.method !== 'GET') return;
    // Requests without an observed GET may contain POST bodies that cannot be
    // reconstructed by a download manager. Leave them in the browser.
    if (!observed || observed.expires < Date.now()) return;
    requests.delete(url);
    try { await browser.downloads.pause(item.id); } catch { return; }
    const job = {id:item.id,name:(item.filename || '').split(/[\\/]/).pop() || 'download',url,header:observed.header,state:'waiting',operationId:newOperationId()};
    try {
      const current = (await browser.downloads.search({id:item.id}))[0];
      if (!current || current.state !== 'in_progress') return;
      jobs.set(item.id,job);
      await changed(job);
      // Confirmation belongs to this browser request. The app receives only a
      // direct task submission, never a confirmation ACK mistaken for creation.
      if (config.confirm !== false) {
        const tab = await browser.tabs.create({url:browser.runtime.getURL(`download-confirm.html?id=${item.id}`)});
        job.tabId = tab.id;
        await changed(job);
      } else await finish(job,'submit');
    } catch {
      if (!job.posted) await browser.downloads.resume(item.id).catch(() => {});
      jobs.delete(item.id);
    }
    // Retain active/uncertain operations, with only a short terminal history.
    const completed = [...jobs].filter(([,j]) => ['created','browser','cancelled'].includes(j.state));
    for (const [id] of completed.slice(0,-20)) jobs.delete(id);
  }
  function attach() {
    if (listening || !browser.downloads?.onDeterminingFilename) return;
    listening = true;
    browser.downloads.onDeterminingFilename.addListener((item,suggest) => {
      suggest();
      void capture(item).catch(() => {});
    });
  }
  browser.webRequest.onBeforeSendHeaders.addListener(event => { void configReady.then(config => {
    if (!config?.enabled) return;
    if (event.tabId < 0 || !httpURL(event.url)) return;
    const header = {...safeHeader(event.requestHeaders),...protectedHeaders(event.requestHeaders)};
    const cookie = event.requestHeaders?.find(h => h.name.toLowerCase() === 'cookie')?.value;
    if (cookie && !/[\r\n]/.test(cookie)) header.Cookie = cookie;
    header['Accept-Encoding'] = 'identity';
    for (const [url,request] of requests) if (request.expires < Date.now()) requests.delete(url);
    requests.set(event.url,{method:event.method,header,expires:Date.now()+60000});
    if (requests.size > 200) requests.delete(requests.keys().next().value);
  }); },{urls:['<all_urls>']},['requestHeaders','extraHeaders']);
  browser.permissions.contains({permissions:['downloads']}).then(granted => { if (granted) attach(); });
  browser.permissions.onAdded.addListener(permissions => { if (permissions.permissions?.includes('downloads')) attach(); });
  async function leaveConfirmation(id,url) {
    await restored;
    for (const job of jobs.values()) {
      if (job.tabId === id && ['waiting','expired'].includes(job.state) && url !== browser.runtime.getURL(`download-confirm.html?id=${job.id}`)) await finish(job,'resume');
    }
  }
  browser.tabs.onRemoved.addListener(id => { void leaveConfirmation(id).catch(() => {}); });
  browser.webNavigation?.onCommitted?.addListener(event => {
    if (event.frameId === 0) void leaveConfirmation(event.tabId,event.url).catch(() => {});
  });
  browser.runtime.onMessage.addListener((message,sender,respond) => {
    if (message?.type !== 'DOWNLOAD_CAPTURE' || !sender.url?.startsWith(browser.runtime.getURL(''))) return;
    void (async () => {
      await restored;
      const job = jobs.get(Number(message.id));
      if (message.action !== 'status' && job) await finish(job,message.action);
      respond({ok:true,job:job ? {id:job.id,name:job.name,state:job.state,taskIds:job.taskIds} : null});
    })().catch(() => respond({ok:false}));
    return true;
  });
  return {capture,finish,jobs};
}
