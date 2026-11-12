import {formatOptions,chosenMediaFormat,hasCodec,newOperationId} from './lib.js';

const FAILURE = '资源操作未完成，请回到原页面重新捕获。';
const TYPES = new Set(['LIST','INSPECT','CREATE','RECOVERABLE','UPDATE','DEEP_SEARCH','CLOSE']);
const http = value => { try { return /^https?:$/.test(new URL(value).protocol); } catch { return false; } };
const fileTitle = item => {
  if (item.metadataReady && item.title) return item.title;
  try { return decodeURIComponent(new URL(item.url).pathname.split('/').filter(Boolean).pop() || new URL(item.url).hostname); }
  catch { return item.title || '网页资源'; }
};

// Only this explicit App viewer holds a lease. Browser bindings and all actual
// inspect/create/update behavior remain in the supplied original handler.
export function createAppMediaBridge(browser,appBridge,{call,preview,pollMs=500}) {
  const sessions = new Map();
  async function frame(scope) {
    const current = await browser.webNavigation.getFrame({tabId:scope.tabId,frameId:scope.frameId});
    if (!current || !http(current.url) || scope.documentId && current.documentId !== scope.documentId ||
        scope.sourcePage && current.url !== scope.sourcePage) throw new Error(FAILURE);
    return current;
  }
  async function allowed(scope) {
    const response = await appBridge.settings();
    const settings = response?.settings || response;
    if (!settings || settings.sniffing?.enabled === false || settings.sniffing?.disabledSites?.[scope.site]) throw new Error(FAILURE);
  }
  function message(s,type,itemId,options) {
    const scope = s.scope;
    return {type,tabId:scope.tabId,...(itemId ? {id:itemId} : {}),...(options ? {options} : {}),
      ...(scope.mode === 'single' ? {target:scope.target} : scope.mode === 'identify' ? {
        identify:true,frameId:scope.frameId,documentId:scope.documentId,sourcePage:scope.sourcePage,playerId:scope.playerId,
        ...(type !== 'LIST' && itemId ? {target:itemId} : {}),
      } : {})};
  }
  async function list(s) {
    const items = await call(message(s,'LIST'));
    if (!Array.isArray(items)) throw new Error(FAILURE);
    s.items = new Map(items.map(item => [item.id,item]));
    return items;
  }
  async function picture(item,url) {
    if (!url || !http(url)) return {};
    try { return await preview(item,url) || {}; } catch { return {}; }
  }
  async function summary(items) {
    return Promise.all(items.map(async item => ({id:item.id,kind:item.kind,title:fileTitle(item),
      width:item.width,height:item.height,duration:item.duration,mime:item.mime,
      mediaRequired:!!(item.mediaRequired || item.manifest || item.sourceType === 'page'),
      association:item.association,variantCount:new Set([item.url,...(item.variants || [])]).size,
      displayUrl:item.url,...await picture(item,item.thumbnail || (item.kind === 'image' ? item.url : ''))})));
  }
  function imageUrls(item) { return [...new Set([item.url,...(item.variants || [])])].filter(http); }
  async function inspect(s,item) {
    if (item.kind === 'image') {
      const variants = await Promise.all(imageUrls(item).map(async (url,i) => ({id:`image-${i}`,
        label:i === 0 ? '当前已加载图片' : `备选图片 ${i}`, ...await picture(item,url)})));
      s.inspections.set(item.id,{urls:new Map(imageUrls(item).map((url,i) => [`image-${i}`,url]))});
      return {title:fileTitle(item),variants,...await picture(item,item.url)};
    }
    const info = await call(message(s,'INSPECT',item.id));
    if (s.closed) throw new Error(FAILURE);
    const choices = formatOptions(info.formats,item.kind);
    s.inspections.set(item.id,{...choices,info});
    return {title:info.title || fileTitle(item),duration:info.duration,
      options:choices.options.map(({id,label,audioOnly,container}) => ({id,label,audioOnly,container})),bestId:choices.bestId,
      audioTracks:(info.formats || []).filter(f => f.id && f.vcodec === 'none' && hasCodec(f.acodec)).map(f => ({id:f.id,label:[f.language || '未标语言',f.label || f.id,f.ext].filter(Boolean).join(' · '),language:f.language || ''})),
      subtitles:info.subtitles || [],...await picture(item,info.thumbnail || item.thumbnail)};
  }
  function options(s,item,raw) {
    if (!raw || typeof raw !== 'object' || Array.isArray(raw)) throw new Error(FAILURE);
    const result = {name:typeof raw.name === 'string' ? raw.name : '',path:typeof raw.path === 'string' ? raw.path : ''};
    if (typeof raw.taskId === 'string') result.taskId = raw.taskId;
    if (item.kind === 'image') {
      const url = raw.imageVariant ? s.inspections.get(item.id)?.urls?.get(raw.imageVariant) : item.url;
      if (!url || !imageUrls(item).includes(url)) throw new Error(FAILURE);
      return {...result,imageUrl:url};
    }
    const entry = s.inspections.get(item.id);
    let format;
    if (item.mediaRequired || item.manifest || item.sourceType === 'page') {
      if (!entry || typeof raw.format !== 'string') throw new Error(FAILURE);
      format = chosenMediaFormat({...entry,chosen:raw.format,audioId:raw.audioId || ''});
      result.format = format.id;
      result.container = format.container || '';
    }
    result.mediaKind = format?.audioOnly ? 'audio' : hasCodec(format?.vcodec) ? 'video' : item.kind === 'audio' ? 'audio' : 'video';
    const languages = Array.isArray(raw.subtitleLanguages) ? raw.subtitleLanguages : [];
    if (!format?.audioOnly && languages.length) {
      if (!entry || languages.some(language => typeof language !== 'string' || !(entry.info.subtitles || []).some(sub => sub.language === language && !!sub.automatic === (raw.writeAutomaticSubtitles === true)))) throw new Error(FAILURE);
      result.subtitleLanguages = languages;
      result.writeAutomaticSubtitles = raw.writeAutomaticSubtitles === true;
      result.embedSubtitles = raw.embedSubtitles === true;
    } else Object.assign(result,{subtitleLanguages:[],writeAutomaticSubtitles:false,embedSubtitles:false});
    return result;
  }
  async function execute(s,command) {
    await frame(s.scope);
    await allowed(s.scope);
    if (s.closed) throw new Error(FAILURE);
    if (command.type === 'DEEP_SEARCH') {
      if (s.scope.mode !== 'identify') throw new Error(FAILURE);
      return call(message(s,'DEEP_SEARCH'));
    }
    const items = await list(s);
    await frame(s.scope);
    if (s.closed) throw new Error(FAILURE);
    if (command.type === 'LIST') return summary(items);
    const item = s.items.get(command.itemId);
    if (!item || s.scope.mode === 'single' && item.id !== s.scope.target) throw new Error(FAILURE);
    if (command.type === 'INSPECT') return inspect(s,item);
    if (command.type === 'RECOVERABLE') return call(message(s,command.type,item.id));
    const chosen = options(s,item,command.options || {});
    if (command.pathFreeze === true) {
      if (command.type !== 'CREATE' || item.kind === 'image') throw new Error(FAILURE);
      // Only the App's pending command supplies this correlation. Raw page
      // options cannot install provenance or a path capability.
      chosen.pathCreate = {sessionId:s.id,commandId:command.id};
    }
    return call(message(s,command.type,item.id,chosen));
  }
  async function stop(s,notify) {
    if (s.closed) return;
    s.closed = true;
    clearTimeout(s.timer);
    sessions.delete(s.id);
    s.items.clear();s.inspections.clear();s.results.clear();
    try {
      if (notify) await appBridge.request('mediaResult',{sessionId:s.id,closed:true,error:FAILURE});
    } catch { /* Closing never wakes the App or retries a download. */ }
    finally { s.release(); }
  }
  function schedule(s) { if (!s.closed) s.timer = setTimeout(() => { void tick(s); },pollMs); }
  async function tick(s) {
    if (s.closed) return;
    try {
      await frame(s.scope);
      const response = await appBridge.request('mediaPoll',{sessionId:s.id});
      if (s.closed) return;
      const command = response?.command;
      if (command?.type === 'CLOSE' || response?.closed) { await stop(s,false); return; }
      if (command) {
        if (typeof command.id !== 'string' || !TYPES.has(command.type)) throw new Error(FAILURE);
        let reply = s.results.get(command.id);
        if (!reply) {
          try { reply = {ok:true,data:await execute(s,command)}; }
          catch { reply = {ok:false,error:FAILURE}; }
          if (s.closed) return;
          s.results.set(command.id,reply);
        }
        await appBridge.request('mediaResult',{sessionId:s.id,commandId:command.id,...reply});
      }
      schedule(s);
    } catch { await stop(s,true); }
  }
  async function open(input) {
    if (!input || !Number.isInteger(input.tabId) || input.tabId < 0 || !['all','single','identify'].includes(input.mode)) throw new Error(FAILURE);
    await appBridge.wake();
    const release = appBridge.acquire();
    const s = {id:newOperationId(),closed:false,release,items:new Map(),inspections:new Map(),results:new Map(),timer:null};
    try {
      const scope = {tabId:input.tabId,mode:input.mode,frameId:Number.isInteger(input.frameId) ? input.frameId : 0,
        documentId:input.documentId || '',sourcePage:input.sourcePage || '',playerId:input.playerId || '',target:input.target};
      const current = await frame(scope);
      scope.documentId ||= current.documentId;
      scope.sourcePage ||= current.url;
      scope.site = input.site || new URL(scope.sourcePage).hostname;
      if (!scope.documentId || scope.mode === 'single' && typeof scope.target !== 'string' || scope.mode === 'identify' && !scope.playerId) throw new Error(FAILURE);
      s.scope = Object.freeze(scope);
      sessions.set(s.id,s);
      await allowed(s.scope);
      const identity = await appBridge.identity();
      const items = await summary(await list(s));
      await frame(s.scope);
      if (s.closed) throw new Error(FAILURE);
      await appBridge.request('mediaOpen',{...identity,sessionId:s.id,view:scope.mode,site:scope.site,items,pathFreezeVersion:1});
      if (s.closed) {
        await appBridge.request('mediaResult',{sessionId:s.id,closed:true,error:FAILURE}).catch(() => {});
        throw new Error(FAILURE);
      }
      schedule(s);
      return {opened:true,sessionId:s.id};
    } catch {
      await stop(s,false);
      throw new Error(FAILURE);
    }
  }
  async function invalidate(tabId) {
    await Promise.all([...sessions.values()].filter(s => s.scope.tabId === tabId).map(s => stop(s,true)));
  }
  return {open,invalidate};
}
