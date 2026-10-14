(() => {
  const seen = new Map(), observedUrls = new Set(), players = new Map(), imageIds = new WeakMap(), ownButtons = new WeakSet(), roots = new WeakSet();
  const queue = [], queued = new Set();
  const isX = /^(?:www\.|mobile\.)?(?:x|twitter)\.com$/.test(new URL(location.href).hostname);
  let queueHead = 0;
  let enabled = false, scheduled = false, positionScheduled = false, nextId = 0, nextImageId = 0, lastHref = location.href;
  const search = {until:0,count:0,urls:new Map(),timer:null,id:'',playerId:'',suppressDom:false};
  const absolute = value => {
    if (typeof value !== 'string' || !value.trim()) return '';
    try { const url = new URL(value, document.baseURI || location.href); return /^https?:$/.test(url.protocol) ? url.href : ''; }
    catch { return ''; }
  };
  const manifest = value => /\.(?:m3u8|mpd)(?:$|[?#])/i.test(value || '');
  function variants(value) {
    const urls = []; let i = 0;
    value = typeof value === 'string' ? value.slice(0,8192) : '';
    while (i < value.length && urls.length < 16) {
      while (/[\s,]/.test(value[i] || '') && i < value.length) i++;
      const start = i;
      // A comma within a URL is data; only whitespace or trailing commas end it.
      while (i < value.length && !/\s/.test(value[i])) i++;
      const url = absolute(value.slice(start,i).replace(/,+$/,''));
      if (url) urls.push(url);
      while (i < value.length && value[i] !== ',') i++;
      i++;
    }
    return urls;
  }
  function cssUrls(value) {
    if (typeof value !== 'string') return [];
    value = value.slice(0,8192);
    const urls = []; let i = 0, imageSet = 0;
    while (i < value.length && urls.length < 16) {
      const c = value[i];
      if (c === ')' && imageSet) { imageSet--; i++; continue; }
      if (c === '(' && imageSet) { imageSet++; i++; continue; }
      if (c === '"' || c === "'") {
        const quote = c; let raw = '';
        let before = i - 1; while (before >= 0 && /\s/.test(value[before])) before--;
        const imageCandidate = value[before] === ',' || value[before] === '(' && /(?:-webkit-)?image-set\s*$/i.test(value.slice(Math.max(0,before-24),before));
        i++;
        while (i < value.length && value[i] !== quote && raw.length < 2048) {
          if (value[i] === '\\' && i + 1 < value.length) i++;
          raw += value[i++];
        }
        if (value[i] === quote) i++;
        if (imageSet && imageCandidate) { const url = absolute(raw); if (url) urls.push(url); }
        continue;
      }
      const match = value.slice(i).match(/^(?:-webkit-)?image-set\s*\(|^url\s*\(/i);
      if (!match) { i++; continue; }
      const isSet = /image-set/i.test(match[0]); i += match[0].length;
      if (isSet) { imageSet++; continue; }
      while (/\s/.test(value[i] || '') && i < value.length) i++;
      let raw = '', quote = value[i] === '"' || value[i] === "'" ? value[i++] : '';
      while (i < value.length && raw.length < 2048) {
        const ch = value[i++];
        if (ch === '\\' && i < value.length) { raw += value[i++]; continue; }
        if (quote ? ch === quote : ch === ')') break;
        raw += ch;
      }
      if (quote) { while (i < value.length && value[i] !== ')') i++; if (value[i] === ')') i++; }
      const url = absolute(raw.trim()); if (url) urls.push(url);
    }
    return [...new Set(urls)];
  }
  function videoSource(video) {
    const value = video.currentSrc || video.src || video.querySelector('source[src]')?.src || '';
    if (!value.startsWith('blob:')) return absolute(value);
    if (absolute(video.src)) return absolute(video.src);
    for (const node of [video,video.parentElement,video.closest('[data-src], [data-hls], [data-mpd]')]) {
      if (!node) continue;
      for (const name of ['data-hls','data-mpd','data-src','data-url','data-source']) {
        const candidate = absolute(node.getAttribute(name));
        if (manifest(candidate)) return candidate;
      }
    }
    for (const source of video.querySelectorAll('source[src]')) if (manifest(source.src)) return absolute(source.src);
    return '';
  }
  const xPlayerRoot = video => video.closest('[data-testid="videoPlayer"]') || video.closest('[data-testid="videoComponent"]') || video;
  function pageAdapter(video) {
    let url;
    try { url = new URL(location.href); } catch { return ''; }
    const host = url.hostname.toLowerCase();
    if (['x.com','www.x.com','twitter.com','www.twitter.com','mobile.twitter.com'].includes(host)) {
      const article = video.closest('[data-testid="tweet"]');
      if (video.closest('[data-testid="quoteTweet"], [data-testid="quotedTweet"]')) return '';
      if (!article) {
        // SSR has no tweet/widget markers. Never infer a post from a feed or a collection.
        const match = url.pathname.match(/^\/[A-Za-z0-9_]+\/status\/(\d+)(?:\/video\/([1-9]\d*))?\/?$/);
        const videos = document.querySelectorAll('video');
        if (!match || document.querySelectorAll('[data-testid="tweet"]').length ||
            videos.length !== 1 || videos[0] !== video ||
            document.querySelectorAll('[data-testid="quoteTweet"], [data-testid="quotedTweet"], [data-testid="tweetPhoto"]').length) return '';
        const slots = new Set();
        for (const link of document.querySelectorAll('a[href*="/status/"]')) {
          let linked;
          try { linked = new URL(link.href || link.getAttribute('href'),url); } catch { return ''; }
          if (!/^(?:www\.|mobile\.)?(?:x|twitter)\.com$/.test(linked.hostname)) continue;
          const post = linked.pathname.match(/^\/[^/]+\/status\/(\d+)(?:\/(video|photo)\/([1-9]\d*))?\/?$/);
          if (!post || post[1] !== match[1] || post[2] === 'photo') return '';
          if (post[3]) slots.add(post[3]);
        }
        if (slots.size > 1 || (!match[2] && [...slots].some(slot => slot !== '1')) ||
            (match[2] && slots.size && !slots.has(match[2]))) return '';
        url.hostname = 'x.com'; url.protocol = 'https:'; url.search = ''; url.hash = '';
        return url.href;
      }
      const explicit = video.closest('a[href*="/status/"]');
      const links = explicit ? [explicit] : [...article.querySelectorAll('a[href*="/status/"]')].filter(a => a.querySelector('time') && !a.closest('[data-testid="quoteTweet"], [data-testid="quotedTweet"]'));
      const posts = new Map();
      for (const link of links) {
        try {
          const post = new URL(link.href || link.getAttribute('href'),location.href);
          const match = post.pathname.match(/^\/[^/]+\/status\/(\d+)(?:\/video\/(\d+))?\/?$/);
          if (!match || !['x.com','www.x.com','twitter.com','www.twitter.com','mobile.twitter.com'].includes(post.hostname)) continue;
          post.hostname = 'x.com'; post.protocol = 'https:'; post.search = ''; post.hash = '';
          posts.set(match[1],post);
        } catch {}
      }
      if (!posts.size && /^\/[^/]+\/status\/\d+(?:\/video\/\d+)?\/?$/.test(url.pathname)) {
        const cards = document.querySelectorAll?.('[data-testid="tweet"]') || [];
        if (cards.length === 1 && cards[0] === article) { const own = new URL(url.href); own.hostname = 'x.com'; own.search = ''; own.hash = ''; posts.set(own.pathname.match(/\/status\/(\d+)/)[1],own); }
      }
      if (posts.size !== 1) return '';
      const post = [...posts.values()][0];
      const videos = [...article.querySelectorAll('video')].filter(v => v.closest('[data-testid="tweet"]') === article);
      // Logged-in X nests videoComponent inside videoPlayer for a single video.
      // Count the physical player once, using the same root as its download button.
      const widgets = [...new Set([...article.querySelectorAll('[data-testid="videoPlayer"], [data-testid="videoComponent"]')].filter(v => v.closest('[data-testid="tweet"]') === article).map(xPlayerRoot))];
      if (Math.max(videos.length,widgets.length) > 1) {
        // A shared timestamp can contain /video/1 without identifying the clicked slot.
        // Only a media link enclosing this one player proves a multi-video target.
        if (!explicit || !/\/video\/[1-9]\d*\/?$/.test(post.pathname) || explicit.querySelectorAll('video').length > 1 ||
            new Set([...explicit.querySelectorAll('[data-testid="videoPlayer"], [data-testid="videoComponent"]')].map(xPlayerRoot)).size > 1) return '';
      }
      return post.href;
    }

    if (['youtube.com','www.youtube.com','m.youtube.com'].includes(host) && url.pathname === '/watch' && url.searchParams.get('v') && !url.searchParams.has('list')) {
      const player = video.closest('#movie_player, ytd-player');
      if (player && !video.closest('#movie_player.ad-showing, #movie_player.ad-interrupting, ytd-player.ad-showing, ytd-player.ad-interrupting') &&
          !player.classList?.contains('ad-showing') && !player.classList?.contains('ad-interrupting')) return url.href;
    }
    if (['bilibili.com','www.bilibili.com'].includes(host) && /^\/video\/(?:BV[\w]+|av\d+)\/?$/.test(url.pathname)) {
      const player = video.closest('.bpx-player-container, .bpx-player-video-wrap, .bilibili-player, #bilibili-player, #bpx-player-container');
      if (player && !video.closest('.bpx-player-ad, .bpx-player-ad-wrap, .bilibili-player-video-ad') &&
          !['bpx-player-ad-active','bpx-player-ad-showing','bilibili-player-video-ad-active'].some(name => player.classList?.contains(name)) &&
          !player.querySelector?.('.bpx-player-ad-active, .bpx-player-ad-showing, .bilibili-player-video-ad-active')) return url.href;
    }
    return '';
  }
  function target(video) {
    const media = video.localName === 'video' ? video : video.querySelector('video');
    const url = media ? videoSource(media) : '';
    if (url) return {url};
    const page = pageAdapter(media || video);
    if (page) {
      const parsed = new URL(page);
      const expectedMediaId = parsed.hostname.endsWith('youtube.com') ? parsed.searchParams.get('v') : parsed.pathname.match(/^\/video\/(BV[\w]+|av\d+)\/?$/)?.[1] || parsed.pathname.match(/\/status\/(\d+)/)?.[1];
      return {url:page,sourceType:'page',expectedMediaId};
    }
    if ((isX && media) || (media?.currentSrc || media?.src || '').startsWith('blob:') || ['videoPlayer','videoComponent'].includes(video.getAttribute?.('data-testid'))) return {url:''};
    return null;
  }
  function suggestedXPage(video) {
    if (!isX) return null;
    const media = video.localName === 'video' ? video : video.querySelector('video');
    if (!media || media.closest('[data-testid="tweet"], [data-testid="quoteTweet"], [data-testid="quotedTweet"]')) return null;
    const url = new URL(location.href);
    const match = url.pathname.match(/^\/[A-Za-z0-9_]+\/status\/(\d+)(?:\/video\/([1-9]\d*))?\/?$/);
    if (!match || !/^https?:$/.test(url.protocol) || document.querySelectorAll('video').length !== 1 || document.querySelectorAll('video')[0] !== media ||
        document.querySelectorAll('[data-testid="tweet"], [data-testid="quoteTweet"], [data-testid="quotedTweet"], [data-testid="tweetPhoto"]').length) return null;
    const slots = new Set();
    for (const link of document.querySelectorAll('a[href*="/status/"]')) {
      if (link.closest?.('[data-testid="quoteTweet"], [data-testid="quotedTweet"]')) return null;
      let linked;
      try { linked = new URL(link.href || link.getAttribute('href'),url); } catch { return null; }
      if (!/^(?:www\.|mobile\.)?(?:x|twitter)\.com$/.test(linked.hostname)) continue;
      const post = linked.pathname.match(/^\/[^/]+\/status\/(\d+)(?:\/(video|photo)\/([1-9]\d*))?\/?$/);
      if (!post || post[2] === 'photo' || (post[1] !== match[1] && post[2])) return null;
      if (post[1] === match[1] && post[3]) slots.add(post[3]);
    }
    if (slots.size > 1 || (!match[2] && [...slots].some(slot => slot !== '1')) ||
        (match[2] && slots.size && !slots.has(match[2]))) return null;
    url.protocol = 'https:'; url.hostname = 'x.com'; url.search = ''; url.hash = '';
    return {suggestedPage:url.href,expectedMediaId:match[1]};
  }
  function layoutAnchor(video) {
    if (!isX) return video;
    const media = video.localName === 'video' ? video : video.querySelector('video');
    // Keep the player identity/source separate from the visible poster's layout box.
    if (video.localName !== 'video') {
      const rect = video.getBoundingClientRect();
      if (rect.width >= 100 && rect.height >= 60) return video;
    }
    for (let el = video.parentElement, depth = 0; el && depth < 6; el = el.parentElement, depth++) {
      if (['body','html','main','article'].includes(el.localName) || el.getAttribute('data-testid') === 'tweet' || el.getAttribute('role') === 'main') break;
      const videos = el.querySelectorAll('video');
      if (!media || videos.length !== 1 || videos[0] !== media) break;
      const rect = el.getBoundingClientRect();
      if (rect.width >= 100 && rect.height >= 60) return el;
    }
    return video;
  }
  const anchoredPlayers = new WeakMap();
  const intersection = typeof IntersectionObserver === 'function' ? new IntersectionObserver(entries => {
    for (const entry of entries) { const player = anchoredPlayers.get(entry.target); if (player && player.anchor === entry.target) player.visible = entry.isIntersecting; }
    schedulePosition();
  }) : null;
  const resize = typeof ResizeObserver === 'function' ? new ResizeObserver(schedulePosition) : null;
  function schedulePosition() {
    if (positionScheduled) return;
    positionScheduled = true;
    if (window.requestAnimationFrame) window.requestAnimationFrame(position);
    else setTimeout(position,16);
  }
  function position() {
    positionScheduled = false;
    const updates = [];
    for (const [video,player] of players) {
      if (!video.isConnected) {
        intersection?.unobserve(player.anchor); resize?.unobserve(player.anchor); anchoredPlayers.delete(player.anchor);
        player.button.remove(); players.delete(video); continue;
      }
      const anchor = layoutAnchor(video);
      if (anchor !== player.anchor) {
        intersection?.unobserve(player.anchor); resize?.unobserve(player.anchor); anchoredPlayers.delete(player.anchor);
        player.anchor = anchor; player.visible = true; anchoredPlayers.set(anchor,player);
        intersection?.observe(anchor); resize?.observe(anchor);
      }
      const rect = enabled && player.visible ? anchor.getBoundingClientRect() : null;
      const visible = !!rect && rect.width >= 100 && rect.height >= 60 && rect.bottom > 0 && rect.top < innerHeight && rect.right > 0 && rect.left < innerWidth;
      updates.push({player,rect,visible,choice:visible ? target(video) : null});
    }
    for (const {player,rect,visible,choice} of updates) {
      const button = player.button;
      button.style.display = visible ? 'block' : 'none';
      if (!visible) continue;
      button.textContent = choice?.url ? '用 kownload 下载' : choice ? '识别这个视频' : '未识别这个视频';
      button.title = choice?.url ? '只处理此播放器中的视频' : choice ? '打开同一画面中的媒体候选以手动选择' : '请先播放视频';
      button.disabled = !choice || player.opening;
      button.style.top = `${Math.max(4,rect.top + 10)}px`;
      button.style.left = `${Math.max(4,Math.min(innerWidth - 145,rect.right - 145))}px`;
    }
  }
  function makePlayer(video) {
    if (isX) video = xPlayerRoot(video);
    if (players.has(video)) return players.get(video);
    // Hydration may wrap an SSR video in a named widget: retain its one button/id.
    const media = video.localName === 'video' ? video : video.querySelector('video');
    if (isX) for (const [existing,player] of players) {
      if (xPlayerRoot(existing) === video || media && (existing === media || (existing.localName !== 'video' && existing.querySelector('video') === media))) {
        players.delete(existing); players.set(video,player); player.target = video;
        schedulePosition(); return player;
      }
    }
    const button = document.createElement('button');
    const player = {id:`frame-player-${++nextId}`,button,opening:false,visible:true,target:video,anchor:layoutAnchor(video)};
    ownButtons.add(button);
    Object.assign(button.style,{position:'fixed',zIndex:'2147483647',display:'none',padding:'8px 12px',border:'0',borderRadius:'10px',background:'#3eaa77',color:'#fff',font:'13px system-ui',cursor:'pointer',boxShadow:'0 3px 12px #0004'});
    button.addEventListener('click',async event => {
      event.preventDefault(); event.stopPropagation();
      if (!enabled || !player.target.isConnected || player.opening) return;
      const choice = target(player.target);
      if (!choice) { schedulePosition(); return; }
      player.opening = true; button.disabled = true;
      try {
        const suggestion = !choice.url ? suggestedXPage(player.target) : null;
        const response = await chrome.runtime.sendMessage({type:'PANEL',...choice,...suggestion,playerId:player.id,sourcePage:location.href,timeStamp:Date.now()});
        if (!response?.ok) { button.textContent = response?.error || '打开失败，请重试'; button.title = button.textContent; }
      } catch { button.textContent = '扩展已更新，请刷新页面'; }
      finally { player.opening = false; schedulePosition(); }
    });
    players.set(video,player); document.documentElement.append(button);
    anchoredPlayers.set(player.anchor,player);
    intersection?.observe(player.anchor); resize?.observe(player.anchor); schedulePosition();
    return player;
  }
  function addItem(found,value,more={}) {
    const url = absolute(value);
    if (url) found.push({url,title:document.title,...more});
  }
  function inspect(el,found) {
    if (ownButtons.has(el) || el.nodeType !== 1) return;
    if (el.localName === 'video') {
      const player = makePlayer(el);
      addItem(found,videoSource(el),{kind:'video',playerId:player.id,thumbnail:absolute(el.poster)});
      schedulePosition();
    } else if (el.localName === 'audio') addItem(found,el.currentSrc || el.src || el.querySelector('source[src]')?.src,{kind:'audio'});
    else if (el.localName === 'img') {
      const choices = variants(el.srcset), url = absolute(el.currentSrc || el.src) || choices[0];
      if (!imageIds.has(el)) imageIds.set(el,`frame-image-${++nextImageId}`);
      addItem(found,url,{kind:'image',imageId:imageIds.get(el),width:el.naturalWidth || 0,height:el.naturalHeight || 0,thumbnail:url,variants:choices});
    } else if (el.localName === 'source') {
      const choices = variants(el.srcset);
      const parent = el.parentElement?.localName;
      addItem(found,el.src || choices[0],{kind:parent === 'picture' ? 'image' : parent === 'audio' ? 'audio' : 'video',mime:el.type || '',variants:choices});
      if (el.parentElement?.localName === 'video') enqueue(el.parentElement);
    } else if (el.srcset) {
      const choices = variants(el.srcset);
      addItem(found,choices[0],{kind:'image',variants:choices});
    }
    if (isX && ['videoPlayer','videoComponent'].includes(el.getAttribute?.('data-testid'))) makePlayer(el);
    const background = getComputedStyle(el).backgroundImage || '';
    const backgrounds = cssUrls(background);
    if (el.clientWidth > 0 && el.clientHeight > 0 && pseudoBudget > 0) {
      for (const pseudo of ['::before','::after']) {
        if (!pseudoBudget--) break;
        backgrounds.push(...cssUrls(getComputedStyle(el,pseudo).backgroundImage || ''));
      }
    }
    for (const url of new Set(backgrounds)) {
      addItem(found,url,{kind:'image',thumbnail:url,width:el.clientWidth || 0,height:el.clientHeight || 0});
    }
    if (el.shadowRoot?.mode === 'open') observeRoot(el.shadowRoot);
  }
  function enqueue(node) {
    if (!enabled || !node || queued.has(node) || ownButtons.has(node)) return;
    queued.add(node); queue.push(node); scheduleScan();
  }
  function observeRoot(root) {
    if (roots.has(root)) return;
    roots.add(root);
    observer.observe(root,{subtree:true,childList:true,attributes:true,attributeFilter:['src','srcset','style','class','poster','data-src','data-hls','data-mpd','data-url','data-source','href','rel','media']});
    for (const child of root.children || []) enqueue(child);
  }
  function scheduleScan() {
    if (scheduled || !enabled) return;
    scheduled = true; setTimeout(scanBatch,0);
  }
  function scanBatch() {
    scheduled = false;
    if (!enabled) { queue.length = 0; queueHead = 0; queued.clear(); return; }
    const found = []; let count = 0;
    pseudoBudget = 12;
    while (queueHead < queue.length && count < 200) {
      const el = queue[queueHead++]; queued.delete(el);
      if (!el || ownButtons.has(el)) continue;
      if (el.nodeType === 1) {
        if (!el.isConnected) continue;
        inspect(el,found); count++;
      }
      for (const child of el.children || []) enqueue(child);
    }
    const newlyObserved = new Set();
    const changed = found.filter(entry => {
      const key = entry.playerId ? `video:${entry.playerId}:${entry.url}` : entry.url;
      const signature = `${entry.width || ''}:${entry.height || ''}:${entry.thumbnail || ''}:${(entry.variants || []).join('|')}`;
      if (seen.get(key) === signature) return false;
      seen.set(key,signature);
      if (!observedUrls.has(entry.url)) newlyObserved.add(entry.url);
      observedUrls.add(entry.url);
      while (seen.size > 4000) seen.delete(seen.keys().next().value);
      while (observedUrls.size > 4000) observedUrls.delete(observedUrls.values().next().value);
      return true;
    });
    const active = search.id && Date.now() < search.until;
    const recent = active && !search.suppressDom ? changed.filter(item => newlyObserved.has(item.url)).map(({playerId,...item}) => item) : [];
    const previous = active && !search.suppressDom ? changed.filter(item => !newlyObserved.has(item.url)) : changed;
    for (const [items,searchId] of [[previous,''],[recent,search.id]]) if (items.length)
      chrome.runtime.sendMessage({type:'ADD_DOM',items,timeStamp:Date.now(),sourcePage:location.href,...(searchId ? {searchId} : {})}).catch(() => {});
    if (queueHead < queue.length) scheduleScan();
    else { queue.length = 0; queueHead = 0; queued.clear(); search.suppressDom = false; }
    schedulePosition();
  }
  let pseudoBudget = 0;
  function fullRescan() { seen.clear(); if (search.id) search.suppressDom = true; if (document.documentElement) enqueue(document.documentElement); }
  const observer = new MutationObserver(records => {
    for (const record of records) {
      if (ownButtons.has(record.target)) continue;
      if (record.target.localName === 'style' || record.target.localName === 'link') { fullRescan(); continue; }
      if (record.type === 'childList') {
        for (const node of record.addedNodes) {
          if (ownButtons.has(node)) continue;
          if (node.localName === 'style' || node.localName === 'link') { fullRescan(); continue; }
          enqueue(node);
        }
        if (record.removedNodes.length) schedulePosition();
      } else enqueue(record.target);
    }
  });
  observer.observe(document,{subtree:true,childList:true,attributes:true,attributeFilter:['src','srcset','style','class','poster','data-src','data-hls','data-mpd','data-url','data-source','href','rel','media']});
  document.addEventListener('load',event => enqueue(event.target),true);
  document.addEventListener('play',event => { enqueue(event.target); schedulePosition(); },true);
  document.addEventListener('loadedmetadata',event => { enqueue(event.target); schedulePosition(); },true);
  window.addEventListener('scroll',schedulePosition,true);
  window.addEventListener('resize',schedulePosition);
  window.addEventListener('pageshow',fullRescan);
  window.addEventListener('popstate',fullRescan);
  window.addEventListener('message',event => {
    if (event.source !== window || !enabled || Date.now() >= search.until) return;
    const data = event.data;
    if (data?.searchId !== search.id) return;
    if (!data || data.type !== 'GOPEED_MEDIA_CANDIDATE' || typeof data.url !== 'string' || data.url.length > 2048 || !/^https?:\/\//i.test(data.url)) return;
    const url = absolute(data.url);
    const mime = typeof data.mime === 'string' ? data.mime.slice(0,128).toLowerCase() : '';
    if (url && /(?:^|\/)(?:key|keys|license|drm)(?:[\/._-]|$)|\.key$/i.test(new URL(url).pathname)) return;
    if (!url || !(/\.(?:m3u8|mpd|mp4|m4v|mov|webm|mp3|m4a|aac|flac|ogg)(?:$|[?#])/i.test(url) || /^(?:video\/|audio\/|application\/(?:vnd\.apple\.mpegurl|x-mpegurl|dash\+xml))/.test(mime) || data.manifest === true)) return;
    const relatedUrls = [];
    if (Array.isArray(data.relatedUrls)) for (const raw of data.relatedUrls.slice(0,16)) {
      if (typeof raw !== 'string' || raw.length > 2048 || !/^https?:\/\//i.test(raw)) continue;
      const related = absolute(raw);
      if (related && related !== url && !relatedUrls.includes(related) && !/(?:^|\/)(?:key|keys|license|drm)(?:[\/._-]|$)|\.key$/i.test(new URL(related).pathname)) relatedUrls.push(related);
    }
    const contextUrls = [];
    if (Array.isArray(data.contextUrls)) for (const raw of data.contextUrls.slice(0,64)) {
      const context = typeof raw === 'string' && raw.length <= 2048 ? absolute(raw) : '';
      if (context && !contextUrls.includes(context)) contextUrls.push(context);
    }
    const media = [...players.values()].find(player => player.id === search.playerId)?.target;
    const video = media?.localName === 'video' ? media : media?.querySelector?.('video');
    const exact = typeof video?.currentSrc === 'string' && video.currentSrc.length > 0 && typeof data.mediaObjectUrl === 'string' &&
      data.mediaObjectUrl === video?.currentSrc;
    const previous = search.urls.get(url);
    if (search.urls.has(url) && (!exact || previous === data.mediaObjectUrl)) return;
    if (!search.urls.has(url)) { if (search.count >= 64) return; search.count++; }
    search.urls.set(url,exact ? data.mediaObjectUrl : '');
    chrome.runtime.sendMessage({type:'ADD_DOM',searchId:search.id,items:[{url,mime,manifest:data.manifest === true,mediaRequired:data.mediaRequired === true,protocolHint:data.protocolHint || '',kind:mime.startsWith('audio/') ? 'audio' : 'video',title:document.title,relatedUrls,contextUrls,...(exact ? {playerId:search.playerId,exactAssociation:true} : {})}],timeStamp:Date.now(),sourcePage:location.href}).catch(() => {});
  });
  setInterval(() => { if (lastHref !== location.href) { lastHref = location.href; fullRescan(); } },1000);
  chrome.runtime.onMessage.addListener((message,sender,sendResponse) => {
    if (message.type === 'SITE_OFF') { enabled = false; search.until = 0; search.id = ''; clearTimeout(search.timer); queue.length = 0; queueHead = 0; queued.clear(); schedulePosition(); }
    if (message.type === 'RESCAN') { enabled = true; fullRescan(); }
    if (message.type === 'SEARCH_START' && enabled && typeof message.searchId === 'string' && message.searchId.length > 0 && message.searchId.length <= 128 && [...players.values()].some(player => player.id === message.playerId)) {
      clearTimeout(search.timer);
      search.until = Date.now() + 60000;
      search.id = message.searchId; search.playerId = message.playerId;
      search.suppressDom = queueHead < queue.length;
      search.count = 0;
      search.urls.clear();
      search.timer = setTimeout(() => { search.until = 0; search.id = ''; search.urls.clear(); },60000);
      sendResponse?.({started:true,searchId:search.id});
    } else if (message.type === 'SEARCH_START') {
      sendResponse?.({started:false});
    }
  });
  chrome.runtime.sendMessage({type:'ENABLED'}).then(response => {
    enabled = response?.ok && response.data === true;
    if (enabled) fullRescan();
  }).catch(() => {});
})();
