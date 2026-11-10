const status = document.getElementById('status');
async function request(message) {
  const response = await chrome.runtime.sendMessage(message);
  if (!response?.ok) throw new Error(response?.error || '无法连接 kownload，请打开 App 后重试');
  return response.data;
}
async function open(message) {
  try { await request(message); window.close(); }
  catch (error) { status.textContent = error.message; }
}
document.getElementById('resources').addEventListener('click',() => open({type:'APP_MEDIA_OPEN'}));
document.getElementById('settings').addEventListener('click',() => open({type:'APP_BRIDGE',action:'openSettings'}));
const permission = document.getElementById('permission');
permission.addEventListener('click',async () => {
  // Optional browser permissions require this browser user gesture; settings
  // themselves remain in the App and this does not change the capture mode.
  const granted = await chrome.permissions.request({permissions:['downloads']});
  permission.hidden = granted;
  status.textContent = granted ? '普通下载已获授权，接管方式可在 App 设置中选择。' : '未授权，仍可查看网页媒体资源。';
  await request({type:'APP_BRIDGE',action:'settings'}).catch(() => {});
});
void chrome.permissions.contains({permissions:['downloads']}).then(granted => {
  permission.hidden = granted;
  if (granted) void open({type:'APP_MEDIA_OPEN'});
  else status.textContent = '在 App 中查看媒体资源，或授权普通下载接管。';
});
