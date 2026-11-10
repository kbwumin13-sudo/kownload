const id = Number(new URLSearchParams(location.search).get('id'));
const texts = {waiting:'浏览器已暂停，尚未创建下载器任务。',expired:'确认已过期，尚未提交给下载器。可以继续浏览器下载或取消。',sending:'正在交给下载器…',created:'已在 kownload 创建任务，浏览器下载已取消。',browser:'由浏览器继续下载。',cancelled:'已取消下载，没有创建下载器任务。',accepted:'下载器正在等待确认，浏览器保持暂停；请到 kownload 确认。',uncertain:'尚未确认下载器是否已接收。浏览器保持暂停，请先到 kownload 检查，避免重复下载。'};
async function update(action='status') {
  document.querySelectorAll('button').forEach(b => b.disabled=true);
  const reply = await chrome.runtime.sendMessage({type:'DOWNLOAD_CAPTURE',id,action});
  const job = reply?.job;
  document.getElementById('name').textContent = job?.name || '';
  document.getElementById('status').textContent = job ? texts[job.state] || '请检查下载状态。' : '此确认已结束，请查看浏览器下载列表。';
  document.querySelectorAll('button').forEach(b => b.disabled=job?.state !== 'waiting' && !(job?.state === 'expired' && b.id !== 'submit'));
}
for (const action of ['submit','resume','cancel']) document.getElementById(action).addEventListener('click',() => update(action));
update();
