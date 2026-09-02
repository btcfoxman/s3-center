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
  await page.goto(payload.authorization_url, {waitUntil: 'domcontentloaded', timeout: REQUEST_TIMEOUT_MS});
  await report('browser_open', exitIP);
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

async function report(status, exitIP = '', error = '') {
  const response = await fetch(payload.report_url, {
    method: 'POST',
    headers: {'Content-Type': 'application/json', Accept: 'application/json'},
    body: JSON.stringify({token: payload.report_token, status, exit_ip: exitIP, error}),
  });
  if (!response.ok) throw new Error(`Could not report connector state (HTTP ${response.status}).`);
}

function safeError(error) {
  const text = error instanceof Error ? error.message : String(error);
  if (!payload?.proxy_url) return text.slice(0, 500);
  return text.replaceAll(payload.proxy_url, publicProxyEndpoint(payload.proxy_url)).slice(0, 500);
}

process.on('SIGINT', () => { releaseProfileLock(); process.exit(130); });
process.on('SIGTERM', () => { releaseProfileLock(); process.exit(143); });
process.on('exit', releaseProfileLock);
