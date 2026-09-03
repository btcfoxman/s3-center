'use strict';

let panelInstalled = false;
let heartbeatTimer;

installWhenAccountIsSelected();
window.addEventListener('popstate', installWhenAccountIsSelected);
setInterval(installWhenAccountIsSelected, 1_500);

function installWhenAccountIsSelected() {
  if (panelInstalled || !/^\/[a-f0-9]{32}(?:\/|$)/i.test(location.pathname)) return;
  panelInstalled = true;
  const host = document.createElement('div');
  host.id = 's3-center-native-connector';
  host.style.cssText = 'all:initial;position:fixed;right:18px;bottom:18px;z-index:2147483647';
  const root = host.attachShadow({mode: 'closed'});
  root.innerHTML = `
    <style>
      *{box-sizing:border-box}.panel{width:min(390px,calc(100vw - 36px));overflow:hidden;border:1px solid #d7dce2;border-radius:16px;background:#fff;color:#1f2937;box-shadow:0 20px 55px #10182838;font:13px/1.5 Inter,system-ui,sans-serif}.head{display:flex;gap:12px;align-items:center;padding:15px 17px;background:linear-gradient(135deg,#fff7ed,#f0fdf4);border-bottom:1px solid #eceff3}.mark{display:grid;place-items:center;width:38px;height:38px;border-radius:11px;background:#f48120;color:#fff;font-weight:800}.title{font-size:14px;font-weight:750}.sub{color:#087a55;font-size:12px}.body{padding:15px 17px}.steps{margin:0 0 12px;padding-left:19px;color:#475467}.steps b{color:#1f2937}.field{display:flex;gap:8px}input{min-width:0;flex:1;border:1px solid #cfd6df;border-radius:9px;padding:10px 11px;background:#fff;color:#101828;font:12px ui-monospace,monospace}input:focus{outline:2px solid #f9b77d;border-color:#f48120}button{border:0;border-radius:9px;padding:10px 12px;background:#f48120;color:#fff;font:700 12px system-ui;cursor:pointer}button.secondary{width:100%;margin-top:8px;background:#eef2f6;color:#344054}button:disabled{opacity:.55;cursor:wait}.status{min-height:20px;margin-top:9px;color:#667085;font-size:12px}.status.ok{color:#087a55;font-weight:650}.status.bad{color:#b42318}.note{margin:10px 0 0;color:#98a2b3;font-size:11px}
    </style>
    <section class="panel" aria-label="S3 Center Cloudflare connector">
      <div class="head"><div class="mark">S3</div><div><div class="title">S3 Center 快速接入</div><div class="sub">原生浏览器 · 独立 Profile · 固定出口</div></div></div>
      <div class="body">
        <ol class="steps"><li>确认预填权限并创建临时 Account API Token</li><li>复制创建结果中的 <b>API Token</b>，粘贴到下方</li></ol>
        <form id="import-form"><div class="field"><input id="token" type="password" autocomplete="off" spellcheck="false" placeholder="粘贴一次性 API Token" aria-label="Cloudflare API Token"><button id="submit" type="submit">安全导入</button></div></form>
        <button id="scan" class="secondary" type="button">读取当前页面显示的新 Token</button>
        <div id="status" class="status" role="status"></div>
        <p class="note">登录验证阶段不会修改 Cloudflare 页面。Token 不写入磁盘；桶级凭据创建后会自动撤销该临时 Token。</p>
      </div>
    </section>`;
  document.documentElement.appendChild(host);

  const form = root.getElementById('import-form');
  const input = root.getElementById('token');
  const submit = root.getElementById('submit');
  const scan = root.getElementById('scan');
  const status = root.getElementById('status');

  const send = () => {
    status.className = 'status';
    status.textContent = '正在经安全通道验证并发现 R2 Bucket…';
    submit.disabled = true;
    scan.disabled = true;
    chrome.runtime.sendMessage({type: 'submit_token', token: input.value}, response => {
      if (chrome.runtime.lastError || !response?.ok) {
        status.className = 'status bad';
        status.textContent = response?.error || chrome.runtime.lastError?.message || '导入失败。';
        submit.disabled = false;
        scan.disabled = false;
        return;
      }
      input.value = '';
      status.className = 'status ok';
      status.textContent = '导入成功，请返回 S3 Center 选择要接入的 Bucket。';
    });
  };
  form.addEventListener('submit', event => { event.preventDefault(); send(); });
  scan.addEventListener('click', () => {
    const values = [...document.querySelectorAll('input,textarea,code,pre')]
      .flatMap(element => [element.value || '', element.textContent || '']);
    const match = values.join('\n').match(/\bcfat_[A-Za-z0-9_-]{20,512}\b/);
    if (!match) {
      status.className = 'status bad';
      status.textContent = '当前页面未发现新格式 Token，请创建后复制并粘贴。';
      return;
    }
    input.value = match[0];
    send();
  });

  heartbeatTimer = setInterval(() => chrome.runtime.sendMessage({type: 'heartbeat'}), 10_000);
}

window.addEventListener('pagehide', () => {
  if (heartbeatTimer) clearInterval(heartbeatTimer);
});
