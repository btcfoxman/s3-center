'use strict';

const parts = location.pathname.split('/').filter(Boolean);
const nonce = parts[0] === 'start' ? parts[1] : '';
const status = document.getElementById('bridge-status');

chrome.runtime.sendMessage({
  type: 'bootstrap',
  origin: location.origin,
  nonce,
  webdriver: navigator.webdriver === true,
}, response => {
  if (chrome.runtime.lastError) {
    showError(`本机安全助手未能启动：${chrome.runtime.lastError.message}`);
    return;
  }
  if (!response?.ok) {
    showError(response?.error || '本机安全助手初始化失败。');
    return;
  }
  if (status) status.textContent = '出口验证成功，正在进入 Cloudflare 官方页面…';
});

function showError(message) {
  if (!status) return;
  status.textContent = `${message} 请关闭窗口，从 S3 Center 重新下载并安装最新版 Connector。`;
  status.style.background = '#3b1717';
  status.style.color = '#ffb4ab';
}
