const enabled = document.getElementById('enabled'),confirm = document.getElementById('confirm'),status = document.getElementById('status');
const config = (await chrome.storage.local.get('downloadCapture')).downloadCapture || {};
enabled.checked = config.enabled === true;confirm.checked = config.confirm !== false;
async function save() {
  if (enabled.checked && !await chrome.permissions.request({permissions:['downloads']})) enabled.checked = false;
  await chrome.storage.local.set({downloadCapture:{enabled:enabled.checked,confirm:confirm.checked}});
  status.textContent = enabled.checked ? '已启用，只处理后续下载。' : '已关闭，浏览器继续处理下载。';
}
enabled.addEventListener('change',save);confirm.addEventListener('change',save);
