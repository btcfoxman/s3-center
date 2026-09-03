'use strict';

const CONFIG_KEY = 's3CenterConnectorConfig';
let initialization;

chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message?.type === 'bootstrap') {
    if (!initialization) initialization = initialize(message, sender);
    initialization.then(() => sendResponse({ok: true})).catch(error => sendResponse({ok: false, error: safeMessage(error)}));
    return true;
  }
  if (message?.type === 'submit_token') {
    submitToken(message, sender).then(() => sendResponse({ok: true})).catch(error => sendResponse({ok: false, error: safeMessage(error)}));
    return true;
  }
  if (message?.type === 'heartbeat') {
    getConfig().then(config => sendEvent(config, {type: 'heartbeat'})).catch(() => {});
  }
  return false;
});

async function initialize(message, sender) {
  const origin = String(message.origin || '');
  const nonce = String(message.nonce || '');
  if (!/^http:\/\/127\.0\.0\.1:\d+$/.test(origin) || !/^[A-Za-z0-9_-]{40,64}$/.test(nonce)) {
    throw new Error('本机安全通道地址无效。');
  }
  if (message.webdriver) {
    throw new Error('浏览器仍处于自动化控制状态，请重新安装最新版 Connector。');
  }
  const response = await timedFetch(`${origin}/config/${encodeURIComponent(nonce)}`);
  if (!response.ok) throw new Error(`读取本机安全配置失败（HTTP ${response.status}）。`);
  const config = await response.json();
  validateConfig(config, origin);
  await chrome.storage.session.set({[CONFIG_KEY]: config});

  const exitIP = await verifyEgress();
  config.exit_ip = exitIP;
  await chrome.storage.session.set({[CONFIG_KEY]: config});
  await sendEvent(config, {type: 'proxy_verified', exit_ip: exitIP});
  await sendEvent(config, {type: 'browser_open', exit_ip: exitIP});

  if (!sender.tab?.id) throw new Error('无法定位原生浏览器标签页。');
  await chrome.tabs.update(sender.tab.id, {url: config.authorization_url});
}

function validateConfig(config, origin) {
  if (!config || config.bridge_origin !== origin || !/^[A-Za-z0-9_-]{40,64}$/.test(config.bridge_token || '')) {
    throw new Error('本机安全配置无效。');
  }
  const authorization = new URL(config.authorization_url);
  if (authorization.protocol !== 'https:' || authorization.hostname !== 'dash.cloudflare.com') {
    throw new Error('Cloudflare 目标地址无效。');
  }
  if (typeof config.proxy_enabled !== 'boolean') throw new Error('代理状态无效。');
}

async function verifyEgress() {
  const response = await timedFetch('https://www.cloudflare.com/cdn-cgi/trace');
  if (!response.ok) throw new Error('无法通过所选出口连接 Cloudflare；未回退到直连。');
  const trace = await response.text();
  const match = /^ip=(.+)$/m.exec(trace);
  if (!match) throw new Error('Cloudflare 未返回浏览器出口 IP。');
  return match[1].trim();
}

async function submitToken(message, sender) {
  const token = String(message.token || '').trim();
  if (!/^cfat_[A-Za-z0-9_-]{20,512}$/.test(token)) {
    throw new Error('请粘贴 Cloudflare 创建完成后显示的 Account API Token。');
  }
  const pageURL = new URL(sender.tab?.url || sender.url || '');
  if (pageURL.protocol !== 'https:' || pageURL.hostname !== 'dash.cloudflare.com') {
    throw new Error('Token 只能从 Cloudflare 官方页面导入。');
  }
  const accountMatch = /^\/([a-f0-9]{32})(?:\/|$)/i.exec(pageURL.pathname);
  if (!accountMatch) {
    throw new Error('当前页面尚未进入具体 Cloudflare Account，请先完成登录并创建 Token。');
  }
  const config = await getConfig();
  await sendEvent(config, {
    type: 'bootstrap_token',
    exit_ip: config.exit_ip || '',
    account_id: accountMatch[1].toLowerCase(),
    bootstrap_token: token,
  });
}

async function getConfig() {
  const stored = await chrome.storage.session.get(CONFIG_KEY);
  const config = stored[CONFIG_KEY];
  if (!config) throw new Error('本机 Connector 会话已失效，请从 S3 Center 重新打开。');
  return config;
}

async function sendEvent(config, event) {
  const response = await timedFetch(`${config.bridge_origin}/event`, {
    method: 'POST',
    headers: {'Authorization': `Bearer ${config.bridge_token}`, 'Content-Type': 'application/json'},
    body: JSON.stringify(event),
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(typeof body.error === 'string' ? body.error : `本机安全通道返回 HTTP ${response.status}。`);
}

async function timedFetch(url, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 30_000);
  try {
    return await fetch(url, {...options, cache: 'no-store', signal: controller.signal});
  } finally {
    clearTimeout(timer);
  }
}

function safeMessage(error) {
  return (error instanceof Error ? error.message : String(error)).slice(0, 300);
}
