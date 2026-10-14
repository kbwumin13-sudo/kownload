// Selection layouts derived from hmjz100/LinkSwift (AGPL-3.0-or-later).
// This function is serialized by chrome.scripting; keep every dependency inside it.
// No network, cookie access, storage writes, event bridge or token refresh in MAIN.
export function readNetdiskPage(provider, sourcePage) {
  if (window !== window.top || location.href !== sourcePage) return null;
  const layouts = {
    baidu: ['.file-list', '.wp-s-core-pan'],
    aliyun: ['[class^="node-list-table-view--"]', '[class^="node-list-grid-view--"]', '[class^="node-list--"]'],
    mcloud: ['.main_file_list', '.home-page'],
    tcloud: ['.c-file-list'],
    xunlei: ['[class*="SourceListItem__item--"]'],
    quark: ['.file-list'], uc: ['.file-list'],
    '123pan': ['.ant-table-wrapper', '.tiled-list', '.file-list', '.custom-table-wrapper', '[class^="mfy_h-table-module__shell__"]'],
    guangya: ['.swangpan-file-list__root[data-ui="file-list"]']
  };
  if (!Object.hasOwn(layouts, provider)) return null;
  const scalar = (v, max = 4096) => typeof v === 'string' || typeof v === 'number' ? String(v).slice(0, max) : '';
  const record = v => v && typeof v === 'object' ? v : {};
  const first = (v, keys) => keys.map(k => v[k]).find(x => x !== undefined && x !== null && x !== '');
  const list = v => Array.isArray(v) ? v.slice(0, 10000) : [];
  const tokens = {}, share = {}, files = [], seen = new Set();
  const put = (out, key, value) => { const s = scalar(value); if (s && !/[\r\n\x00]/.test(s)) out[key] = s; };
  const schemas = {
    baidu: {id:['fs_id','fsId','id'], name:['server_filename','filename','name'], size:['size'], dir:['isdir','isDir'], hash:['md5']},
    aliyun: {id:['file_id','fileId','id'], name:['name','fileName'], size:['size'], dir:['type','isDir'], hash:['content_hash','contentHash']},
    mcloud: {id:['contentID','contentId','catalogID','catalogId','id'], name:['contentName','catalogName','name'], size:['contentSize','size'], dir:['isDir','type'], hash:['digest','md5']},
    tcloud: {id:['id','fileId'], name:['name','fileName'], size:['size','fileSize'], dir:['isFolder','isDir','fileType'], hash:['md5']},
    xunlei: {id:['id','fileId'], name:['name','fileName'], size:['size'], dir:['kind','isDir'], hash:['hash','md5']},
    quark: {id:['fid','id'], name:['file_name','name'], size:['size'], dir:['dir','isDir','file_type'], hash:['md5']},
    uc: {id:['fid','id'], name:['file_name','name'], size:['size'], dir:['dir','isDir','file_type'], hash:['md5']},
    '123pan': {id:['FileId','fileId','id'], name:['FileName','fileName','name'], size:['Size','size'], dir:['Type','type','isDir'], hash:['Etag','etag']},
    guangya: {id:['fileId','id'], name:['fileName','name'], size:['fileSize','size'], dir:['fileType','type','isDir'], hash:['hash','md5']}
  };
  function add(raw) {
    raw = record(raw);
    const s = schemas[provider], id = scalar(first(raw,s.id),256), name = scalar(first(raw,s.name),512);
    if (!id || !name || /[/\\\x00]/.test(name) || name === '.' || name === '..' || seen.has(id)) return;
    const size = Number(first(raw,s.size) || 0), dir = first(raw,s.dir);
    if (!Number.isSafeInteger(size) || size < 0) return;
    let isDir = dir === true || dir === 1 || dir === '1' || ['folder','directory','drive#folder'].includes(dir);
    if ((provider === 'quark' || provider === 'uc') && raw.dir == null && raw.isDir == null) isDir = raw.file === false || raw.file_type === 0;
    if (provider==='guangya' && raw.resType!==undefined) isDir=raw.resType===2;
    if (provider === 'mcloud' && (raw.catalogID || raw.catalogId) && !raw.contentID && !raw.contentId) isDir = true;
    const meta = {};
    // Only identifiers/attributes used by providers, never download URLs or auth.
    for (const [key, aliases] of Object.entries({driveId:['drive_id','driveId'],parentId:['parent_file_id','parentFileId','parentId','pdir_fid'],category:['category'],S3KeyFlag:['S3KeyFlag'],StorageNode:['StorageNode'],share_fid_token:['share_fid_token']})) put(meta,key,first(raw,aliases));
    files.push({id,name,size,isDir,hash:scalar(first(raw,s.hash),256),meta});
    seen.add(id);
    put(tokens,'stoken',raw.stoken);
  }
  function propsOf(node) {
    const out = [];
    for (const key of Object.keys(node || {}).slice(0,256)) {
      if (key.startsWith('__reactProps$')) out.push(record(node[key]));
      if (!key.startsWith('__reactFiber$') && !key.startsWith('__reactInternalInstance$')) continue;
      let fiber = node[key];
      for (let i=0; fiber && i<24; i++,fiber=fiber.return) {
        out.push(record(fiber.memoizedProps || fiber.stateNode?.props || fiber.pendingProps));
        if (fiber._currentElement) out.push(record(fiber._currentElement._owner?._instance?.props));
      }
    }
    return out;
  }
  function selected(props) {
    const rows = list(props.dataSource || props.list || props.loadedFileList || props.files);
    const selection = props.selectedRowKeys ?? props.selectedKeys ?? props.selectedItems ?? props.rowSelection?.selectedRowKeys;
    const keys = new Set((typeof selection === 'string' ? selection.split(',') : list(selection)).map(x => scalar(typeof x === 'object' ? first(x,schemas[provider].id) : x,256)));
    for (const row of rows) if (keys.has(scalar(first(row,schemas[provider].id),256)) || selection == null && (row.checked === true || row.selected === true)) add(row);
    for (const key of ['stoken','shareToken','share_token','shareId','share_id','parentId','parent_file_id']) {
      if (key === 'shareId' || key === 'share_id') put(share,'shareId',props[key]);
      else if (key === 'parentId' || key === 'parent_file_id') put(share,'parentId',props[key]);
      else put(tokens,key,props[key]);
    }
  }
  if (provider === 'baidu' && !files.length && typeof window.require === 'function') {
    try {
      const context=window.require('system-core:context/context.js');
      list(context?.instanceForSystem?.list?.getSelected?.()).forEach(add);
    } catch { /* Older Baidu builds may not expose this fixed selection accessor. */ }
  }
  for (const selector of layouts[provider]) for (const node of Array.from(document.querySelectorAll(selector)).slice(0,2000)) {
    const vue = record(node.__vue__ || node.__vueParentComponent?.proxy);
    if (provider === 'baidu') {
      list(vue.allFileList).filter(x => x.selected === true || x.selected === 1).forEach(add);
      list(vue.selectedList).forEach(add);
    } else if (provider === 'mcloud') {
      list(vue.selectList).forEach(x => add(x.item || x));
      const watchers = record(vue._computedWatchers);
      list(watchers.fileList?.value || vue.fileList).forEach((x,i) => {if (list(vue.selectedFile).includes(i)) add(x);});
      list(watchers.dirList?.value || vue.dirList).forEach((x,i) => {if (list(vue.selectedDir).includes(i)) add(x);});
    } else if (provider === 'tcloud') list(vue.selectedList).forEach(add);
    else if (provider === 'xunlei' && list(vue.selected).includes(vue.info?.id)) add(vue.info);
    for (const props of propsOf(node)) selected(props);
    for (const key of ['shareId','shareid','shareCode','parentId']) put(share,key === 'shareid' || key === 'shareCode' ? 'shareId' : key,vue[key]);
    put(tokens,'stoken',vue.stoken);
  }
  // DOM fallback requires an explicit row selection; a single-file share is not auto-selected.
  for (const node of Array.from(document.querySelectorAll('[data-file-id][aria-selected="true"], [data-fid][aria-selected="true"], [data-file-id].selected, [data-fid].selected, [data-file-id]:has(input[type="checkbox"]:checked)')).slice(0,2000)) {
    const d = record(node.dataset);
    const raw = {id:d.fileId || d.fid,name:d.fileName || d.name,size:d.size,isDir:d.isDir === 'true'};
    add(raw);
    for (const props of propsOf(node)) add(props.record || props.file || props.item);
  }
  function stored(key) {
    try { const s = localStorage.getItem(key); if (!s || s.length>32768) return null; try {return JSON.parse(s);} catch {return s;} } catch {return null;}
  }
  const url = new URL(sourcePage), decode = v => {try {return decodeURIComponent(v);} catch {return '';}};
  const parts = url.pathname.split('/').filter(Boolean).map(decode);
  const index = parts.findIndex(x => ['s','share','t'].includes(x));
  let urlShareId = index >= 0 ? parts[index+1] || '' : '';
  const fragment = url.hash.slice(1), queryIndex = fragment.indexOf('?');
  const fragmentPath = queryIndex < 0 ? fragment : fragment.slice(0,queryIndex);
  if (provider === 'baidu' && parts.join('/') === 'share/init') urlShareId = url.searchParams.get('surl') ? '1'+url.searchParams.get('surl') : '';
  if (provider === 'mcloud' && parts.join('/') === 'shareweb') {
    const route = fragmentPath.split('/').filter(Boolean).map(decode);
    if (route[0] === 'w' && route[1] === 'i') urlShareId = route[2] || '';
  }
  put(share,'shareId',urlShareId || url.searchParams.get('shareId') || url.searchParams.get('shareid') || url.searchParams.get('linkID') || url.searchParams.get('code'));
  const passwordKeys = ['pwd','password','pass_code','receive_code'];
  put(share,'password',passwordKeys.map(k=>url.searchParams.get(k)).find(Boolean) || (fragment.startsWith('pwd=') ? new URL('?'+fragment,sourcePage).searchParams.get('pwd') : ''));
  if (provider === 'baidu') {
    const data = record(window.yunData);
    for (const [key, aliases] of Object.entries({bdstoken:['bdstoken','BDSTOKEN'],sign:['sign','SIGN'],timestamp:['timestamp','TIMESTAMP'],uk:['share_uk','SHARE_UK'],shareId:['shareid','SHARE_ID']})) {
      const v = first(data,aliases) ?? aliases.map(alias=>window.locals?.get?.(alias)).find(v=>v!=null);
      put(key === 'shareId' || key === 'uk' ? share : tokens,key,record(v).value ?? v);
      if (key === 'shareId') put(tokens,'shareid',record(v).value ?? v);
    }
    put(tokens,'jsToken',window.jsToken); put(tokens,'sekey',window.currentSekey || window.cache?.list?.config?.params?.sekey);
    if (share.shareId) put(share,'password',stored(`${share.shareId.replace(/^1(.{22})$/,'$1')}_pwd`) || share.password);
  } else if (provider === 'aliyun') {
    const t = record(stored('token'));
    for (const key of ['access_token','token_type','user_id','default_drive_id','resource_drive_id']) put(tokens,key,t[key]);
    put(tokens,'share_token',stored('share_token')); put(tokens,'access_token',stored('access_token') || tokens.access_token);
  } else if (provider === 'tcloud') put(tokens,'accessToken',stored('accessToken'));
  else if (provider === '123pan') {put(tokens,'authorToken',stored('authorToken'));put(tokens,'LoginUuid',stored('LoginUuid') || stored('loginUuid'));}
  else if (provider === 'xunlei' || provider === 'guangya') {
    try {
      for (let i=0;i<Math.min(localStorage.length,256);i++) {
        const key = localStorage.key(i);
        if (/^credentials_[A-Za-z0-9_-]{1,128}$/.test(key)) {
          const c = record(stored(key));
          for (const field of ['access_token','token_type','user_id']) put(tokens,field,c[field]);
          put(tokens,'client_id',key.slice(12));
        } else if (/^captcha_[A-Za-z0-9_-]{1,128}$/.test(key)) put(tokens,'captcha_token',record(stored(key)).captcha_token);
      }
    } catch { /* Storage may be unavailable on a share page. */ }
    put(tokens,'deviceid',scalar(stored('deviceid')).match(/\w{32}/)?.[0]);
    if (provider === 'guangya') put(share,'password',url.searchParams.get('code'));
  }
  let accountId=tokens.user_id || '';
  if (!accountId) {
    for(const key of ['userInfo','userinfo','user_info','user','accountInfo']) {
      const own=record(stored(key));
      accountId=scalar(own.ucid || own.user_id || own.userId || own.uid || own.userID || own.id,256);
      if(accountId) break;
    }
  }
  if(provider==='baidu' && !accountId) {
    const data=record(window.yunData);accountId=scalar(data.MYUK || data.MY_UK || data.USER_ID,256);
  }
  return {provider,sourcePage,files,tokens,share,accountId};
}
