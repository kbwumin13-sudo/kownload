const id = Number(new URLSearchParams(location.search).get('id'));
const texts = {waiting:'浏览器已暂停，尚未创建下载器任务。',expired:'确认已过期，尚未提交给下载器。可以继续浏览器下载或取消。',sending:'正在交给下载器…',created:'已在 kownload 创建任务，浏览器下载已取消。',browser:'由浏览器继续下载。',cancelled:'已取消下载，没有创建下载器任务。',accepted:'下载器正在等待确认，浏览器保持暂停；请到 kownload 确认。',uncertain:'尚未确认下载器是否已接收。浏览器保持暂停，请先到 kownload 检查，避免重复下载。'};
let checking=false,closed=false,timer;
async function update(action='status') {
  if (checking) return;
  checking=true;clearTimeout(timer);
  document.querySelectorAll('button').forEach(b => b.disabled=true);
  try {
    const reply = await chrome.runtime.sendMessage({type:'DOWNLOAD_CAPTURE',id,action});
    if (!reply?.ok) throw new Error('unavailable');
    const job = reply.job,current=reply.browser;
    const state=current?.state === 'complete' && (!job || ['waiting','expired','browser'].includes(job.state)) ? 'browser' : job?.state;
    document.getElementById('name').textContent = job?.name || '';
    document.getElementById('status').textContent = state === 'browser' && current?.state === 'complete' ? '浏览器已完成下载，没有新建下载器任务。' : state === 'browser' && current?.state === 'interrupted' ? '浏览器下载已中断，未新建下载器任务。请在浏览器下载列表重试。' : job ? texts[state] || '请检查下载状态。' : '此确认已结束，请查看浏览器下载列表。';
    document.querySelectorAll('button').forEach(b => b.disabled=state !== 'waiting' && !(state === 'expired' && b.id !== 'submit'));
    // Only an unresolved, open confirmation maintains communication. Besides
    // refreshing the UI, these messages preserve its pending filename callback
    // across the MV3 worker's otherwise idle 30-second lifetime.
    if (!closed && job && ['waiting','sending','accepted','uncertain'].includes(state)) timer=setTimeout(()=>update(),10000);
  } catch {
    document.getElementById('status').textContent='暂时无法读取下载状态，请保持此页并检查浏览器下载列表。';
    if (!closed) timer=setTimeout(()=>update(),10000);
  } finally { checking=false; }
}
for (const action of ['submit','resume','cancel']) document.getElementById(action).addEventListener('click',() => update(action));
addEventListener('pagehide',()=>{closed=true;clearTimeout(timer);});
addEventListener('pageshow',()=>{closed=false;update();});
update();
