const EXT = /\.(m3u8|mpd|mp4|m4v|mov|webm|mkv|mp3|m4a|aac|ogg|oga|wav|flac|jpg|jpeg|png|webp|gif|avif|bmp)(?:$|[?#])/i;
const FRAGMENT = /(?:^|[\/_\-.])(seg(?:ment)?|chunk|frag(?:ment)?|part|init)(?:[\/_\-.]?\d+)?\.(?:m4s|ts|aac|mp4)|\.m4s(?:$|[?#])/i;
export function classify(url, mime = '', requestType = '', disposition = '') {
  let pathname;
  try { const u = new URL(url); if (!['http:','https:'].includes(u.protocol)) return null; pathname = u.pathname; } catch { return null; }
  if (FRAGMENT.test(pathname)) return null;
  const m = String(mime).split(';')[0].trim().toLowerCase();
  const ext = (pathname.match(EXT) || String(disposition).replace(/[\"']/g,'').split(';').map(x => x.trim().match(EXT)).find(Boolean))?.[1]?.toLowerCase();
  if (/\.ts$/i.test(pathname) && !FRAGMENT.test(pathname)) return {kind:'video',manifest:false};
  if (/mpegurl|vnd\.apple\.mpegurl|dash\+xml/.test(m) || ['m3u8','mpd'].includes(ext)) return {kind:'video', manifest:true};
  if (m.startsWith('video/') || ['mp4','m4v','mov','webm','mkv'].includes(ext)) return {kind:'video', manifest:false};
  if (m.startsWith('audio/') || ['mp3','m4a','aac','ogg','oga','wav','flac'].includes(ext)) return {kind:'audio', manifest:false};
  if (m.startsWith('image/') || ['jpg','jpeg','png','webp','gif','avif','bmp'].includes(ext)) return {kind:'image', manifest:false};
  if (requestType === 'image' && (!m || m.startsWith('image/'))) return {kind:'image', manifest:false};
  if (requestType === 'video' && (!m || m.startsWith('video/'))) return {kind:'video', manifest:false};
  if (requestType === 'audio' && (!m || m.startsWith('audio/'))) return {kind:'audio', manifest:false};
  return null;
}
export function candidateKey(c) { return [c.tabId,c.frameId,c.documentId || 'unknown-document',c.sourcePage || '',c.pageEpoch || 0,c.url].join('\u001f'); }
export function pinnedCandidate(items,url,frameId) {
  const matches = items.filter(item => item.url === url);
  return Number.isInteger(frameId) ? matches.find(item => item.frameId === frameId) : matches[0];
}
export function isSmall(c) { return c.kind === 'image' && Number.isFinite(c.width) && Number.isFinite(c.height) && c.width > 0 && c.height > 0 && c.width < 128 && c.height < 128; }
export function mergeCandidate(old, next) {
  if (!old) return next;
  // A DOM tag is only a hint: retain MIME/manifest evidence for this URL and route.
  const type = classify(next.url,next.mime || old.mime || '',next.kind || old.kind || '');
  const manifest = !!(old.manifest || next.manifest || type?.manifest);
  return {...old,...next,kind:manifest ? 'video' : type?.kind || next.kind || old.kind,manifest,
    mediaRequired:!!(old.mediaRequired || next.mediaRequired),width:next.width || old.width, height:next.height || old.height,
    title:next.title || old.title, thumbnail:next.thumbnail || old.thumbnail,playerId:next.playerId || old.playerId,imageId:next.imageId || old.imageId,
    relatedUrls:[...new Set([...(old.relatedUrls || []),...(next.relatedUrls || [])])].slice(0,64),
    contextUrls:[...new Set([...(old.contextUrls || []),...(next.contextUrls || [])])].slice(0,64),
    header:{...old.header,...next.header}};
}
export function nativeData(outer) {
  if (!outer || outer.code !== 0) throw new Error(outer?.msg || outer?.message || 'Native Messaging 请求失败');
  const rest = outer.data;
  if (!rest || rest.code !== 0) throw new Error(rest?.msg || rest?.message || 'Gopeed 请求失败');
  return rest.data;
}
export function inspectData(data) {
  if (!data || typeof data !== 'object' || !Array.isArray(data.formats)) throw new Error('Gopeed 返回的媒体格式无效');
  if (!data.supported) throw new Error(data.reason || 'Gopeed 不支持此资源');
  if (!data.formats.length) throw new Error('未找到可下载格式');
  return data;
}
export function taskId(data) {
  const id = typeof data === 'string' ? data : data?.id || data?.taskId;
  if (!id || typeof id !== 'string') throw new Error('Gopeed 未返回任务 ID');
  return id;
}
export function safeHeader(headers = []) {
  const out = {};
  for (const h of headers) {
    const k = h.name?.toLowerCase();
    if (k === 'referer' || k === 'origin' || k === 'user-agent') out[k === 'user-agent' ? 'User-Agent' : k === 'referer' ? 'Referer' : 'Origin'] = h.value;
  }
  return out;
}
// Sensitive headers stay in the worker's short-lived request vault, never in
// candidate/session storage. Only actually observed resource scopes are exported.
export function protectedHeaders(headers = []) {
  const out = {};
  for (const h of headers) {
    const name = String(h.name || ''); const value = h.value;
    if (!/^(?:authorization|x-api-key|x-auth-token|x-playback-token)$/i.test(name) || typeof value !== 'string' || value.length > 8192 || /[\r\n]/.test(value)) continue;
    out[name] = value;
  }
  return out;
}
export function cookieScopeHosts(item) {
  const roots = [item.url,...(item.relatedUrls || []),...(item.contextUrls || [])];
  const hosts = new Set();
  for (const raw of roots) {
    try { const u = new URL(raw); if (/^https?:$/.test(u.protocol)) hosts.add(u.hostname); } catch {}
    if (hosts.size >= 16) break;
  }
  return [...hosts];
}
export const hasCodec = value => !!value && value !== 'none' && value !== 'unknown';
export function formatChoice(format, formats) {
  if (!format?.id) throw new Error('请选择媒体格式');
  if (hasCodec(format.vcodec) && !hasCodec(format.acodec) && !format.id.includes('+')) {
    const audioFormats = [...formats].reverse().filter(f => f.id && f.vcodec === 'none' && hasCodec(f.acodec));
    const audio = (format.ext === 'mp4' && audioFormats.find(f => f.ext === 'm4a')) || audioFormats[0];
    if (!audio) throw new Error('此清晰度缺少音轨，检查结果中也没有可合并的音频格式');
    return {id:`${format.id}+${audio.id}`,container:format.ext === 'mp4' && audio.ext === 'm4a' ? 'mp4' : 'mkv'};
  }
  return {id:format.id,container:format.container || format.ext || ''};
}
export function formatOptions(formats,mediaKind = 'video') {
  const options = [];
  for (const format of formats || []) {
    if (!format?.id) continue;
    let choice;
    try { choice = formatChoice(format,formats); } catch { continue; }
    const audioOnly = format.vcodec === 'none' && hasCodec(format.acodec);
    const unknownTracks = !hasCodec(format.vcodec) && !audioOnly;
    const resolution = format.height ? `${format.height}p` : format.width ? `${format.width}px` : format.label || '原始画质';
    const container = choice.container || format.ext || '未知容器';
    const label = audioOnly ? `仅音频 · ${container}` : unknownTracks ? `轨道未确认 · ${container}` : `${resolution} · ${container}${choice.id.includes('+') ? ' · 自动配音轨' : hasCodec(format.acodec) ? ' · 含音轨' : ''}`;
    const detail = [format.language,format.fps ? `${format.fps}fps` : '',hasCodec(format.vcodec) ? format.vcodec.split('.')[0] : ''].filter(Boolean).join(' · ');
    options.push({...choice,label:detail ? `${label} · ${detail}` : label,audioOnly,rawId:format.id,vcodec:format.vcodec,acodec:format.acodec,ext:format.ext,language:format.language || '',height:format.height || 0,width:format.width || 0,filesize:format.filesize || 0});
  }
  if (!options.length) throw new Error('没有可下载且带音轨的视频或音频格式');
  const defaults = options.filter(x => mediaKind === 'audio' ? x.audioOnly : hasCodec(x.vcodec));
  if (!defaults.length) return {options,bestId:''};
  const best = defaults.reduce((a,b) => {
    const score = x => [x.height,x.width,x.container === 'mp4' ? 1 : 0,x.filesize];
    const aa = score(a),bb = score(b);
    for (let i = 0; i < aa.length; i++) if (aa[i] !== bb[i]) return bb[i] > aa[i] ? b : a;
    return a;
  });
  return {options,bestId:best.id};
}
export function chosenMediaFormat(entry) {
  const selected = entry?.options.find(f => f.id === entry.chosen);
  if (!selected) throw new Error('请选择可下载的格式');
  if (entry.audioId && hasCodec(selected.vcodec) && !hasCodec(selected.acodec)) {
    const audio = entry.info?.formats.find(f => f.id === entry.audioId && f.vcodec === 'none' && hasCodec(f.acodec));
    if (!audio) throw new Error('选择的音轨已失效，请重新检查');
    return {...selected,id:`${selected.rawId}+${audio.id}`,container:selected.ext === 'mp4' && audio.ext === 'm4a' ? 'mp4' : 'mkv'};
  }
  return selected;
}
export function recoverableTasks(tasks,sourcePage,protocol) {
  if (!Array.isArray(tasks) || !sourcePage || !protocol) return [];
  return tasks.filter(t => t && ['pause','error'].includes(t.status) && (t.protocol || t.meta?.req?.protocol) === protocol && t.meta?.req?.extra?.sourcePage === sourcePage && typeof t.id === 'string');
}
export function nativeCookies(cookies) {
  return cookies.map(c => ({name:c.name,value:c.value,domain:c.domain,path:c.path,secure:c.secure,hostOnly:c.hostOnly,httpOnly:c.httpOnly,expirationDate:c.expirationDate}));
}
export function nativeForwardMessage(method,path,data) {
  if (typeof path !== 'string' || !path.startsWith('/')) throw new Error('无效的 Gopeed 接口路径');
  return {method:'forward',params:{method,path:path.startsWith('/api/v1/') ? path : `/api/v1${path}`,...(data === undefined ? {} : {data})}};
}
const routeKey = (tabId,frameId) => `${tabId}:${frameId}`;
const candidateIndexes = new WeakMap();
const lookupKey = item => [item.tabId,item.frameId,item.sourcePage || '',item.pageEpoch || 0,item.url].join('\u001f');
function indexFor(state) {
  let index = candidateIndexes.get(state);
  if (!index) { index = new Map(); for (const item of Object.values(state.items || {})) { const key = lookupKey(item); if (!index.has(key)) index.set(key,new Map()); index.get(key).set(item.documentId || '',item.id || candidateKey(item)); } candidateIndexes.set(state,index); }
  return index;
}
export function resetCandidateIndex(state) { candidateIndexes.delete(state); }
export function applyCandidate(state,item) {
  state.items ||= {};
  state.routes ||= {};
  state.topCommit ||= {};
  const route = state.routes[routeKey(item.tabId,item.frameId)];
  const barrier = Math.max(state.topCommit[item.tabId] || 0,route?.at || 0);
  if (!Number.isFinite(item.capturedAt) || item.capturedAt < barrier) return null;
  if (route?.documentId && item.documentId && route.documentId !== item.documentId) return null;
  if (!item.network && route?.url && route.at <= item.capturedAt && item.sourcePage && item.sourcePage !== route.url) return null;
  if (item.network && route?.url && route.at <= item.capturedAt) item = {...item,sourcePage:route.url};
  item = {...item,pageEpoch:barrier};
  const index = indexFor(state),groupKey = lookupKey(item);
  const group = index.get(groupKey) || new Map();
  const exact = group.get(item.documentId || '');
  const fallback = item.documentId ? group.get('') : group.size === 1 ? group.values().next().value : undefined;
  const key = exact || fallback || candidateKey(item);
  group.set(item.documentId || '',key); index.set(groupKey,group);
  state.items[key] = mergeCandidate(state.items[key],{...item,id:key});
  return state.items[key];
}
export function applyNavigation(state,event) {
  resetCandidateIndex(state);
  state.items ||= {};
  state.submitted ||= {};
  state.routes ||= {};
  state.topCommit ||= {};
  const {tabId,frameId,timeStamp:at,url,documentId = '',kind} = event;
  if (!Number.isFinite(at)) return state;
  const key = routeKey(tabId,frameId);
  if ((state.routes[key]?.at || 0) > at) return state;
  state.routes[key] = {at,url,documentId,kind};
  if (frameId === 0) state.topCommit[tabId] = at;
  if (kind === 'commit' && frameId === 0) {
    for (const route of Object.keys(state.routes)) if (route.startsWith(`${tabId}:`) && route !== key) delete state.routes[route];
  }
  for (const [id,item] of Object.entries(state.items)) {
    if (item.tabId !== tabId || (frameId !== 0 && item.frameId !== frameId)) continue;
    const old = !Number.isFinite(item.capturedAt) || item.capturedAt < at ||
      (!item.network && item.frameId === frameId && !!item.sourcePage && item.sourcePage !== url) ||
      (item.frameId === frameId && !!documentId && !!item.documentId && item.documentId !== documentId);
    if (old) {
      delete state.items[id];
      for (const mark of Object.keys(state.submitted)) if (mark.startsWith(`${tabId}\u001f${id}\u001f`)) delete state.submitted[mark];
    } else if (item.network && item.frameId === frameId) item.sourcePage = url;
  }
  return state;
}


// Keeps worker state in memory and checkpoints only dirty tabs. Navigation and
// user actions can explicitly await flush rather than relying on worker shutdown.
export function createSessionStore(storage,{onCommit = () => {},delay = 150,maxItems = 3000,maxPerTab = 1000} = {}) {
  const prefix = 'mediaTabV2:',dirty = new Set();
  let loaded,flushing,timer,legacy = false;
  const read = () => loaded ||= storage.get(null).then(raw => {
    const state = {items:{},submitted:{},routes:{},topCommit:{},searches:{}};
    const parts = Object.entries(raw || {}).filter(([key]) => key.startsWith(prefix));
    if (parts.length) for (const [,part] of parts) for (const key of Object.keys(state)) Object.assign(state[key],part?.[key] || {});
    else if (raw?.mediaStateV1) { Object.assign(state,raw.mediaStateV1); legacy = true; for (const item of Object.values(state.items)) dirty.add(item.tabId); for (const key of Object.keys(state.routes)) dirty.add(Number(key.split(':')[0])); }
    return state;
  });
  function trim(state) {
    const byTab = new Map(),entries = Object.entries(state.items);
    for (const [id,item] of entries) { const list = byTab.get(item.tabId) || []; list.push([id,item]); byTab.set(item.tabId,list); }
    const erase = ([id,item]) => { delete state.items[id]; for (const mark of Object.keys(state.submitted)) if (mark.startsWith(`${item.tabId}\u001f${id}\u001f`)) delete state.submitted[mark]; dirty.add(item.tabId); };
    const oldest = list => list.sort((a,b) => (a[1].kind === 'video') - (b[1].kind === 'video') || (a[1].capturedAt || 0) - (b[1].capturedAt || 0));
    for (const list of byTab.values()) if (list.length > maxPerTab) for (const entry of oldest(list).slice(0,list.length-maxPerTab)) erase(entry);
    let remaining = Object.entries(state.items);
    if (remaining.length > maxItems) for (const entry of oldest(remaining).slice(0,remaining.length-maxItems)) erase(entry);
    // Conservative UTF-16 estimate leaves room for session storage bookkeeping.
    let bytes = JSON.stringify(state).length * 2;
    if (bytes > 6 * 1024 * 1024) for (const entry of oldest(Object.entries(state.items))) {
      // Estimate only the removed entry. Serializing the whole multi-MiB state
      // after every eviction turns a burst of signed manifests into quadratic work.
      const [id,item] = entry;
      bytes -= (JSON.stringify(id).length + JSON.stringify(item).length + 2) * 2;
      for (const [mark,value] of Object.entries(state.submitted)) if (mark.startsWith(`${item.tabId}\u001f${id}\u001f`)) bytes -= (JSON.stringify(mark).length + JSON.stringify(value).length + 2) * 2;
      erase(entry); if (bytes <= 5 * 1024 * 1024) break;
    }
    if (entries.length !== Object.keys(state.items).length) resetCandidateIndex(state);
  }
  const flush = () => {
    if (timer && typeof clearTimeout === 'function') clearTimeout(timer); timer = null;
    if (flushing) return flushing;
    flushing = (async () => {
      const state = await read(); trim(state);
      while (dirty.size) {
        const tabs = [...dirty]; dirty.clear();
        const payload = {};
        for (const tabId of tabs) payload[prefix+tabId] = {
          items:Object.fromEntries(Object.entries(state.items).filter(([,x]) => x.tabId === tabId)),
          submitted:Object.fromEntries(Object.entries(state.submitted).filter(([key]) => key.startsWith(`${tabId}\u001f`))),
          routes:Object.fromEntries(Object.entries(state.routes).filter(([key]) => key.startsWith(`${tabId}:`))),
          topCommit:state.topCommit[tabId] === undefined ? {} : {[tabId]:state.topCommit[tabId]},
          searches:Object.fromEntries(Object.entries(state.searches || {}).filter(([key]) => key.startsWith(`${tabId}:`) && key && state.searches[key].until > Date.now()))
        };
        try { await storage.set(payload); }
        catch(error) { tabs.forEach(tab => dirty.add(tab)); throw error; }
        if (legacy && storage.remove) { await storage.remove('mediaStateV1'); legacy = false; }
        for (const tabId of tabs) onCommit(tabId,state);
      }
    })().finally(() => { flushing = null; });
    return flushing;
  };
  const changed = (tabId,immediate = false) => {
    dirty.add(tabId);
    if (immediate) return flush();
    if (!timer && typeof setTimeout === 'function') timer = setTimeout(() => { timer = null; flush().catch(() => {}); },delay);
    return Promise.resolve();
  };
  return {read,changed,flush};
}

export function sameDocument(a,b) {
  return a.tabId === b.tabId && a.frameId === b.frameId && a.documentId === b.documentId && a.sourcePage === b.sourcePage && (a.pageEpoch || 0) === (b.pageEpoch || 0);
}
export function groupMedia(items) {
  const children = new Set(),index = new Map(),images = new Map(),parents = new Map(items.map(x => [x.id,x.id]));
  const find = id => { let root = id; while (parents.get(root) !== root) root = parents.get(root); while (id !== root) { const next = parents.get(id); parents.set(id,root); id = next; } return root; };
  const key = (item,url = item.url) => [item.tabId,item.frameId,item.documentId,item.sourcePage,item.pageEpoch || 0,url].join('\u001f');
  for (const item of items) { const k = key(item); const list = index.get(k) || []; list.push(item); index.set(k,list); }
  for (const root of items) for (const related of root.relatedUrls || []) for (const item of index.get(key(root,related)) || []) {
    if (root.id !== item.id && (!item.playerId || item.playerId === root.playerId)) { children.add(item.id); parents.set(find(item.id),find(root.id)); }
  }
  const components = new Map();
  for (const item of items) { const root = find(item.id),list = components.get(root) || []; list.push(item); components.set(root,list); }
  for (const list of components.values()) if (list.every(x => children.has(x.id))) {
    const representative = [...list].sort((a,b) => Number(!!b.playerId)-Number(!!a.playerId) || a.id.localeCompare(b.id))[0];
    children.delete(representative.id);
  }
  for (const item of items) if (item.kind === 'image' && item.imageId) {
    const k = [item.tabId,item.frameId,item.documentId,item.imageId].join('\u001f');
    const old = images.get(k);
    if (!old || (item.capturedAt || 0) >= (old.capturedAt || 0)) images.set(k,item);
  }
  return items.filter(item => !children.has(item.id) && (item.kind !== 'image' || !item.imageId || images.get([item.tabId,item.frameId,item.documentId,item.imageId].join('\u001f')) === item));
}
export function mediaRelatedUrls(info,rootUrl) {
  const urls = new Set();
  for (const f of info.formats || []) for (const raw of [f.manifestUrl,f.sourceUrl]) {
    if (!raw) continue;
    try { const url = new URL(raw,rootUrl); if (/^https?:$/.test(url.protocol) && url.href !== rootUrl) urls.add(url.href); } catch {}
  }
  return [...urls];
}
export function mediaContextUrls(info,rootUrl) {
  const urls = new Set(mediaRelatedUrls(info,rootUrl));
  for (const raw of info.contextUrls || []) {
    try { const u = new URL(raw,rootUrl); if (/^https?:$/.test(u.protocol)) urls.add(u.href); } catch {}
  }
  return [...urls].slice(0,64);
}
