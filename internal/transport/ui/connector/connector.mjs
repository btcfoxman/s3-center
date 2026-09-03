import crypto from 'node:crypto';
import fs from 'node:fs';
import http from 'node:http';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import process from 'node:process';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
import {Server as ProxyChainServer} from 'proxy-chain';

const CONNECTOR_PROTOCOL_VERSION = 'native-browser-v1';
const PROFILE_PATTERN = /^cf-[a-f0-9]{16}$/;
const IDENTITY_PATTERN = /^cfi-[a-f0-9]{24}$/;
const REQUEST_TIMEOUT_MS = 30_000;
const EXTENSION_READY_TIMEOUT_MS = 45_000;
const MAX_BRIDGE_BODY_BYTES = 16 << 10;

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
  const profileRoot = path.join(localRoot, 'S3Center', 'Profiles', payload.profile_id);
  fs.mkdirSync(profileRoot, {recursive: true});
  assertProfileBinding(profileRoot, payload);
  ({lockPath, lockHandle} = acquireProfileLock(profileRoot));

  const executablePath = locateBrowser();
  const connectorRoot = path.dirname(fileURLToPath(import.meta.url));
  const extensionRoot = path.join(connectorRoot, 'extension');
  assertExtension(extensionRoot);

  bridge = await createBridge();
  const browserProxyURL = await startLocalProxy(payload.proxy_url);
  browserProcess = launchNativeBrowser(executablePath, profileRoot, extensionRoot, bridge.startURL, browserProxyURL);
  await bridge.waitUntilReady(EXTENSION_READY_TIMEOUT_MS);
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

function assertExtension(extensionRoot) {
  for (const name of ['manifest.json', 'background.js', 'bootstrap.js', 'panel.js']) {
    if (!fs.existsSync(path.join(extensionRoot, name))) {
      throw new Error(`Connector browser extension is incomplete: ${name} is missing.`);
    }
  }
}

function launchNativeBrowser(executablePath, profileRoot, extensionRoot, startURL, browserProxyURL) {
  const args = [
    `--user-data-dir=${profileRoot}`,
    '--new-window',
    '--start-maximized',
    '--no-first-run',
    '--no-default-browser-check',
    '--disable-background-mode',
    `--disable-extensions-except=${extensionRoot}`,
    `--load-extension=${extensionRoot}`,
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

function publicProxyEndpoint(raw) {
  if (!raw) return 'direct';
  const proxy = parseProxy(raw);
  return `${proxy.protocol}//${proxy.host}`;
}

async function createBridge() {
  const nonce = crypto.randomBytes(32).toString('base64url');
  const bridgeToken = crypto.randomBytes(32).toString('base64url');
  let resolveReady;
  let ready = false;
  const readyPromise = new Promise(resolve => { resolveReady = resolve; });

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
      writeBridgeHTML(response, bootstrapPage());
      return;
    }
    if (request.method === 'GET' && requestURL.pathname === `/config/${nonce}`) {
      ready = true;
      resolveReady();
      writeBridgeJSON(response, 200, {
        authorization_url: payload.authorization_url,
        bridge_origin: origin,
        bridge_token: bridgeToken,
        proxy_enabled: Boolean(payload.proxy_url),
      });
      return;
    }
    if (request.method === 'POST' && requestURL.pathname === '/event') {
      if (request.headers.authorization !== `Bearer ${bridgeToken}`) {
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
        new Promise((_, reject) => setTimeout(() => reject(new Error('The native browser helper extension did not start. Reinstall the latest connector and try again.')), timeout)),
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
  switch (event.type) {
    case 'proxy_verified': {
      const exitIP = String(event.exit_ip || '').trim();
      if (!net.isIP(exitIP)) throw new Error('Cloudflare did not return a valid browser exit IP.');
      await report('proxy_verified', exitIP);
      return;
    }
    case 'browser_open':
      await report('browser_open', String(event.exit_ip || '').trim());
      return;
    case 'bootstrap_token':
      await report('bootstrap_token', String(event.exit_ip || '').trim(), '', event.bootstrap_token, event.account_id);
      return;
    case 'error':
      await report('', '', String(event.error || 'The native browser connector failed.'));
      return;
    case 'heartbeat':
      return;
    default:
      throw new Error('Unsupported bridge event.');
  }
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

function writeBridgeHTML(response, markup) {
  const body = Buffer.from(markup);
  response.writeHead(200, {
    'Cache-Control': 'no-store',
    'Content-Type': 'text/html; charset=utf-8',
    'Content-Length': body.length,
    'Content-Security-Policy': "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'",
    'X-Content-Type-Options': 'nosniff',
  });
  response.end(body);
}

function bootstrapPage() {
  return `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>S3 Center 原生 Cloudflare 登录</title><style>
    :root{color-scheme:dark}*{box-sizing:border-box}body{margin:0;min-height:100vh;display:grid;place-items:center;background:#07100f;color:#eaf5f0;font:15px/1.65 system-ui,sans-serif}.card{width:min(620px,calc(100% - 36px));padding:38px;border:1px solid #29463e;border-radius:22px;background:#0f1d1b;box-shadow:0 24px 80px #0007}.eyebrow{color:#72e6b4;font:700 12px ui-monospace,monospace;letter-spacing:.17em}h1{margin:14px 0 8px;font-size:clamp(27px,6vw,39px);line-height:1.15}p{color:#b8ccc4}.status{margin-top:24px;padding:14px 16px;border-radius:12px;background:#122a25;color:#8ef0c5}.note{font-size:13px;color:#809a91}
  </style></head><body><main class="card"><div class="eyebrow">S3 CENTER · NATIVE BROWSER</div><h1>正在准备 Cloudflare 安全登录</h1><p>此窗口由原生 Edge / Chrome 打开，没有使用 Playwright、Selenium 或远程调试。独立 Profile 与专属代理正在初始化。</p><p id="bridge-status" class="status">正在载入本机安全助手，随后会自动进入 Cloudflare 官方页面…</p><p class="note">若此页面停留超过 45 秒，请关闭窗口，从管理端重新下载并安装最新版 Connector。</p></main></body></html>`;
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
