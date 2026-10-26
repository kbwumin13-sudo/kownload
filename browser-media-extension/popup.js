import {isSmall,formatOptions,chosenMediaFormat,pinnedCandidate,hasCodec} from './lib.js';

const $ = id => document.getElementById(id);
const params = new URLSearchParams(location.search);
const pinned = params.get('url') || '';
const pinnedFrame = params.has('frame') ? Number(params.get('frame')) : null;
const identify = params.get('mode') === 'identify';
const single = params.get('mode') === 'single';
const scoped = single || identify;
const playerScope = {identify:true,frameId:Number(params.get('frame')),documentId:params.get('doc') || '',sourcePage:params.get('source') || '',playerId:params.get('player') || ''};
const target = params.get('target') || '';
let pinnedApplied = false;
let tabId,host = '',kind = 'all',items = [];
const selected = new Set();
const formats = new Map();
const inspections = new Map();
const imageChoices = new Map();
const needsMedia = item => !!(item.manifest || item.sourceType === 'page' || item.mediaRequired);
function mediaKindFor(item,format) {
  if (item.kind === 'image') return undefined;
  if (format?.audioOnly) return 'audio';
  if (hasCodec(format?.vcodec)) return 'video';
  return item.kind === 'audio' ? 'audio' : 'video';
}
let recoverFor = '';
let busy = false;
const call = async (type,props = {}) => {
  const response = await chrome.runtime.sendMessage({type,tabId,...(single ? {target} : identify ? {...playerScope,...(type !== 'LIST' && props.id ? {target:props.id} : {})} : {}),...props});
  if (!response?.ok) throw new Error(response?.error || '扩展请求失败');
  return response.data;
};
const status = (message,ok = false) => { $('status').textContent = message; $('status').classList.toggle('ok',ok); };
const label = item => {
  const title = formats.get(item.id)?.info?.title || (item.metadataReady ? item.title : '');
  if (title) return title;
  try { const u = new URL(item.url); return decodeURIComponent(u.pathname.split('/').filter(Boolean).pop() || u.hostname); }
  catch { return '媒体资源'; }
};
const timeLabel = seconds => { const total=Math.round(seconds);return Number.isFinite(seconds)&&total>0 ? `${Math.floor(total/60)}:${String(total%60).padStart(2,'0')}` : ''; };
const category = {video:'视频',image:'图片',audio:'音频'};
function visible() {
  if (scoped) return items;
  const q = $('search').value.trim().toLowerCase();
  return items.filter(x => (kind === 'all' || x.kind === kind) && ($('showAll').checked || !isSmall(x)) && (!q || `${label(x)} ${x.url}`.toLowerCase().includes(q)));
}
function render() {
  for (const type of ['video','image','audio']) $(`n-${type}`).textContent = items.filter(x => x.kind === type && ($('showAll').checked || !isSmall(x))).length;
  $('n-all').textContent = items.filter(x => $('showAll').checked || !isSmall(x)).length;
  const rows = visible();
  const audioSelected = selected.size === 1 && items.some(x => selected.has(x.id) && mediaKindFor(x,formats.get(x.id)?.options.find(f => f.id === formats.get(x.id)?.chosen)) === 'audio');
  $('count').textContent = identify ? '请确认此播放器对应的一个视频' : single ? audioSelected ? '仅下载音频' : '仅下载这个视频' : `${selected.size} 项已选`;
  $('download').textContent = scoped ? audioSelected ? '下载音频' : '下载这个视频' : '下载已选资源';
  const list = $('list'); list.replaceChildren();
  if (!rows.length) { const empty = document.createElement('div'); empty.className = 'empty'; empty.textContent = identify ? '还未找到这个播放器的下载地址。可开启增强识别后重新播放；新发现的候选需你确认一项。' : single ? '这个视频的资源已失效，请回到原页面重新点击下载' : '当前分类尚未发现资源'; list.append(empty); }
  for (const x of rows) {
    const row = document.createElement('label'); row.className = 'item';
    const check = document.createElement('input'); check.type = identify ? 'radio' : 'checkbox'; check.name = 'video-choice'; check.checked = selected.has(x.id); check.addEventListener('change',() => { if (check.checked) { if (identify) selected.clear(); selected.add(x.id); if (needsMedia(x)) loadFormat(x).catch(e => status(e.message)); } else selected.delete(x.id); render(); });
    const main = document.createElement('div'); main.className = 'item-main';
    const thumb = x.kind === 'image' || x.thumbnail ? document.createElement('img') : document.createElement('div');
    thumb.className = x.kind === 'image' || x.thumbnail ? 'thumb' : 'thumb placeholder';
    if (thumb.tagName === 'IMG') { thumb.src = x.thumbnail || x.url; thumb.referrerPolicy = 'no-referrer'; thumb.addEventListener('error',() => thumb.style.visibility = 'hidden'); }
    else thumb.textContent = x.kind === 'video' ? '▶' : '♪';
    const details = document.createElement('div'); details.className = 'details';
    const title = document.createElement('strong'); title.textContent = label(x); title.title = title.textContent;
    const subtitle = document.createElement('small'); subtitle.textContent = [category[x.kind],new URL(x.url).hostname,x.association === 'search' ? '增强发现，待确认' : '',timeLabel(x.duration || formats.get(x.id)?.info?.duration),x.width && x.height ? `${x.width}×${x.height}` : ''].filter(Boolean).join(' · ');
    details.append(title,subtitle); main.append(thumb,details);
    if (!single) row.append(check);
    row.append(main);
    if (needsMedia(x)) { const tag = document.createElement('span'); tag.className = 'tag'; tag.textContent = '可选格式'; row.append(tag); }
    list.append(row);
  }
  $('selectAll').checked = rows.length > 0 && rows.every(x => selected.has(x.id));
  $('download').disabled = busy || !selected.size || $('siteOff').checked;
  if (selected.size !== 1 || !selected.has(recoverFor)) { recoverFor = ''; $('recoverTask').replaceChildren(new Option('请先查找任务','')); }
  $('update').disabled = busy || !recoverFor || !$('recoverTask').value;
  renderFormats();
}
function renderFormats() {
  const area = $('formats'); area.replaceChildren();
  for (const x of items.filter(x => selected.has(x.id) && needsMedia(x) && formats.has(x.id))) {
    const entry = formats.get(x.id);
    const line = document.createElement('label'); const name = document.createElement('span'); name.textContent = single || identify ? '下载格式' : `${label(x)} 格式`;
    const select = document.createElement('select');
    const empty = document.createElement('option'); empty.value = ''; empty.textContent = '请选择格式'; select.append(empty);
    for (const f of entry.options) { const option = document.createElement('option'); option.value = f.id; option.textContent = f.label; select.append(option); }
    select.value = entry.chosen || '';
    select.addEventListener('change',() => { entry.chosen = select.value; render(); });
    line.append(name,select); area.append(line);
    const choice = entry.options.find(f => f.id === entry.chosen);
    const audioFormats = entry.info.formats.filter(f => f.vcodec === 'none' && hasCodec(f.acodec));
    if (audioFormats.length && hasCodec(choice?.vcodec) && !hasCodec(choice.acodec)) {
      const row = document.createElement('label'), caption = document.createElement('span'), audio = document.createElement('select');caption.textContent='音轨';
      audio.append(new Option('自动选择音轨',''));
      for (const f of audioFormats) audio.append(new Option([f.language || '未标语言',f.label || f.id,f.ext].filter(Boolean).join(' · '),f.id));
      audio.value=entry.audioId || '';audio.addEventListener('change',()=>{entry.audioId=audio.value;});row.append(caption,audio);area.append(row);
    }
    if (entry.info.subtitles?.length && !choice?.audioOnly) {
      const row = document.createElement('label'), caption = document.createElement('span'), subtitles = document.createElement('select');caption.textContent='字幕';
      subtitles.append(new Option('不下载字幕',''));
      const manualLanguages = new Set(entry.info.subtitles.filter(s=>!s.automatic).map(s=>s.language));
      for (const sub of entry.info.subtitles.filter(s=>!s.automatic || !manualLanguages.has(s.language))) subtitles.append(new Option(`${sub.name || sub.language}${sub.automatic ? ' · 自动字幕' : ''}`,`${sub.language}|${sub.automatic ? 'auto' : 'manual'}`));
      subtitles.value=entry.subtitleChoice || '';subtitles.addEventListener('change',()=>{entry.subtitleChoice=subtitles.value;});row.append(caption,subtitles);area.append(row);
    }
  }
  for (const x of items.filter(x=>selected.has(x.id)&&x.kind==='image'&&x.variants?.length)) {
    const row=document.createElement('label'), caption=document.createElement('span'), select=document.createElement('select');caption.textContent='图片来源';
    for(const [i,url] of [...new Set([x.url,...x.variants])].entries()) select.append(new Option(i===0?'当前已加载图片':`备选图片 ${i}`,url));
    select.value=imageChoices.get(x.id)||x.url;select.addEventListener('change',()=>imageChoices.set(x.id,select.value));row.append(caption,select);area.append(row);
  }
}
async function refresh() {
  items = await call('LIST');
  if (single) { items = items.filter(x => x.id === target).slice(0,1); selected.clear(); if (items[0]) selected.add(items[0].id); }
  if (!scoped && pinned && !pinnedApplied) {
    const match = pinnedCandidate(items,pinned,pinnedFrame);
    if (match) { selected.add(match.id); pinnedApplied = true; }
  }
  for (const id of selected) if (!items.some(x => x.id === id)) selected.delete(id);
  render();
  for (const x of items.filter(x => selected.has(x.id) && needsMedia(x) && !formats.has(x.id))) loadFormat(x).catch(e => status(e.message));
}
async function loadFormat(x) {
  if (formats.has(x.id)) return formats.get(x.id);
  if (inspections.has(x.id)) return inspections.get(x.id);
  const promise = (async () => {
    status(`正在检查 ${label(x)}…`);
    const info = await call('INSPECT',{id:x.id});
    const result = formatOptions(info.formats,x.kind);
    const entry = {...result,chosen:result.bestId,info};
    formats.set(x.id,entry);
    if (info.title) { x.title=info.title; x.metadataReady=true; }
    x.thumbnail=info.thumbnail || x.thumbnail;x.duration=info.duration || x.duration;
    render();
    status(entry.chosen ? `已为 ${label(x)} 默认选择${x.kind === 'audio' ? '音频格式' : '最佳带音轨格式'}` : entry.options.every(f => f.audioOnly) ? '仅找到音频，请明确选择“仅音频”格式' : '请选择下载格式；标注“轨道未确认”的格式需要手动确认',!!entry.chosen);
    return entry;
  })();
  inspections.set(x.id,promise);
  try { return await promise; } finally { inspections.delete(x.id); }
}
async function ensureFormats(chosen) {
  for (const x of chosen.filter(needsMedia)) await loadFormat(x);
}
async function perform(type) {
  if (busy) return;
  const chosen = items.filter(x => selected.has(x.id));
  if (!chosen.length || scoped && chosen.length !== 1) return;
  if (type === 'UPDATE' && (chosen.length !== 1 || recoverFor !== chosen[0].id || !$('recoverTask').value)) { status('请选择一项资源和本页可更新任务'); return; }
  busy = true; render();
  try {
    await ensureFormats(chosen);
    const ids = [],errors = [];
    for (const x of chosen) {
      try {
        status(`正在提交 ${label(x)}…`);
        const choice = formats.get(x.id);
        const format = choice ? chosenMediaFormat(choice) : null;
        const mediaKind = mediaKindFor(x,format);
        const subtitle = choice?.subtitleChoice && !format?.audioOnly ? choice.subtitleChoice.split('|') : [];
        const result = await call(type,{id:x.id,options:{format:format?.id,mediaKind,container:format?.container || '',subtitleLanguages:subtitle[0]?[subtitle[0]]:[],writeAutomaticSubtitles:subtitle[1]==='auto',embedSubtitles:!!subtitle[0],imageUrl:x.kind==='image'?imageChoices.get(x.id)||'':undefined,name:$('name').value.trim(),path:$('path').value.trim(),taskId:$('recoverTask').value}});
        ids.push(`${label(x)}：${type === 'UPDATE' ? '已更新并继续' : result.duplicate ? '已在 kownload 中' : '已交给 kownload 下载'}`);
      } catch(e) { errors.push(`${label(x)}：${e.message}`); }
    }
    status([...ids,...errors].join('\n'),errors.length === 0);
  } catch(e) { status(e.message); }
  finally { busy = false; render(); }
}
async function start() {
  if (scoped) document.body.classList.add('single');
  if ($('deepSearch')) $('deepSearch').hidden = !identify;
  try {
    const explicit = Number(params.get('tab'));
    const tab = Number.isInteger(explicit) && explicit > 0 ? await chrome.tabs.get(explicit) : (await chrome.tabs.query({active:true,currentWindow:true}))[0];
    if (!tab?.id || !/^https?:/.test(tab.url || '')) throw new Error('此页面无法嗅探媒体');
    tabId = tab.id; host = new URL(tab.url).hostname; $('site').textContent = host;
    $('siteOff').checked = await call('SITE',{host});
    $('showAll').checked = (await chrome.storage.local.get('showAll')).showAll === true;
    await refresh();
    // An empty path follows the app's current default, including a directory
    // changed by the first-download tutorial while this popup stays open.
    // Only a location entered by the user is sent as an explicit override.
  } catch(e) { status(e.message); }
}
document.querySelectorAll('.chip').forEach(button => button.addEventListener('click',() => { kind = button.dataset.kind; document.querySelectorAll('.chip').forEach(x => x.classList.toggle('active',x === button)); render(); }));
$('search').addEventListener('input',render);
$('showAll').addEventListener('change',async () => { await chrome.storage.local.set({showAll:$('showAll').checked}); render(); });
$('selectAll').addEventListener('change',() => { if (scoped) return; for (const x of visible()) { if ($('selectAll').checked) { selected.add(x.id); if (needsMedia(x)) loadFormat(x).catch(e => status(e.message)); } else selected.delete(x.id); } render(); });
$('siteOff').addEventListener('change',async () => { try { await call('SITE',{host,set:$('siteOff').checked}); if ($('siteOff').checked) { selected.clear(); items = []; } else await refresh(); render(); } catch(e) { status(e.message); } });
$('deepSearch')?.addEventListener('click',async () => { try { await call('DEEP_SEARCH'); status('增强识别已开启。请重新播放这个视频，再回来确认新发现的候选；不会自动选中。',true); } catch(e) { status(e.message); } });
$('download').addEventListener('click',() => perform('CREATE'));
$('findTasks').addEventListener('click',async () => {
  const chosen = items.filter(x => selected.has(x.id));
  if (chosen.length !== 1) { status('请先选择一项资源以查找同页任务'); return; }
  try {
    const tasks = await call('RECOVERABLE',{id:chosen[0].id});
    recoverFor = chosen[0].id;
    const select = $('recoverTask'); select.replaceChildren(new Option('请选择任务',''));
    for (const task of tasks) select.add(new Option(`${task.name} · ${task.status === 'pause' ? '已暂停' : '失败'}`,task.id));
    status(tasks.length ? `找到 ${tasks.length} 项可更新任务` : '本页没有同协议的暂停或失败任务',tasks.length > 0);
    render();
  } catch(e) { status(e.message); }
});
$('recoverTask').addEventListener('change',render);
$('update').addEventListener('click',() => perform('UPDATE'));
chrome.runtime.onMessage.addListener(msg => { if (msg.type === 'CHANGED' && msg.tabId === tabId) refresh().catch(e => status(e.message)); });
start();
