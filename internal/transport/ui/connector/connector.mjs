import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';
import {spawn} from 'node:child_process';
import {Server as ProxyChainServer} from 'proxy-chain';

const CONNECTOR_PROTOCOL_VERSION = 'native-browser-v2';
const ISOLATED_BROWSER_PROFILE = 'browser-chrome-v1';
const PROFILE_PATTERN = /^cf-[a-f0-9]{16}$/;
const IDENTITY_PATTERN = /^cfi-[a-f0-9]{24}$/;
const REQUEST_TIMEOUT_MS = 30_000;
const BROWSER_READY_TIMEOUT_MS = 45_000;
const MAX_BRIDGE_BODY_BYTES = 16 << 10;
const MAX_TRACE_BODY_BYTES = 64 << 10;

let payload;
let lockPath;
let lockHandle;
let browserProcess;
let bridge;
let localProxyServer;
let stopping = false;

try {
  const ticketURL = parseLaunchArgument(process.argv[2]);
  payload = await fetchJSON(ticketURL);
  validatePayload(payload);

  const localRoot = process.env.LOCALAPPDATA || path.join(os.homedir(), 'AppData', 'Local');
  const identityRoot = path.join(localRoot, 'S3Center', 'Profiles', payload.profile_id);
  fs.mkdirSync(identityRoot, {recursive: true});
  assertProfileBinding(identityRoot, payload);
  ({lockPath, lockHandle} = acquireProfileLock(identityRoot));

  const executablePath = locateBrowser();
  const profileRoot = path.join(identityRoot, ISOLATED_BROWSER_PROFILE);
  initializeBrowserProfile(profileRoot, payload.profile_id);

  const browserProxyURL = await startLocalProxy(payload.proxy_url);
  const exitIP = await verifyBrowserRoute(browserProxyURL);
  await report('proxy_verified', exitIP);
  bridge = await createBridge(exitIP);
  browserProcess = launchNativeBrowser(executablePath, profileRoot, bridge.startURL, browserProxyURL);
  await bridge.waitUntilReady(BROWSER_READY_TIMEOUT_MS);
  await waitForBrowserExit(browserProcess);
  await bridge.close();
  bridge = undefined;
  await closeLocalProxy();
  releaseProfileLock();
} catch (error) {
  const message = safeError(error);
  if (payload?.report_url && payload?.report_token) {
    try { await report('', '', message); } catch { /* The original error is more useful. */ }
  }
  console.error(`S3 Center Connector: ${message}`);
  await stopResources();
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
  const loopback = ['localhost', '127.0.0.1', '::1'].includes(ticketURL.hostname);
  if (ticketURL.protocol !== 'https:' && !(loopback && ticketURL.protocol === 'http:')) {
    throw new Error('Connector tickets must use HTTPS.');
  }
  return ticketURL;
}

async function fetchJSON(target, options = {}) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS);
  try {
    const response = await fetch(target, {
      headers: {Accept: 'application/json', ...(options.headers || {})},
      cache: 'no-store',
      ...options,
      signal: controller.signal,
    });
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(apiErrorMessage(body, `HTTP ${response.status}`));
    return body;
  } finally {
    clearTimeout(timer);
  }
}

function apiErrorMessage(body, fallback) {
  if (typeof body?.error === 'string') return body.error;
  if (typeof body?.error?.message === 'string') return body.error.message;
  return fallback;
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
  const reportURL = new URL(value.report_url);
  if (reportURL.protocol !== 'https:' && !['localhost', '127.0.0.1', '::1'].includes(reportURL.hostname)) {
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
    try { fs.closeSync(lockHandle); } catch { /* Already closed. */ }
    lockHandle = undefined;
  }
  if (lockPath) {
    try { fs.unlinkSync(lockPath); } catch { /* Already removed. */ }
    lockPath = undefined;
  }
}

function locateBrowser() {
  if (process.env.S3CENTER_BROWSER_PATH) {
    const configured = path.resolve(process.env.S3CENTER_BROWSER_PATH);
    if (!fs.existsSync(configured)) {
      throw new Error('S3CENTER_BROWSER_PATH does not point to an installed browser.');
    }
    if (path.basename(configured).toLowerCase() !== 'chrome.exe') {
      throw new Error('Connector 2.1 requires Google Chrome for strong account isolation. S3CENTER_BROWSER_PATH must point to chrome.exe.');
    }
    return configured;
  }
  const candidates = [
    process.env.PROGRAMFILES && path.join(process.env.PROGRAMFILES, 'Google', 'Chrome', 'Application', 'chrome.exe'),
    process.env['PROGRAMFILES(X86)'] && path.join(process.env['PROGRAMFILES(X86)'], 'Google', 'Chrome', 'Application', 'chrome.exe'),
    process.env.LOCALAPPDATA && path.join(process.env.LOCALAPPDATA, 'Google', 'Chrome', 'Application', 'chrome.exe'),
  ].filter(Boolean);
  const found = candidates.find(candidate => fs.existsSync(candidate));
  if (!found) {
    throw new Error('Connector 2.1 requires an up-to-date Google Chrome. Edge is not used because Windows implicit sign-in can attach the local Microsoft account to a new Edge profile.');
  }
  return found;
}

function initializeBrowserProfile(profileRoot, profileID) {
  const defaultRoot = path.join(profileRoot, 'Default');
  const preferencesPath = path.join(defaultRoot, 'Preferences');
  fs.mkdirSync(defaultRoot, {recursive: true});
  if (!fs.existsSync(preferencesPath)) {
    const preferences = {
      browser: {
        check_default_browser: false,
        has_seen_welcome_page: true,
      },
      credentials_enable_service: false,
      distribution: {
        do_not_create_any_shortcuts: true,
        import_bookmarks: false,
        import_history: false,
        import_search_engine: false,
        make_chrome_default: false,
        skip_first_run_ui: true,
      },
      profile: {
        name: `S3 Center ${profileID.slice(-6)}`,
        password_manager_enabled: false,
      },
      sync: {requested: false},
    };
    fs.writeFileSync(preferencesPath, `${JSON.stringify(preferences, null, 2)}\n`, {encoding: 'utf8', mode: 0o600});
  }
  const firstRunMarker = path.join(profileRoot, 'First Run');
  if (!fs.existsSync(firstRunMarker)) fs.writeFileSync(firstRunMarker, '', {mode: 0o600});
}

function launchNativeBrowser(executablePath, profileRoot, startURL, browserProxyURL) {
  const args = [
    `--user-data-dir=${profileRoot}`,
    '--profile-directory=Default',
    '--new-window',
    '--start-maximized',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-background-mode',
    '--disable-default-apps',
    '--disable-sync',
    '--disable-extensions',
    startURL,
  ];
  if (browserProxyURL) {
    args.splice(args.length - 1, 0, `--proxy-server=${browserProxyURL}`, '--proxy-bypass-list=localhost;127.0.0.1');
  }
  return spawn(executablePath, args, {stdio: 'ignore', windowsHide: false});
}

function waitForBrowserExit(child) {
  return new Promise((resolve, reject) => {
    child.once('error', reject);
    child.once('exit', (code, signal) => {
      if (code && !stopping) {
        reject(new Error(`The native browser exited unexpectedly (${signal || code}).`));
        return;
      }
      resolve();
    });
  });
}

function parseProxy(raw) {
  const proxy = new URL(raw.includes('://') ? raw : `http://${raw}`);
  if (!['http:', 'https:', 'socks5:', 'socks5h:'].includes(proxy.protocol) || !proxy.hostname) {
    throw new Error('Proxy must use HTTP, HTTPS, SOCKS5, or SOCKS5H.');
  }
  if ((proxy.pathname && proxy.pathname !== '/') || proxy.search || proxy.hash) {
    throw new Error('Proxy URL must not contain a path, query, or fragment.');
  }
  return proxy;
}

async function startLocalProxy(raw) {
  if (!raw) return '';
  const upstreamProxyURL = parseProxy(raw).toString();
  localProxyServer = new ProxyChainServer({
    host: '127.0.0.1',
    port: 0,
    prepareRequestFunction: () => ({requestAuthentication: false, upstreamProxyUrl: upstreamProxyURL}),
  });
  await localProxyServer.listen();
  if (!Number.isInteger(localProxyServer.port) || localProxyServer.port < 1) {
    throw new Error('Could not allocate the private proxy bridge.');
  }
  return `http://127.0.0.1:${localProxyServer.port}`;
}

async function verifyBrowserRoute(browserProxyURL) {
  const trace = await fetchCloudflareTrace(browserProxyURL);
  const match = /^ip=(.+)$/m.exec(trace);
  const exitIP = match?.[1]?.trim() || '';
  if (!net.isIP(exitIP)) throw new Error('Cloudflare did not return a valid proxy exit IP.');
  return exitIP;
}

function fetchCloudflareTrace(browserProxyURL) {
  return new Promise((resolve, reject) => {
    const target = 'http://www.cloudflare.com/cdn-cgi/trace';
    const proxy = browserProxyURL ? new URL(browserProxyURL) : undefined;
    const request = http.get({
      hostname: proxy?.hostname || 'www.cloudflare.com',
      port: proxy?.port || 80,
      path: proxy ? target : '/cdn-cgi/trace',
      headers: {
        Accept: 'text/plain',
        Connection: 'close',
        Host: 'www.cloudflare.com',
        'User-Agent': 'S3-Center-Connector/2.1',
      },
      timeout: REQUEST_TIMEOUT_MS,
    }, response => {
      const chunks = [];
      let size = 0;
      response.on('data', chunk => {
        size += chunk.length;
        if (size > MAX_TRACE_BODY_BYTES) {
          request.destroy(new Error('Cloudflare trace response is too large.'));
          return;
        }
        chunks.push(chunk);
      });
      response.on('end', () => {
        if (response.statusCode !== 200) {
          reject(new Error(`Cloudflare proxy verification returned HTTP ${response.statusCode || 0}.`));
          return;
        }
        resolve(Buffer.concat(chunks).toString('utf8'));
      });
    });
    request.once('timeout', () => request.destroy(new Error('Cloudflare proxy verification timed out.')));
    request.once('error', reject);
  });
}

function publicProxyEndpoint(raw) {
  if (!raw) return 'direct';
  const proxy = parseProxy(raw);
  return `${proxy.protocol}//${proxy.host}`;
}

async function createBridge(exitIP) {
  const nonce = crypto.randomBytes(32).toString('base64url');
  const bridgeToken = crypto.randomBytes(32).toString('base64url');
  const scriptNonce = crypto.randomBytes(24).toString('base64url');
  let resolveReady;
  let rejectReady;
  let ready = false;
  let browserReported = false;
  const readyPromise = new Promise((resolve, reject) => { resolveReady = resolve; rejectReady = reject; });

  const server = http.createServer((request, response) => {
    void handleBridgeRequest(request, response).catch(error => {
      if (!response.headersSent) writeBridgeJSON(response, 400, {error: safeError(error)});
      else response.destroy();
    });
  });
  server.on('clientError', (_error, socket) => socket.end('HTTP/1.1 400 Bad Request\r\n\r\n'));
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Could not start the private browser bridge.');
  const origin = `http://127.0.0.1:${address.port}`;
  const expectedHost = `127.0.0.1:${address.port}`;

  async function handleBridgeRequest(request, response) {
    if (request.headers.host !== expectedHost) {
      writeBridgeJSON(response, 421, {error: 'Invalid bridge host.'});
      return;
    }
    const requestURL = new URL(request.url || '/', origin);
    if (request.method === 'GET' && requestURL.pathname === `/start/${nonce}`) {
      writeBridgeHTML(response, bootstrapPage({nonce, bridgeToken, scriptNonce, exitIP}), scriptNonce);
      return;
    }
    if (request.method === 'POST' && requestURL.pathname === `/ready/${nonce}`) {
      if (request.headers['x-s3center-bridge-token'] !== bridgeToken || request.headers.origin !== origin) {
        writeBridgeJSON(response, 401, {error: 'Invalid bridge token.'});
        return;
      }
      const event = await readBridgeJSON(request);
      if (event.webdriver === true) {
        const error = new Error('Automated browser detected. Connector requires an unmodified native Chrome process.');
        rejectReady(error);
        writeBridgeJSON(response, 400, {error: error.message});
        return;
      }
      if (!browserReported) {
        await report('browser_open', exitIP);
        browserReported = true;
      }
      ready = true;
      resolveReady();
      writeBridgeJSON(response, 200, {ok: true});
      return;
    }
    if (request.method === 'POST' && requestURL.pathname === `/import/${nonce}`) {
      if (request.headers['x-s3center-bridge-token'] !== bridgeToken || request.headers.origin !== origin) {
        writeBridgeJSON(response, 401, {error: 'Invalid bridge token.'});
        return;
      }
      const event = await readBridgeJSON(request);
      await processBridgeEvent(event);
      writeBridgeJSON(response, 200, {ok: true});
      return;
    }
    writeBridgeJSON(response, 404, {error: 'Not found.'});
  }

  return {
    startURL: `${origin}/start/${nonce}`,
    waitUntilReady(timeout) {
      if (ready) return Promise.resolve();
      return Promise.race([
        readyPromise,
        new Promise((_, reject) => setTimeout(() => reject(new Error('The isolated Chrome control page did not open. Close the window, reinstall the latest connector, and try again.')), timeout)),
      ]);
    },
    close() {
      return new Promise(resolve => server.close(() => resolve()));
    },
  };
}

async function readBridgeJSON(request) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > MAX_BRIDGE_BODY_BYTES) throw new Error('Bridge event is too large.');
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8'));
  } catch {
    throw new Error('Bridge event is invalid.');
  }
}

async function processBridgeEvent(event) {
  if (!event || typeof event.type !== 'string') throw new Error('Bridge event type is missing.');
  if (event.type !== 'bootstrap_token') throw new Error('Unsupported bridge event.');
  await report('bootstrap_token', String(event.exit_ip || '').trim(), '', event.bootstrap_token, event.account_id);
}

function writeBridgeJSON(response, status, value) {
  const body = Buffer.from(JSON.stringify(value));
  response.writeHead(status, {
    'Cache-Control': 'no-store',
    'Content-Type': 'application/json; charset=utf-8',
    'Content-Length': body.length,
    'X-Content-Type-Options': 'nosniff',
  });
  response.end(body);
}

function writeBridgeHTML(response, markup, scriptNonce) {
  const body = Buffer.from(markup);
  response.writeHead(200, {
    'Cache-Control': 'no-store',
    'Content-Type': 'text/html; charset=utf-8',
    'Content-Length': body.length,
    'Content-Security-Policy': `default-src 'none'; style-src 'unsafe-inline'; script-src 'nonce-${scriptNonce}'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'`,
    'X-Content-Type-Options': 'nosniff',
  });
  response.end(body);
}

function bootstrapPage({nonce, bridgeToken, scriptNonce, exitIP}) {
  const config = JSON.stringify({
    authorizationURL: payload.authorization_url,
    bridgeToken,
    exitIP,
    importURL: `/import/${nonce}`,
    readyURL: `/ready/${nonce}`,
  }).replaceAll('<', '\\u003c');
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>S3 Center Cloudflare 隔离接入</title><style>
    :root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;min-height:100vh;background:radial-gradient(circle at 15% 10%,#183d34 0,transparent 30%),#07100f;color:#eaf5f0;font:15px/1.55 system-ui,sans-serif}.shell{width:min(820px,calc(100% - 36px));margin:38px auto;padding:clamp(24px,5vw,44px);border:1px solid #29463e;border-radius:24px;background:#0f1d1bf2;box-shadow:0 24px 80px #0007}.eyebrow{color:#72e6b4;font:700 12px ui-monospace,monospace;letter-spacing:.17em}h1{margin:12px 0 8px;font-size:clamp(28px,6vw,42px);line-height:1.1}p{color:#b8ccc4}.safe{display:flex;gap:12px;align-items:center;margin:22px 0;padding:14px 16px;border:1px solid #28584a;border-radius:14px;background:#102922;color:#9af5cf}.dot{width:9px;height:9px;border-radius:50%;background:#72e6b4;box-shadow:0 0 18px #72e6b4}.steps{display:grid;gap:16px;margin:26px 0}.step{padding:20px;border:1px solid #243d37;border-radius:16px;background:#0b1715}.step b{display:block;margin-bottom:7px;color:#fff}.open{display:inline-flex;margin-top:10px;padding:11px 16px;border-radius:11px;background:#f18b4c;color:#101311;font-weight:800;text-decoration:none}label{display:block;margin-top:14px;color:#d9e9e2;font-weight:650}input,textarea{width:100%;margin-top:7px;padding:12px 13px;border:1px solid #355249;border-radius:10px;background:#081310;color:#f1faf6;font:14px/1.45 ui-monospace,monospace;outline:none}textarea{min-height:92px;resize:vertical}input:focus,textarea:focus{border-color:#72e6b4;box-shadow:0 0 0 3px #72e6b426}button{margin-top:16px;padding:12px 18px;border:0;border-radius:11px;background:#72e6b4;color:#07100f;font-weight:850;cursor:pointer}button:disabled{opacity:.55;cursor:default}.status{min-height:24px;margin-top:13px;color:#93aaa1}.status.error{color:#ff9f8e}.status.ok{color:#8ef0c5}.note{font-size:13px;color:#809a91}code{color:#ffb17b}
  </style></head><body><main class="shell"><div class="eyebrow">S3 CENTER · ISOLATED CHROME</div><h1>Cloudflare 安全接入控制台</h1><p>这是全新的 S3 Center 专用 Chrome Profile，不读取本机默认 Chrome/Edge 数据；初始不绑定浏览器账号，并关闭同步、首次导入、扩展和密码保存。</p><div class="safe"><span class="dot"></span><span id="safe-text">正在验证原生浏览器与出口…</span></div><div class="steps"><section class="step"><b>1. 打开 Cloudflare 官方 Token 页面</b><span>请在新标签页完成登录和 Token 创建。完成后保留本控制台标签页。</span><br><a class="open" href="${escapeHTML(payload.authorization_url)}" target="_blank" rel="noopener noreferrer">打开 Cloudflare 官方页面 ↗</a></section><section class="step"><b>2. 把一次性 Token 安全导入 S3 Center</b><span>在 Cloudflare Token 结果页复制只显示一次的 <code>cfat_...</code>，同时复制浏览器地址栏中的 Account 页面地址；控制台会自动从地址中识别 Account ID。</span><form id="import-form" autocomplete="off"><label>Cloudflare Account 页面地址或 32 位 Account ID<input id="account-reference" inputmode="url" spellcheck="false" placeholder="https://dash.cloudflare.com/&lt;account-id&gt;/api-tokens/..." required></label><label>一次性引导 API Token<textarea id="bootstrap-token" spellcheck="false" placeholder="cfat_..." required></textarea></label><button id="submit" type="submit" disabled>安全导入并发现 R2 Bucket</button><div id="status" class="status" role="status" aria-live="polite"></div></form></section></div><p class="note">Token 仅通过随机的 <code>127.0.0.1</code> 本机桥接提交到当前短期会话，不写入浏览器磁盘、数据库或日志。成功后可关闭本窗口并返回管理端选择 Bucket。</p></main><script nonce="${scriptNonce}">
    const config=${config};const form=document.getElementById('import-form');const account=document.getElementById('account-reference');const token=document.getElementById('bootstrap-token');const submit=document.getElementById('submit');const status=document.getElementById('status');const safeText=document.getElementById('safe-text');
    function accountID(value){const clean=value.trim().toLowerCase();if(/^[a-f0-9]{32}$/.test(clean))return clean;try{const parsed=new URL(clean);if(parsed.hostname!=='dash.cloudflare.com')return '';return parsed.pathname.split('/').find(part=>/^[a-f0-9]{32}$/.test(part))||''}catch{return ''}}
    form.addEventListener('submit',async event=>{event.preventDefault();status.className='status';const id=accountID(account.value);if(!id){status.textContent='无法识别 Account ID，请复制当前 Cloudflare 页面完整地址。';status.classList.add('error');return}submit.disabled=true;status.textContent='正在通过绑定代理验证 Token 并发现 Bucket…';try{const response=await fetch(config.importURL,{method:'POST',headers:{'Content-Type':'application/json','X-S3Center-Bridge-Token':config.bridgeToken},body:JSON.stringify({type:'bootstrap_token',account_id:id,bootstrap_token:token.value.trim(),exit_ip:config.exitIP})});const body=await response.json().catch(()=>({}));if(!response.ok)throw new Error(body.error||('HTTP '+response.status));token.value='';status.textContent='导入成功。请返回 S3 Center 管理端选择 Bucket。';status.classList.add('ok')}catch(error){status.textContent=error.message||String(error);status.classList.add('error');submit.disabled=false}});
    async function announce(){try{const response=await fetch(config.readyURL,{method:'POST',headers:{'Content-Type':'application/json','X-S3Center-Bridge-Token':config.bridgeToken},body:JSON.stringify({webdriver:navigator.webdriver===true})});const body=await response.json().catch(()=>({}));if(!response.ok)throw new Error(body.error||('HTTP '+response.status));safeText.textContent='原生 Chrome 与独立 Profile 已就绪 · 出口 IP '+config.exitIP;submit.disabled=false}catch(error){safeText.textContent=error.message||String(error);safeText.parentElement.style.borderColor='#7c3d35';safeText.parentElement.style.color='#ff9f8e'}}announce();
  </script></body></html>`;
}

function escapeHTML(value) {
  return String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;').replaceAll("'", '&#39;');
}

async function report(status, exitIP = '', error = '', bootstrapToken = '', accountID = '') {
  await fetchJSON(payload.report_url, {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({
      token: payload.report_token,
      connector_version: CONNECTOR_PROTOCOL_VERSION,
      status,
      exit_ip: exitIP,
      error,
      account_id: accountID,
      bootstrap_token: bootstrapToken,
    }),
  });
}

function safeError(error) {
  const text = error instanceof Error ? error.message : String(error);
  if (!payload?.proxy_url) return text.slice(0, 500);
  return text.replaceAll(payload.proxy_url, publicProxyEndpoint(payload.proxy_url)).slice(0, 500);
}

async function closeLocalProxy() {
  if (!localProxyServer) return;
  try { await localProxyServer.close(true); } catch { /* Already closed. */ }
  localProxyServer = undefined;
}

async function stopResources() {
  if (stopping) return;
  stopping = true;
  if (browserProcess && browserProcess.exitCode === null) {
    try { browserProcess.kill(); } catch { /* Already closed. */ }
  }
  if (bridge) {
    try { await bridge.close(); } catch { /* Already closed. */ }
    bridge = undefined;
  }
  await closeLocalProxy();
  releaseProfileLock();
}

process.on('SIGINT', () => { void stopResources().finally(() => process.exit(130)); });
process.on('SIGTERM', () => { void stopResources().finally(() => process.exit(143)); });
process.on('exit', releaseProfileLock);
