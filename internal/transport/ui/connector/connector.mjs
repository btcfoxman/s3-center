import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';
import { chromium } from 'playwright-core';

const PROFILE_PATTERN = /^cf-[a-f0-9]{16}$/;
const IDENTITY_PATTERN = /^cfi-[a-f0-9]{24}$/;
const REQUEST_TIMEOUT_MS = 30_000;

let payload;
let lockPath;
let lockHandle;

try {
  const ticketURL = parseLaunchArgument(process.argv[2]);
  payload = await fetchJSON(ticketURL);
  validatePayload(payload);

  const localRoot = process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local');
  const profileRoot = path.join(localRoot, 'S3Center', 'Profiles', payload.profile_id);
  fs.mkdirSync(profileRoot, {recursive: true});
  assertProfileBinding(profileRoot, payload);
  ({lockPath, lockHandle} = acquireProfileLock(profileRoot));

  const executablePath = locateBrowser();
  const proxy = playwrightProxy(payload.proxy_url);
  const context = await chromium.launchPersistentContext(profileRoot, {
    executablePath,
    headless: false,
    proxy,
    viewport: null,
    locale: 'zh-CN',
    args: ['--start-maximized'],
  });

  context.on('close', releaseProfileLock);
  const page = context.pages()[0] || await context.newPage();
  const exitIP = await verifyBrowserEgress(page);
  await report('proxy_verified', exitIP);

  let bootstrapSubmitted = false;
  await page.exposeBinding('__s3CenterImportBootstrap', async (source, rawToken) => {
    if (bootstrapSubmitted) throw new Error('This bootstrap token has already been submitted.');
    const token = validateBootstrapToken(rawToken);
    const accountID = accountIDFromDashboard(source.page.url());
    await report('bootstrap_token', exitIP, '', token, accountID);
    bootstrapSubmitted = true;
    return {ok: true};
  });
  page.on('domcontentloaded', () => { void installImportPanel(page); });
  await page.goto(payload.authorization_url, {waitUntil: 'domcontentloaded', timeout: REQUEST_TIMEOUT_MS});
  await installImportPanel(page);
  await report('browser_open', exitIP);
  const panelTimer = setInterval(() => { void installImportPanel(page); }, 2_000);
  context.on('close', () => clearInterval(panelTimer));
  await context.waitForEvent('close');
} catch (error) {
  const message = safeError(error);
  if (payload?.report_url && payload?.report_token) {
    try { await report('', '', message); } catch { /* The original error is more useful. */ }
  }
  console.error(`S3 Center Connector: ${message}`);
  releaseProfileLock();
  process.exitCode = 1;
}

function parseLaunchArgument(raw) {
  if (!raw) throw new Error('Missing s3center:// launch URL. Start the connector from S3 Center.');
  const launch = new URL(raw);
  if (launch.protocol !== 's3center:' || launch.hostname !== 'cloudflare-login') {
    throw new Error('Unsupported connector request.');
  }
  const rawTicketURL = launch.searchParams.get('ticket_url');
  if (!rawTicketURL) throw new Error('Connector ticket is missing.');
  const ticketURL = new URL(rawTicketURL);
  const loopback = ticketURL.hostname === 'localhost' || ticketURL.hostname === '127.0.0.1' || ticketURL.hostname === '::1';
  if (ticketURL.protocol !== 'https:' && !(loopback && ticketURL.protocol === 'http:')) {
    throw new Error('Connector tickets must use HTTPS.');
  }
  return ticketURL;
}

async function fetchJSON(target) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    const response = await fetch(target, {headers: {Accept: 'application/json'}, cache: 'no-store', signal: controller.signal});
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || `S3 Center returned HTTP ${response.status}.`);
    return body;
  } finally {
    clearTimeout(timer);
  }
}

function validatePayload(value) {
  if (!value || !PROFILE_PATTERN.test(value.profile_id || '') || !IDENTITY_PATTERN.test(value.identity_id || '')) {
    throw new Error('S3 Center returned an invalid profile identity.');
  }
  for (const field of ['authorization_url', 'report_url', 'report_token']) {
    if (!value[field]) throw new Error(`S3 Center response is missing ${field}.`);
  }
  const authorization = new URL(value.authorization_url);
  if (authorization.protocol !== 'https:' || authorization.hostname !== 'dash.cloudflare.com') {
    throw new Error('Refusing an unexpected Cloudflare authorization destination.');
  }
  const report = new URL(value.report_url);
  if (report.protocol !== 'https:' && !['localhost', '127.0.0.1', '::1'].includes(report.hostname)) {
    throw new Error('Connector report destination must use HTTPS.');
  }
  if (value.proxy_url) parseProxy(value.proxy_url);
}

function assertProfileBinding(profileRoot, value) {
  const metadataPath = path.join(profileRoot, 's3-center-profile.json');
  const expected = {
    identity_id: value.identity_id,
    profile_id: value.profile_id,
    proxy_endpoint: publicProxyEndpoint(value.proxy_url),
  };
  if (fs.existsSync(metadataPath)) {
    const current = JSON.parse(fs.readFileSync(metadataPath, 'utf8'));
    if (current.identity_id !== expected.identity_id || current.profile_id !== expected.profile_id || current.proxy_endpoint !== expected.proxy_endpoint) {
      throw new Error('This browser profile is bound to another Cloudflare identity or proxy. Reauthorize from S3 Center to create a fresh profile.');
    }
    return;
  }
  fs.writeFileSync(metadataPath, `${JSON.stringify(expected, null, 2)}\n`, {encoding: 'utf8', mode: 0o600});
}

function acquireProfileLock(profileRoot) {
  const target = path.join(profileRoot, '.s3-center-connector.lock');
  try {
    const handle = fs.openSync(target, 'wx', 0o600);
    fs.writeFileSync(handle, String(process.pid));
    return {lockPath: target, lockHandle: handle};
  } catch (error) {
    if (error.code !== 'EEXIST') throw error;
    const pid = Number.parseInt(fs.readFileSync(target, 'utf8'), 10);
    if (Number.isInteger(pid) && !processIsAlive(pid)) {
      fs.unlinkSync(target);
      return acquireProfileLock(profileRoot);
    }
    throw new Error('This Cloudflare profile is already open. Close its browser window and try again.');
  }
}

function processIsAlive(pid) {
  try { process.kill(pid, 0); return true; } catch { return false; }
}

function releaseProfileLock() {
  if (lockHandle !== undefined) {
    try { fs.closeSync(lockHandle); } catch { /* already closed */ }
    lockHandle = undefined;
  }
  if (lockPath) {
    try { fs.unlinkSync(lockPath); } catch { /* already removed */ }
    lockPath = undefined;
  }
}

function locateBrowser() {
  const candidates = [
    process.env.S3CENTER_BROWSER_PATH,
    process.env.PROGRAMFILES && path.join(process.env.PROGRAMFILES, 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
    process.env['PROGRAMFILES(X86)'] && path.join(process.env['PROGRAMFILES(X86)'], 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
    process.env.LOCALAPPDATA && path.join(process.env.LOCALAPPDATA, 'Microsoft', 'Edge', 'Application', 'msedge.exe'),
    process.env.PROGRAMFILES && path.join(process.env.PROGRAMFILES, 'Google', 'Chrome', 'Application', 'chrome.exe'),
    process.env['PROGRAMFILES(X86)'] && path.join(process.env['PROGRAMFILES(X86)'], 'Google', 'Chrome', 'Application', 'chrome.exe'),
    process.env.LOCALAPPDATA && path.join(process.env.LOCALAPPDATA, 'Google', 'Chrome', 'Application', 'chrome.exe'),
  ].filter(Boolean);
  const found = candidates.find(candidate => fs.existsSync(candidate));
  if (!found) throw new Error('Microsoft Edge or Google Chrome was not found. Set S3CENTER_BROWSER_PATH to the browser executable.');
  return found;
}

function parseProxy(raw) {
  const proxy = new URL(raw.includes('://') ? raw : `http://${raw}`);
  if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(proxy.protocol) || !proxy.hostname) {
    throw new Error('Proxy must use HTTP, HTTPS, SOCKS5, or SOCKS5H.');
  }
  return proxy;
}

function playwrightProxy(raw) {
  if (!raw) return undefined;
  const proxy = parseProxy(raw);
  const protocol = proxy.protocol === 'socks5h:' ? 'socks5:' : proxy.protocol;
  const config = {server: `${protocol}//${proxy.host}`};
  if (proxy.username) config.username = decodeURIComponent(proxy.username);
  if (proxy.password) config.password = decodeURIComponent(proxy.password);
  return config;
}

function publicProxyEndpoint(raw) {
  if (!raw) return 'direct';
  const proxy = parseProxy(raw);
  return `${proxy.protocol}//${proxy.host}`;
}

async function verifyBrowserEgress(page) {
  const response = await page.goto('https://www.cloudflare.com/cdn-cgi/trace', {waitUntil: 'domcontentloaded', timeout: REQUEST_TIMEOUT_MS});
  if (!response || !response.ok()) throw new Error('Proxy egress check failed; the isolated browser was closed without falling back to a direct connection.');
  const trace = await page.locator('body').innerText({timeout: 10_000});
  const match = /^ip=(.+)$/m.exec(trace);
  if (!match) throw new Error('Cloudflare did not return an exit IP; authorization was stopped.');
  return match[1].trim();
}

function validateBootstrapToken(raw) {
  const token = String(raw || '').trim();
  if (token.length < 20 || token.length > 512 || /\s/.test(token)) {
    throw new Error('Paste the API token value displayed after Cloudflare creates the temporary token.');
  }
  if (token.startsWith('cfoat_')) {
    throw new Error('This is an OAuth access token. Use the API token created by the guided Cloudflare page.');
  }
  if (!token.startsWith('cfat_')) {
    throw new Error('Only the Account API Token created by the guided Cloudflare page can be imported.');
  }
  return token;
}

function accountIDFromDashboard(rawURL) {
  const current = new URL(rawURL);
  if (current.protocol !== 'https:' || current.hostname !== 'dash.cloudflare.com') {
    throw new Error('The token can only be imported from the Cloudflare dashboard.');
  }
  const match = /^\/([a-f0-9]{32})(?:\/|$)/i.exec(current.pathname);
  if (!match) {
    throw new Error('Cloudflare account context is unavailable. Finish creating the Account API Token on the guided page first.');
  }
  return match[1].toLowerCase();
}

async function installImportPanel(page) {
  if (page.isClosed()) return;
  const current = new URL(page.url());
  if (current.protocol !== 'https:' || current.hostname !== 'dash.cloudflare.com') return;
  await page.evaluate(() => {
    if (document.getElementById('s3-center-import-host')) return;
    const host = document.createElement('div');
    host.id = 's3-center-import-host';
    host.style.cssText = 'all:initial;position:fixed;right:18px;bottom:18px;z-index:2147483647';
    const root = host.attachShadow({mode: 'open'});
    root.innerHTML = `
      <style>
        *{box-sizing:border-box} .panel{width:min(390px,calc(100vw - 36px));border:1px solid #d7dce2;border-radius:16px;background:#fff;color:#1f2937;box-shadow:0 20px 55px #10182838;font:13px/1.5 Inter,system-ui,sans-serif;overflow:hidden}
        .head{display:flex;gap:12px;align-items:center;padding:15px 17px;background:linear-gradient(135deg,#fff7ed,#f0fdf4);border-bottom:1px solid #eceff3}.mark{display:grid;place-items:center;width:38px;height:38px;border-radius:11px;background:#f48120;color:#fff;font-weight:800}.title{font-size:14px;font-weight:750}.sub{color:#667085;font-size:12px}
        .body{padding:15px 17px}.steps{margin:0 0 12px;padding-left:19px;color:#475467}.steps b{color:#1f2937}.field{display:flex;gap:8px}input{min-width:0;flex:1;border:1px solid #cfd6df;border-radius:9px;padding:10px 11px;font:12px ui-monospace,monospace;color:#101828;background:#fff}input:focus{outline:2px solid #f9b77d;border-color:#f48120}
        button{border:0;border-radius:9px;padding:10px 12px;background:#f48120;color:#fff;font:700 12px system-ui;cursor:pointer}button.secondary{margin-top:8px;width:100%;background:#eef2f6;color:#344054}button:disabled{opacity:.55;cursor:wait}.status{min-height:20px;margin-top:9px;color:#667085;font-size:12px}.status.ok{color:#087a55;font-weight:650}.status.bad{color:#b42318}.note{margin:10px 0 0;color:#98a2b3;font-size:11px}
      </style>
      <section class="panel" aria-label="S3 Center Cloudflare connector">
        <div class="head"><div class="mark">S3</div><div><div class="title">S3 Center 快速接入</div><div class="sub">当前登录与后续 API 固定使用同一代理出口</div></div></div>
        <div class="body">
          <ol class="steps"><li>在 Cloudflare 中确认预填权限并创建临时 Token</li><li>复制创建结果中的 <b>API Token</b>，粘贴到下方</li></ol>
          <form id="import-form"><div class="field"><input id="token" type="password" autocomplete="off" spellcheck="false" placeholder="粘贴一次性 API Token" aria-label="Cloudflare API Token"><button id="submit" type="submit">安全导入</button></div></form>
          <button id="scan" class="secondary" type="button">自动读取当前页面的新格式 Token</button>
          <div id="status" class="status" role="status"></div>
          <p class="note">引导 Token 仅在内存中使用；桶级凭据创建后将自动撤销。若放弃接入，请在 Cloudflare 手动删除该临时 Token。登录 Cookie 不会离开此电脑。</p>
        </div>
      </section>`;
    document.documentElement.appendChild(host);
    const form = root.getElementById('import-form');
    const input = root.getElementById('token');
    const submit = root.getElementById('submit');
    const scan = root.getElementById('scan');
    const status = root.getElementById('status');

    const send = async () => {
      status.className = 'status';
      status.textContent = '正在经安全通道验证并发现 R2 Bucket…';
      submit.disabled = true;
      scan.disabled = true;
      try {
        await window.__s3CenterImportBootstrap(input.value);
        input.value = '';
        status.className = 'status ok';
        status.textContent = '导入成功。请返回 S3 Center 选择要接入的 Bucket。';
      } catch (error) {
        status.className = 'status bad';
        status.textContent = error instanceof Error ? error.message : String(error);
        submit.disabled = false;
        scan.disabled = false;
      }
    };
    form.addEventListener('submit', event => { event.preventDefault(); void send(); });
    scan.addEventListener('click', () => {
      const values = [...document.querySelectorAll('input,textarea,code,pre')]
        .flatMap(element => [element.value || '', element.textContent || '']);
      const match = values.join('\n').match(/\bcfat_[A-Za-z0-9_-]{20,}\b/);
      if (!match) {
        status.className = 'status bad';
        status.textContent = '当前页未发现新格式 Token，请创建后复制并粘贴。';
        return;
      }
      input.value = match[0];
      void send();
    });
  }).catch(() => { /* Navigation can race panel injection; the timer retries. */ });
}

async function report(status, exitIP = '', error = '', bootstrapToken = '', accountID = '') {
  const response = await fetch(payload.report_url, {
    method: 'POST',
    headers: {'Content-Type': 'application/json', Accept: 'application/json'},
    body: JSON.stringify({token: payload.report_token, status, exit_ip: exitIP, error, account_id: accountID, bootstrap_token: bootstrapToken}),
  });
  const body = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(body.error || `Could not report connector state (HTTP ${response.status}).`);
}

function safeError(error) {
  const text = error instanceof Error ? error.message : String(error);
  if (!payload?.proxy_url) return text.slice(0, 500);
  return text.replaceAll(payload.proxy_url, publicProxyEndpoint(payload.proxy_url)).slice(0, 500);
}

process.on('SIGINT', () => { releaseProfileLock(); process.exit(130); });
process.on('SIGTERM', () => { releaseProfileLock(); process.exit(143); });
process.on('exit', releaseProfileLock);
