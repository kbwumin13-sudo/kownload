import {classify,nativeData,inspectData,taskId,safeHeader,protectedHeaders,cookieScopeHosts,recoverableTasks,nativeCookies,applyCandidate,applyNavigation,nativeForwardMessage,createSessionStore,resetCandidateIndex,groupMedia,mediaRelatedUrls,mediaContextUrls} from './lib.js';
import {installNetdiskBridge} from './netdisk.js';

installNetdiskBridge(chrome);

const pendingHeaders = new Map();
const requestStarts = new Map();
const createJobs = new Map(),inspectJobs = new Map();
const inspected = new Map();
const requestVault = new Map();
const searchKey = (tabId,frameId,documentId) => `${tabId}:${frameId}:${documentId}`;
function vaultKey(tabId,frameId,url) { return `${tabId}:${frameId}:${url}`; }
function scopedHeadersFor(item) {
  const scopes = [];
  for (const url of [item.url,...(item.contextUrls || []),...(item.relatedUrls || [])].slice(0,128)) {
    const saved = requestVault.get(vaultKey(item.tabId,item.frameId,url));
    if (saved?.until > Date.now() && (!saved.documentId || saved.documentId === item.documentId)) scopes.push({origin:new URL(url).origin,url,headers:saved.header});
  }
  return [...new Map(scopes.map(x => [x.url,x])).values()];
}
const usesMedia = item => item.kind !== 'image' && !!(item.manifest || item.sourceType === 'page' || item.mediaRequired || scopedHeadersFor(item).length);
let configCache;
let queue = Promise.resolve();
const serial = (fn) => { const job = queue.then(fn,fn); queue = job.catch(() => {}); return job; };
const store = createSessionStore(chrome.storage.session,{onCommit(tabId,state) {
  badge(tabId,state);
  chrome.runtime.sendMessage({type:'CHANGED',tabId}).catch(() => {});
}});
const read = store.read;
const write = (state,tabId,immediate = false) => store.changed(tabId,immediate);
function belongsToPlayer(item,msg) {
  return !!msg.playerId && item.kind === 'video' && item.frameId === msg.frameId &&
    item.documentId === msg.documentId && item.sourcePage === msg.sourcePage &&
    (item.playerId === msg.playerId || item.manualFor === msg.playerId);
}
function badge(tabId,state) {
  const count = groupMedia(Object.values(state.items).filter(x => x.tabId === tabId)).length;
  chrome.action.setBadgeBackgroundColor({tabId,color:'#3eaa77'}).catch(() => {});
  chrome.action.setBadgeText({tabId,text:count ? String(Math.min(count,999)) : ''}).catch(() => {});
}
const siteOf = (url) => { try { return new URL(url).hostname; } catch { return ''; } };
async function disabled(tabId) {
  const tab = await chrome.tabs.get(tabId).catch(() => null);
  if (!tab || !/^https?:/.test(tab.url || '')) return true;
  const host = siteOf(tab?.url);
  return !!(host && (await chrome.storage.local.get('disabledSites')).disabledSites?.[host]);
}
function cleanUrl(url) { try { const u = new URL(url); return ['http:','https:'].includes(u.protocol) ? u.href : null; } catch { return null; } }
async function addBatch(items,immediate = false) {
  if (!items.length) return [];
  return serial(async () => {
    if (await disabled(items[0].tabId)) return [];
    const state = await read(),found = [];
    for (const item of items) {
      if (!cleanUrl(item.url)) continue;
      const candidate = applyCandidate(state,item);
      if (candidate) found.push(candidate);
    }
    if (found.length) await write(state,items[0].tabId,immediate);
    return found;
  });
}
async function add(item,immediate = false) { return (await addBatch([item],immediate))[0] || null; }
async function clear(tabId, frameId = null,removed = false) {
  return serial(async () => {
    const state = await read();
    for (const key of Object.keys(state.searches || {})) if (key.startsWith(`${tabId}:`) && (frameId === null || key.startsWith(`${tabId}:${frameId}:`))) delete state.searches[key];
    for (const [key,saved] of requestVault) if (saved.tabId === tabId && (frameId === null || saved.frameId === frameId)) requestVault.delete(key);
    for (const [key,item] of Object.entries(state.items)) if (item.tabId === tabId && (frameId === null || item.frameId === frameId)) delete state.items[key];
    for (const key of Object.keys(state.submitted)) if (key.startsWith(`${tabId}\u001f`)) delete state.submitted[key];
    if (removed) {
      for (const key of Object.keys(state.routes || {})) if (key.startsWith(`${tabId}:`)) delete state.routes[key];
      delete state.topCommit?.[tabId];
    }
    resetCandidateIndex(state);
    await write(state,tabId,true);
  });
}
function navigate(event,kind) {
  return serial(async () => {
    const state = await read();
    for (const [key,s] of Object.entries(state.searches || {})) if (s.tabId === event.tabId && (event.frameId === 0 || s.frameId === event.frameId)) delete state.searches[key];
    for (const [key,s] of requestVault) if (s.tabId === event.tabId && (event.frameId === 0 || s.frameId === event.frameId)) requestVault.delete(key);
    applyNavigation(state,{...event,kind});
    await write(state,event.tabId,true);
  });
}
chrome.webRequest.onBeforeRequest.addListener(d => {
  if (d.tabId >= 0 && !requestStarts.has(d.requestId)) requestStarts.set(d.requestId,d.timeStamp);
}, {urls:['<all_urls>']});
chrome.webRequest.onBeforeSendHeaders.addListener(d => {
  if (d.tabId >= 0) {
    if (!requestStarts.has(d.requestId)) requestStarts.set(d.requestId,d.timeStamp);
    pendingHeaders.set(d.requestId,safeHeader(d.requestHeaders));
    const header = protectedHeaders(d.requestHeaders);
    if (Object.keys(header).length && cleanUrl(d.url)) {
      requestVault.set(vaultKey(d.tabId,d.frameId,d.url),{tabId:d.tabId,frameId:d.frameId,documentId:d.documentId || '',header,until:Date.now()+5*60000});
      if (requestVault.size > 1000) requestVault.delete(requestVault.keys().next().value);
    }
  }
}, {urls:['<all_urls>']}, ['requestHeaders','extraHeaders']);
chrome.webRequest.onHeadersReceived.addListener(d => {
  if (d.tabId < 0) return;
  const mime = d.responseHeaders?.find(h => h.name?.toLowerCase() === 'content-type')?.value || '';
  const disposition = d.responseHeaders?.find(h => h.name?.toLowerCase() === 'content-disposition')?.value || '';
  const type = classify(d.url,mime,d.type,disposition);
  if (!type) return;
  // A bare TS network response is commonly an HLS segment. Explicit player
  // sources and attachment filenames still admit standalone TS through ADD_DOM.
  if (/\.ts$/i.test(new URL(d.url).pathname) && !disposition) return;
  const header = pendingHeaders.get(d.requestId) || {};
  const capturedAt = requestStarts.get(d.requestId) || d.timeStamp;
  void (async () => {
    const frame = d.frameId >= 0 ? await chrome.webNavigation.getFrame({tabId:d.tabId,frameId:d.frameId}).catch(() => null) : null;
    const tab = await chrome.tabs.get(d.tabId).catch(() => null);
    await add({tabId:d.tabId,frameId:d.frameId,documentId:d.documentId || '',url:d.url,...type,network:true,capturedAt,
      mime,title:tab?.title || '',sourcePage:frame?.url || tab?.url || d.initiator || '',header,authRequired:requestVault.has(vaultKey(d.tabId,d.frameId,d.url)),mediaRequired:requestVault.has(vaultKey(d.tabId,d.frameId,d.url))});
  })().catch(() => {});
}, {urls:['<all_urls>']}, ['responseHeaders']);
chrome.webRequest.onCompleted.addListener(d => { pendingHeaders.delete(d.requestId); requestStarts.delete(d.requestId); },{urls:['<all_urls>']});
chrome.webRequest.onErrorOccurred.addListener(d => { pendingHeaders.delete(d.requestId); requestStarts.delete(d.requestId); },{urls:['<all_urls>']});
chrome.webNavigation.onCommitted.addListener(d => { void navigate(d,'commit'); });
chrome.webNavigation.onHistoryStateUpdated.addListener(d => { void navigate(d,'spa'); });
chrome.webNavigation.onReferenceFragmentUpdated.addListener(d => { void navigate(d,'spa'); });
chrome.tabs.onRemoved.addListener(tabId => { void clear(tabId,null,true); });

function native(message,rest = false) {
  return new Promise((resolve,reject) => chrome.runtime.sendNativeMessage('com.gopeed.gopeed',message,response => {
    const err = chrome.runtime.lastError;
    if (err) reject(new Error(err.message)); else { try {
      if (!response || response.code !== 0) throw new Error(response?.msg || response?.message || 'kownload 唤起失败');
      resolve(rest ? nativeData(response) : response.data);
    } catch(e) { reject(e); } }
  }));
}
async function forward(method,path,data) {
  await native({method:'wakeup',meta:{silent:true}});
  return native(nativeForwardMessage(method,path,data),true);
}
function cookieHeader(cookies) {
  return cookies.filter(c => c.name && c.value != null).map(c => `${c.name}=${c.value}`).join('; ');
}
function safeError(error) {
  return String(error?.message || error || '未知错误').replace(/https?:\/\/[^\s"'<>]+/gi,'[资源地址]').replace(/Cookie\s*[:=]\s*[^\r\n]+/gi,'Cookie: [已隐藏]');
}
async function selectedRequest(item) {
  const stores = chrome.cookies.getAllCookieStores ? await chrome.cookies.getAllCookieStores().catch(() => []) : [];
  const storeId = stores.find(x => x.tabIds?.includes(item.tabId))?.id;
  if (chrome.cookies.getAllCookieStores && !storeId) throw new Error('无法确认当前窗口的登录状态，请回到页面重新捕获');
  let partitionKey;
  if (chrome.cookies.getPartitionKey) {
    const details = {tabId:item.tabId,frameId:item.frameId};
    if (item.documentId) details.documentId = item.documentId;
    const result = await chrome.cookies.getPartitionKey(details).catch(() => undefined);
    partitionKey = result?.partitionKey || result;
  }
  const query = {url:item.url,...(storeId ? {storeId} : {})};
  const cookies = await chrome.cookies.getAll(query);
  if (partitionKey?.topLevelSite) cookies.push(...await chrome.cookies.getAll({...query,partitionKey}).catch(() => []));
  const uniqueCookies = [...new Map(cookies.map(c => [[c.name,c.domain,c.path,c.partitionKey?.topLevelSite || ''].join('\u001f'),c])).values()];
  const header = {...item.header};
  if (!header.Referer) {
    const source = cleanUrl(item.sourcePage) || cleanUrl((await chrome.tabs.get(item.tabId).catch(() => null))?.url);
    if (source) header.Referer = source;
  }
  const value = cookieHeader(uniqueCookies);
  if (value) header.Cookie = value;
  const relatedCookies = [];
  for (const host of cookieScopeHosts(item)) {
    // The cookie jar enforces each cookie's path on actual downstream requests.
    // A root-manifest URL query alone omits /keys/ and /segments/ cookies.
    const relatedQuery = {domain:host,...(storeId ? {storeId} : {})};
    relatedCookies.push(...await chrome.cookies.getAll(relatedQuery));
    if (partitionKey?.topLevelSite) relatedCookies.push(...await chrome.cookies.getAll({...relatedQuery,partitionKey}).catch(() => []));
  }
  const jar = [...new Map([...uniqueCookies,...relatedCookies].map(c => [[c.name,c.domain,c.path,c.partitionKey?.topLevelSite || ''].join('\u001f'),c])).values()];
  const scopedHeaders = scopedHeadersFor(item);
  if (item.authRequired && !scopedHeaders.length) throw new Error('此资源的授权已失效，请返回网页重新播放后捕获');
  return {url:item.url,header,cookies:nativeCookies(jar),scopedHeaders,title:item.title || '',sourcePage:item.sourcePage || '',sourceType:item.sourceType || '',expectedMediaId:item.expectedMediaId || '',contextId:[item.tabId,item.frameId,item.documentId,item.pageEpoch || 0,item.playerId || ''].join(':'),context:{storeId,partitionKey}};
}
function mediaExtra(req,options = {}) {
  const header = Object.fromEntries(Object.entries(req.header || {}).filter(([key])=>key.toLowerCase()!=='cookie'));
  return {header,cookies:req.cookies,title:req.title,sourcePage:req.sourcePage,
    scopedHeaders:req.scopedHeaders,sourceType:req.sourceType,expectedMediaId:req.expectedMediaId,contextId:req.contextId,...options};
}
async function inspect(item) {
  const req = await selectedRequest(item);
  const contextKey = JSON.stringify([item.id,req.context,req.cookies,req.header,req.scopedHeaders]);
  const cached = inspected.get(contextKey);
  if (cached?.until > Date.now()) return cached.data;
  if (inspectJobs.has(contextKey)) return inspectJobs.get(contextKey);
  const job = (async () => {
    const data = inspectData(await forward('POST','/media/inspect',{req:{url:req.url,protocol:'media',extra:mediaExtra(req,{collectContext:true})}}));
    // The source might have navigated while extraction was in flight.
    await serial(async () => {
      const state = await read(),current = state.items[item.id];
      if (!current) throw new Error('资源已失效，请回到视频重新点击');
      current.relatedUrls = [...new Set([...(current.relatedUrls || []),...mediaRelatedUrls(data,current.url)])].slice(0,64);
      current.contextUrls = [...new Set([...(current.contextUrls || []),...mediaContextUrls(data,current.url)])].slice(0,64);
      current.mediaId = data.mediaId || '';
      current.title = data.title || current.title;
      current.metadataReady = !!data.title;
      current.thumbnail = data.thumbnail || current.thumbnail;
      current.duration = data.duration || current.duration;
      await write(state,item.tabId,true);
    });
    if (inspected.size >= 100) inspected.delete(inspected.keys().next().value);
    inspected.set(contextKey,{data,until:Date.now()+30000});
    return data;
  })().finally(() => inspectJobs.delete(contextKey));
  inspectJobs.set(contextKey,job);
  return job;
}
async function create(item,options = {}) {
  const format = options.format;
  const mediaKind = options.mediaKind === 'audio' ? 'audio' : 'video';
  if (usesMedia(item) && !format) throw new Error('请先选择媒体格式');
  const markChoice = options.subtitleLanguages?.length ? JSON.stringify([format || '',options.subtitleLanguages,!!options.writeAutomaticSubtitles,!!options.embedSubtitles]) : format || '';
  const mark = `${item.tabId}\u001f${item.id}\u001f${markChoice}${usesMedia(item) && mediaKind === 'audio' ? '\u001faudio' : ''}${options.imageUrl ? '\u001f'+options.imageUrl : ''}`;
  if (createJobs.has(mark)) return createJobs.get(mark);
  const job = (async () => {
    const state = await read();
    if (state.submitted[mark]) return {taskId:state.submitted[mark],duplicate:true};
    const imageUrl = options.imageUrl;
    if (imageUrl && (item.kind !== 'image' || ![item.url,...(item.variants || [])].includes(imageUrl))) throw new Error('图片地址已失效，请重新捕获');
    const req = await selectedRequest(imageUrl ? {...item,url:imageUrl} : item);
    if (!(await read()).items[item.id]) throw new Error('资源已失效，请重新捕获');
    const media = usesMedia(item);
    const extra = media ? mediaExtra(req,{format,mediaKind,container:options.container || '',subtitleLanguages:options.subtitleLanguages || [],writeAutomaticSubtitles:!!options.writeAutomaticSubtitles,embedSubtitles:!!options.embedSubtitles}) : {header:req.header,sourcePage:req.sourcePage};
    const payload = {req:{url:req.url,...(media ? {protocol:'media'} : {}),extra},opts:{name:options.name || '',path:options.path || ''}};
    const id = taskId(await forward('POST','/tasks',payload));
    await serial(async () => { const latest = await read(); if (latest.items[item.id]) { latest.submitted[mark] = id; await write(latest,item.tabId,true); } });
    return {taskId:id,duplicate:false};
  })().finally(() => createJobs.delete(mark));
  createJobs.set(mark,job);
  return job;
}
async function recoveryTasks(item) {
  const tasks = await forward('GET','/tasks?status=pause&status=error');
  if (!Array.isArray(tasks)) throw new Error('kownload 返回的任务列表无效');
  return recoverableTasks(tasks,item.sourcePage,usesMedia(item) ? 'media' : 'http');
}
async function update(item,options) {
  if (!options.taskId || !/^[\w-]+$/.test(options.taskId)) throw new Error('请先选择可更新任务');
  if (!(await recoveryTasks(item)).some(t => t.id === options.taskId)) throw new Error('任务已不在本页暂停或失败列表中，请重新查找');
  if (usesMedia(item) && !options.format) throw new Error('请先选择媒体格式');
  const req = await selectedRequest(item);
  const extra = {header:req.header,sourcePage:req.sourcePage};
  if (usesMedia(item)) Object.assign(extra,mediaExtra(req,{format:options.format,mediaKind:options.mediaKind === 'audio' ? 'audio' : 'video',container:options.container || '',subtitleLanguages:options.subtitleLanguages || [],writeAutomaticSubtitles:!!options.writeAutomaticSubtitles,embedSubtitles:!!options.embedSubtitles}));
  const taskReq = {url:req.url,...(usesMedia(item) ? {protocol:'media'} : {}),extra};
  await forward('PATCH',`/tasks/${options.taskId}`,{req:taskReq});
  await forward('PUT',`/tasks/${options.taskId}/continue`);
  return {taskId:options.taskId};
}
chrome.runtime.onMessage.addListener((msg,sender,sendResponse) => {
  if (typeof msg?.type==='string' && msg.type.startsWith('NETDISK_')) return false;
  (async () => {
    if (msg.type === 'ADD_DOM') {
      const tabId = sender.tab?.id;
      if (!Number.isInteger(tabId)) throw new Error('缺少网页标签');
      let search;
      if (msg.searchId) {
        const state = await read();
        search = state.searches?.[searchKey(tabId,sender.frameId,sender.documentId)];
        if (!search || search.searchId !== msg.searchId || search.until <= Date.now() || search.sourcePage !== (msg.sourcePage || sender.url)) return [];
      }
      const items = [];
      for (const raw of msg.items || []) {
        const url = cleanUrl(raw.url);
        const type = url && (raw.manifest || raw.protocolHint === 'manifest' ? {kind:'video',manifest:true} : classify(url,raw.mime || '',raw.kind || ''));
        if (!type) continue;
        const proved = search && raw.playerId === search.playerId;
        items.push({tabId,frameId:sender.frameId,documentId:sender.documentId || '',url,...type,capturedAt:msg.timeStamp || Date.now(),
          width:raw.width || undefined,height:raw.height || undefined,thumbnail:raw.thumbnail || '',playerId:raw.playerId || '',imageId:raw.imageId || '',variants:raw.variants || [],
          relatedUrls:(raw.relatedUrls || []).map(cleanUrl).filter(Boolean).slice(0,16),
          contextUrls:(raw.contextUrls || []).map(cleanUrl).filter(Boolean).slice(0,64),mediaRequired:!!raw.mediaRequired || raw.protocolHint === 'manifest',
          ...(search ? {searchId:search.searchId,manualFor:proved ? '' : search.playerId,association:proved ? 'source' : 'search'} : {}),
          title:raw.title || sender.tab.title || '',sourcePage:msg.sourcePage || sender.url || '',header:{}});
      }
      return (await addBatch(items,true)).map(x => x.id);
    }
    if (msg.type === 'LIST') {
      const state = await read();
      const list = Object.values(state.items).filter(x => x.tabId === msg.tabId && (msg.target === undefined || x.id === msg.target)).map(x => ({...x,mediaRequired:usesMedia(x)}));
      if (msg.identify) return groupMedia(list.filter(x => belongsToPlayer(x,msg))).slice(0,10);
      return msg.target === undefined ? groupMedia(list) : list;
    }
    if (msg.type === 'SITE') {
      const sites = (await chrome.storage.local.get('disabledSites')).disabledSites || {};
      if (msg.set !== undefined) {
        if (msg.set) sites[msg.host] = true; else delete sites[msg.host];
        await chrome.storage.local.set({disabledSites:sites});
        if (msg.set) await clear(msg.tabId);
        const frames = await chrome.webNavigation.getAllFrames({tabId:msg.tabId}).catch(() => []);
        for (const frame of frames || []) chrome.tabs.sendMessage(msg.tabId,{type:msg.set ? 'SITE_OFF' : 'RESCAN'},{frameId:frame.frameId}).catch(() => {});
      }
      return !!sites[msg.host];
    }
    if (msg.type === 'ENABLED') return !(await disabled(sender.tab?.id));
    if (msg.type === 'PANEL') {
      const tabId = sender.tab?.id;
      const url = cleanUrl(msg.url);
      if (!Number.isInteger(tabId)) throw new Error('缺少网页标签');
      if (!url) {
        if (!msg.playerId || !sender.documentId) throw new Error('未识别到这个视频的下载地址，请播放后重试');
        if (msg.suggestedPage !== undefined) {
          const source = cleanUrl(msg.sourcePage), senderPage = cleanUrl(sender.url), suggested = cleanUrl(msg.suggestedPage);
          const frame = await chrome.webNavigation.getFrame({tabId,frameId:sender.frameId ?? 0}).catch(() => null);
          if (!source || source !== senderPage || !frame || frame.documentId !== sender.documentId || frame.url !== source ||
              !suggested || msg.sourceType || typeof msg.expectedMediaId !== 'string' || !/^\d+$/.test(msg.expectedMediaId)) throw new Error('当前帖子来源已失效');
          const page = new URL(source), candidatePage = new URL(suggested);
          const post = page.pathname.match(/^\/[A-Za-z0-9_]+\/status\/(\d+)(?:\/video\/([1-9]\d*))?\/?$/);
          if (!['x.com','www.x.com','twitter.com','www.twitter.com','mobile.twitter.com'].includes(page.hostname) ||
              !['http:','https:'].includes(page.protocol) || !post || post[1] !== msg.expectedMediaId ||
              candidatePage.protocol !== 'https:' || candidatePage.hostname !== 'x.com' || candidatePage.username || candidatePage.password ||
              candidatePage.search || candidatePage.hash || candidatePage.pathname !== page.pathname ||
              suggested !== `https://x.com${page.pathname}`) throw new Error('当前帖子来源已失效');
          const candidate = await add({tabId,frameId:sender.frameId ?? 0,documentId:sender.documentId,url:suggested,
            kind:'video',manifest:true,capturedAt:Date.now(),sourcePage:source,title:'当前帖子视频（请确认）',
            playerId:'',manualFor:msg.playerId,sourceType:'page',expectedMediaId:post[1],header:{}},true);
          if (!candidate) throw new Error('资源已失效，请回到视频重新点击');
        }
        const params = new URLSearchParams({tab:String(tabId),mode:'identify',frame:String(sender.frameId ?? 0),doc:sender.documentId,source:msg.sourcePage || sender.url || '',player:msg.playerId});
        await chrome.tabs.create({url:chrome.runtime.getURL(`popup.html?${params}`)});
        return true;
      }
      const state = await read();
      const existing = Object.values(state.items).find(x => x.tabId === tabId && x.frameId === (sender.frameId ?? 0) &&
        x.documentId === (sender.documentId || '') && x.sourcePage === (msg.sourcePage || sender.url || '') && x.url === url);
      const page = msg.sourceType === 'page';
      const parsed = new URL(url);
      const allowedPage = page && (['youtube.com','www.youtube.com','m.youtube.com'].includes(parsed.hostname) && parsed.pathname === '/watch' && !parsed.searchParams.has('list') && parsed.searchParams.get('v') === msg.expectedMediaId || /^(?:www\.)?bilibili\.com$/.test(parsed.hostname) && /^\/video\/(BV[\w]+|av[0-9]+)\/?$/.test(parsed.pathname));
      const xPage = page && parsed.hostname === 'x.com' && /^\/[^/]+\/status\/\d+(?:\/video\/\d+)?\/?$/.test(parsed.pathname) && parsed.pathname.match(/\/status\/(\d+)/)?.[1] === msg.expectedMediaId && ['x.com','www.x.com','twitter.com','www.twitter.com','mobile.twitter.com'].includes(new URL(msg.sourcePage || sender.url).hostname);
      if (page && (!(allowedPage || xPage) || !xPage && url !== (msg.sourcePage || sender.url))) throw new Error('尚未确认这个视频的独立页面');
      const type = page ? {kind:'video',manifest:true} : classify(url,existing?.mime || '','video');
      if (!type) throw new Error('这个视频的资源暂不支持下载');
      // Capture at the click boundary so a scan still in flight cannot select another player.
      const candidate = await add({tabId,frameId:sender.frameId ?? 0,documentId:sender.documentId || '',url,...type,
        capturedAt:msg.timeStamp || Date.now(),sourcePage:msg.sourcePage || sender.url || '',title:sender.tab.title || '',playerId:msg.playerId || '',sourceType:page ? 'page' : '',expectedMediaId:msg.expectedMediaId || '',header:{}},true);
      if (!candidate) throw new Error('资源已失效，请回到视频重新点击');
      const params = new URLSearchParams({tab:String(tabId),mode:'single',target:candidate.id});
      await chrome.tabs.create({url:chrome.runtime.getURL(`popup.html?${params}`)});
      return true;
    }
    if (msg.type === 'DEEP_SEARCH') {
      if (!msg.identify || !Number.isInteger(msg.tabId) || !msg.documentId || !msg.playerId) throw new Error('请从未识别的视频入口开启增强搜索');
      if (await disabled(msg.tabId)) throw new Error('本站嗅探已关闭');
      const frame = await chrome.webNavigation.getFrame({tabId:msg.tabId,frameId:msg.frameId});
      if (!frame || frame.documentId !== msg.documentId || frame.url !== msg.sourcePage) throw new Error('页面已切换，请重新点击视频入口');
      const searchId = globalThis.crypto?.randomUUID?.() || `search-${Date.now()}-${Math.random().toString(36).slice(2)}`;
      const search = {searchId,tabId:msg.tabId,frameId:msg.frameId,documentId:msg.documentId,sourcePage:msg.sourcePage,playerId:msg.playerId,until:Date.now()+60000};
      await serial(async () => {
        const state = await read(); state.searches ||= {};
        state.searches[searchKey(msg.tabId,msg.frameId,msg.documentId)] = search;
        for (const [id,item] of Object.entries(state.items)) if (belongsToPlayer(item,msg) && item.association === 'search') delete state.items[id];
        resetCandidateIndex(state); await write(state,msg.tabId,true);
      });
      const started = await chrome.tabs.sendMessage(msg.tabId,{type:'SEARCH_START',playerId:msg.playerId,searchId},{frameId:msg.frameId});
      if (!started?.started) throw new Error('页面脚本已更新，请刷新原视频页面后重试');
      await chrome.scripting.executeScript({target:{tabId:msg.tabId,documentIds:[msg.documentId]},func:searchId => { window.__gopeedMediaSearchId = searchId; },args:[searchId],world:'MAIN'});
      await chrome.scripting.executeScript({target:{tabId:msg.tabId,documentIds:[msg.documentId]},files:['deep-search.js'],world:'MAIN'});
      return {searchId,until:search.until};
    }
    if (msg.type === 'CONFIG') { if (configCache?.until > Date.now()) return configCache.data; const data = await forward('GET','/config'); configCache = {data,until:Date.now()+10000}; return data; }
    if (msg.type === 'INSPECT' || msg.type === 'CREATE' || msg.type === 'UPDATE' || msg.type === 'RECOVERABLE') {
      const state = await read();
      const item = state.items[msg.id];
      if (!item || item.tabId !== msg.tabId || (msg.target !== undefined && msg.target !== item.id) || msg.identify && !belongsToPlayer(item,msg)) throw new Error('资源已失效，请刷新列表');
      if (msg.type === 'INSPECT') return inspect(item);
      if (msg.type === 'RECOVERABLE') return (await recoveryTasks(item)).map(t => ({id:t.id,name:t.meta?.opts?.name || t.name || '未命名任务',status:t.status}));
      return msg.type === 'CREATE' ? create(item,msg.options) : update(item,msg.options);
    }
    throw new Error('未知请求');
  })().then(data => sendResponse({ok:true,data}),error => sendResponse({ok:false,error:safeError(error)}));
  return true;
});
