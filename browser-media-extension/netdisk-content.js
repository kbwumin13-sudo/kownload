// Isolated-world, top-frame bootstrap. No page message listener or auth collection.
(() => {
  if (window !== window.top) return;
  let busy=false,stopped=false,host,button,status,lastProbe=0,probeNotice='';
  const send = type => chrome.runtime.sendMessage({type});
  async function probe() {
    if (stopped || busy || document.visibilityState==='hidden' || Date.now()-lastProbe<14000) return;
    lastProbe=Date.now();
    try {
      const result=await send('NETDISK_PROBE');
      probeNotice=result?.error || '';
      if(status) status.textContent=probeNotice;
      // A redirect/SPA may be too early to claim on its first probe. Keep the
      // already bounded App handoff alive until it syncs or reaches its limit.
      if(result?.state==='synced' || result?.state==='idle' && !handoff()){clearInterval(timer);timer=null;}
    } catch { /* App/extension may be unavailable. */ }
  }
  function mount() {
    if (stopped || !document.body || host?.isConnected) return;
    host=document.createElement('div');
    host.style.cssText='position:fixed;right:20px;bottom:28px;z-index:2147483647';
    const root=host.attachShadow({mode:'closed'});
    const style=document.createElement('style');
    style.textContent='button{background:#16875d;color:white;border:0;border-radius:8px;padding:10px 18px;font:600 14px system-ui;cursor:pointer;box-shadow:0 3px 14px #0003}button:disabled{opacity:.65;cursor:wait}p{max-width:260px;margin:8px 0 0;padding:8px;background:#fff;color:#333;border-radius:6px;font:13px system-ui;box-shadow:0 2px 10px #0002}p:empty{display:none}';
    button=document.createElement('button');button.type='button';button.textContent='kownload 下载';
    status=document.createElement('p');status.setAttribute('role','status');status.textContent=probeNotice;
    button.addEventListener('click',async event=>{
      // Page-generated events must not wake the app or collect authentication.
      if (!event.isTrusted || busy || stopped) return;
      busy=true;button.disabled=true;status.textContent='正在同步选中文件…';
      try {
        const result=await send('NETDISK_CLICK');
        status.textContent=result?.error || (result?.state==='confirm' ? `已提交 ${result.count} 项，请在 kownload 确认` : '正在同步，请稍后重试');
      } catch {status.textContent='kownload 连接失败，请打开应用后重试';}
      finally {busy=false;button.disabled=false;}
    });
    root.append(style,button,status);document.body.append(host);
  }
  const observer=new MutationObserver(mount);
  observer.observe(document,{childList:true,subtree:true});
  document.addEventListener('DOMContentLoaded',()=>{mount();resumeHandoff();void probe();},{once:true});
  document.addEventListener('visibilitychange',()=>{resumeHandoff();void probe();});
  // Recheck only a bounded App-initiated handoff. Ordinary drive browsing
  // performs one passive probe, rather than spawning a native host forever.
  const handoff=()=>{
    try {
      const u=new URL(location.href);
      if(u.searchParams.get('gopeed_request')) return true;
      // Match only the observed mobile personal-home redirect's hash query.
      return ['http:','https:'].includes(u.protocol) && !u.username && !u.password && !u.port &&
        ['yun.139.com','caiyun.139.com'].includes(u.hostname) && u.pathname==='/w/' &&
        u.hash.includes('?') &&
        !!new URLSearchParams(u.hash.slice(u.hash.indexOf('?')+1)).get('gopeed_request');
    } catch {return false;}
  };
  const handoffStarted=Date.now();
  const repeatProbe=()=>{if(Date.now()-handoffStarted>600000){clearInterval(timer);timer=null;return}void probe()};
  let timer=handoff()?setInterval(repeatProbe,15000):null;
  function resumeHandoff() {
    if(!stopped && !timer && handoff() && Date.now()-handoffStarted<=600000) timer=setInterval(repeatProbe,15000);
  }
  // Mobile's SPA installs its /index handoff route after document_start.
  window.addEventListener('hashchange',()=>{resumeHandoff();void probe();});
  window.addEventListener('popstate',()=>{resumeHandoff();void probe();});
  window.addEventListener('pagehide',()=>{stopped=true;clearInterval(timer);observer.disconnect();});
  window.addEventListener('pageshow',event=>{
    if (!event.persisted) return;
    stopped=false;lastProbe=0;
    observer.observe(document,{childList:true,subtree:true});
    timer=handoff()?setInterval(repeatProbe,15000):null;mount();void probe();
  });
  mount();void probe();
})();
