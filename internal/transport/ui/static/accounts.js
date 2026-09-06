(function () {
  'use strict';

  function csrfToken() {
    const match = /(?:^|; )s3orch_csrf=([^;]*)/.exec(document.cookie);
    return match ? match[1] : '';
  }

  function fetchJSON(url, options) {
    return fetch(url, options).then(function (response) {
      return response.json().catch(function () { return {}; }).then(function (body) {
        if (!response.ok) throw new Error(body.error || ('HTTP ' + response.status));
        return body;
      });
    });
  }

  function postJSON(url, payload) {
    return fetchJSON(url, {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken()},
      body: JSON.stringify(payload)
    });
  }

  const dialog = document.getElementById('account-dialog');
  const openButton = document.getElementById('account-add-btn');
  const cancelButton = document.getElementById('account-cancel');
  const manualForm = document.getElementById('account-form');
  let pollTimer = 0;

  function selectTab(panelID) {
    if (!dialog) return;
    dialog.querySelectorAll('.onboarding-tab').forEach(function (tab) {
      const selected = tab.dataset.panel === panelID;
      tab.classList.toggle('active', selected);
      tab.setAttribute('aria-selected', selected ? 'true' : 'false');
    });
    dialog.querySelectorAll('.onboarding-panel').forEach(function (panel) {
      panel.classList.toggle('active', panel.id === panelID);
    });
    dialog.scrollTop = 0;
  }

  if (dialog) {
    dialog.querySelectorAll('.onboarding-tab').forEach(function (tab) {
      tab.addEventListener('click', function () { selectTab(tab.dataset.panel); });
    });
  }

  if (openButton && dialog) {
    openButton.addEventListener('click', function () {
      resetOAuthFlow();
      if (dialog.dataset.oauthEnabled === 'true') selectTab('oauth-panel');
      dialog.showModal();
      dialog.scrollTop = 0;
    });
  }
  if (cancelButton && dialog) {
    cancelButton.addEventListener('click', function () {
      window.clearTimeout(pollTimer);
      dialog.close();
    });
  }

  if (manualForm) {
    const submitButton = document.getElementById('account-submit');
    const status = document.getElementById('account-add-status');
    const warning = document.getElementById('warning-percent');
    const warningOutput = document.getElementById('warning-output');
    warning.addEventListener('input', function () { warningOutput.textContent = warning.value + '%'; });

    manualForm.addEventListener('submit', function (event) {
      event.preventDefault();
      const values = new FormData(manualForm);
      submitButton.disabled = true;
      setStatus(status, 'checking', '正在通过指定出口验证 R2 Bucket…');
      postJSON('api/r2-accounts', {
        name: values.get('name'), account_label: values.get('account_label'),
        account_id: values.get('account_id'), jurisdiction: values.get('jurisdiction'),
        bucket: values.get('bucket'), access_key_id: values.get('access_key_id'),
        secret_access_key: values.get('secret_access_key'), proxy_url: values.get('proxy_url'),
        warning_percent: Number(values.get('warning_percent'))
      }).then(function () {
        setStatus(status, 'success', '验证通过，账号已加入调度。正在刷新额度视图…');
        manualForm.reset();
        setTimeout(function () { location.reload(); }, 700);
      }).catch(function (error) {
        setStatus(status, 'error', error.message);
      }).finally(function () { submitButton.disabled = false; });
    });
  }

  const oauthStartForm = document.getElementById('oauth-start-form');
  const oauthCompleteForm = document.getElementById('oauth-complete-form');
  const oauthSessionBox = document.getElementById('oauth-session');
  const oauthStatus = document.getElementById('oauth-status');
  const oauthStartButton = document.getElementById('oauth-start');
  const oauthLaunchButton = document.getElementById('oauth-launch');
  const oauthProfileID = document.getElementById('oauth-profile-id');
  const oauthSessionState = document.getElementById('oauth-session-state');
  const oauthRouteLabel = document.getElementById('oauth-route-label');
  const oauthExistingAccount = document.getElementById('oauth-existing-account');
  const oauthIdentityLabel = document.getElementById('oauth-identity-label');
  const oauthProxyURL = document.getElementById('oauth-proxy-url');
  const oauthAccount = document.getElementById('oauth-account');
  const oauthBucket = document.getElementById('oauth-bucket');
  const oauthWarning = document.getElementById('oauth-warning-percent');
  const oauthWarningOutput = document.getElementById('oauth-warning-output');
  const oauthCompleteButton = document.getElementById('oauth-complete');
  let oauthSessionID = '';
  let connectorURL = '';
  let discoveredAccounts = [];

  if (oauthWarning) oauthWarning.addEventListener('input', function () { oauthWarningOutput.textContent = oauthWarning.value + '%'; });
  if (oauthLaunchButton) {
    oauthLaunchButton.addEventListener('click', function () {
      if (!connectorURL) return;
      window.location.href = connectorURL;
      setStatus(oauthStatus, 'checking', '已请求启动 Connector 2.1，等待专用 Chrome 出口验证…');
      schedulePoll(700);
    });
  }
  if (oauthAccount) oauthAccount.addEventListener('change', renderBuckets);

  if (oauthStartForm) {
    oauthStartForm.addEventListener('submit', function (event) {
      event.preventDefault();
      oauthStartButton.disabled = true;
      setStatus(oauthStatus, 'checking', '正在创建一次性登录通道…');
      postJSON('api/cloudflare-onboarding/start', {
        account_id: oauthExistingAccount.value,
        identity_label: oauthIdentityLabel.value,
        proxy_url: oauthProxyURL.disabled ? '' : oauthProxyURL.value
      }).then(function (body) {
        oauthSessionID = body.session.id;
        connectorURL = body.connector_url;
        oauthProfileID.textContent = body.session.profile_id;
        oauthSessionBox.hidden = false;
        oauthLaunchButton.disabled = false;
        oauthSessionState.textContent = '通道已创建';
        oauthRouteLabel.textContent = body.session.proxy + ' → Cloudflare Token Wizard';
        setStatus(oauthStatus, 'success', '通道有效期至 ' + new Date(body.session.expires_at).toLocaleTimeString() + '。点击上方按钮启动原生隔离浏览器与官方预填 Token 页面。');
        oauthSessionBox.scrollIntoView({behavior: 'smooth', block: 'nearest'});
        schedulePoll(1200);
      }).catch(function (error) {
        setStatus(oauthStatus, 'error', error.message);
      }).finally(function () { oauthStartButton.disabled = false; });
    });
  }

  if (oauthCompleteForm) {
    oauthCompleteForm.addEventListener('submit', function (event) {
      event.preventDefault();
      oauthCompleteButton.disabled = true;
      setStatus(oauthStatus, 'checking', '正在创建 Bucket 级 R2 Token，并等待 Cloudflare IAM 生效后验证连接（通常数秒，最长约 1 分钟）…');
      postJSON('api/cloudflare-onboarding/complete', {
        session_id: oauthSessionID, account_id: oauthAccount.value, bucket: oauthBucket.value,
        backend_name: document.getElementById('oauth-backend-name').value,
        warning_percent: Number(oauthWarning.value)
      }).then(function (body) {
        const message = body.warning
          ? 'Bucket 已接入，但临时引导 Token 自动撤销失败，请立即在 Cloudflare 中手动撤销。'
          : '凭据已加密保存，临时引导 Token 已撤销，Bucket 已加入调度。正在刷新…';
        setStatus(oauthStatus, body.warning ? 'error' : 'success', message);
        window.clearTimeout(pollTimer);
        setTimeout(function () { location.reload(); }, body.warning ? 5000 : 850);
      }).catch(function (error) {
        setStatus(oauthStatus, 'error', error.message);
        oauthCompleteButton.disabled = false;
      });
    });
  }

  document.querySelectorAll('.cf-oauth-reauthorize').forEach(function (button) {
    button.addEventListener('click', function () {
      if (!dialog || !oauthStartForm) return;
      resetOAuthFlow();
      oauthExistingAccount.value = button.dataset.accountId || '';
      oauthIdentityLabel.value = button.dataset.accountLabel || 'Cloudflare R2 identity';
      oauthProxyURL.value = '';
      oauthProxyURL.disabled = true;
      oauthProxyURL.placeholder = '沿用账号已加密保存的代理；如需修改请先使用卡片上的代理设置';
      oauthStartButton.textContent = '重新登录 / 添加 Bucket';
      selectTab('oauth-panel');
      dialog.showModal();
      dialog.scrollTop = 0;
      setStatus(oauthStatus, '', '将沿用该账号绑定的隔离 Profile 与代理出口。');
    });
  });

  function resetOAuthFlow() {
    window.clearTimeout(pollTimer);
    oauthSessionID = '';
    connectorURL = '';
    discoveredAccounts = [];
    if (!oauthStartForm) return;
    oauthStartForm.reset();
    oauthExistingAccount.value = '';
    oauthProxyURL.disabled = false;
    oauthProxyURL.placeholder = 'http://user:pass@203.0.113.10:8080';
    oauthStartButton.textContent = '创建隔离登录';
    oauthSessionBox.hidden = true;
    oauthCompleteForm.hidden = true;
    oauthLaunchButton.disabled = false;
    oauthProfileID.textContent = '';
    setStatus(oauthStatus, '', '');
  }

  function schedulePoll(delay) {
    window.clearTimeout(pollTimer);
    if (oauthSessionID) pollTimer = window.setTimeout(pollOAuth, delay);
  }

  function pollOAuth() {
    fetchJSON('api/cloudflare-onboarding/status?id=' + encodeURIComponent(oauthSessionID), {cache: 'no-store'})
      .then(function (body) {
        updateOAuthState(body.session);
        if (!['authorized', 'complete', 'error'].includes(body.session.status)) schedulePoll(1400);
      }).catch(function (error) { setStatus(oauthStatus, 'error', error.message); });
  }

  function updateOAuthState(session) {
    const labels = {
      waiting_connector: '等待 Connector', opening_browser: '启动浏览器', waiting_authorization: '等待引导 Token',
      discovering: '验证并发现资源', authorized: '引导完成', provisioning: '创建凭据', complete: '接入完成', error: '发生错误'
    };
    oauthSessionState.textContent = labels[session.status] || session.status;
    if (session.exit_ip) oauthRouteLabel.textContent = session.proxy + ' · EXIT ' + session.exit_ip + ' → Cloudflare Token Wizard';
    if (session.status === 'error') {
      setStatus(oauthStatus, 'error', session.error || '授权失败，请重试。');
      return;
    }
    if (session.status === 'waiting_authorization') setStatus(oauthStatus, 'checking', session.error || '专用 Chrome 出口验证成功。请在 Cloudflare 官方页创建临时引导 Token，再将 Token 与 Account 页面地址粘贴回本机控制页。');
    if (session.status === 'discovering') setStatus(oauthStatus, 'checking', '正在经同一代理验证引导 Token，并读取账号与 R2 Bucket…');
    if (session.status === 'authorized') {
      discoveredAccounts = session.accounts || [];
      renderAccounts();
      oauthCompleteForm.hidden = false;
      oauthLaunchButton.disabled = true;
      setStatus(oauthStatus, 'success', '发现完成。请选择要接入的 Bucket。');
      oauthCompleteForm.scrollIntoView({behavior: 'smooth', block: 'nearest'});
    }
  }

  function renderAccounts() {
    oauthAccount.replaceChildren();
    discoveredAccounts.forEach(function (account) {
      const option = document.createElement('option');
      option.value = account.id;
      option.textContent = account.name + ' · ' + account.id.slice(0, 8);
      option.disabled = !account.buckets || account.buckets.length === 0;
      oauthAccount.appendChild(option);
    });
    renderBuckets();
  }

  function renderBuckets() {
    oauthBucket.replaceChildren();
    const account = discoveredAccounts.find(function (item) { return item.id === oauthAccount.value; });
    (account && account.buckets || []).forEach(function (bucket) {
      const option = document.createElement('option');
      option.value = bucket.name;
      option.textContent = bucket.name + ' · ' + (bucket.jurisdiction || 'default');
      oauthBucket.appendChild(option);
    });
    oauthCompleteButton.disabled = oauthBucket.options.length === 0;
  }

  function setStatus(element, state, message) {
    if (!element) return;
    element.className = 'account-add-status' + (state ? ' ' + state : '');
    element.textContent = message;
  }

  const accountStateDialog = document.getElementById('account-state-dialog');
  const accountStateForm = document.getElementById('account-state-form');
  const accountStateID = document.getElementById('account-state-id');
  const accountStatePaused = document.getElementById('account-state-paused');
  const accountStateLabel = document.getElementById('account-state-label');
  const accountStateTitle = document.getElementById('account-state-title');
  const accountStateDescription = document.getElementById('account-state-description');
  const accountStateStatus = document.getElementById('account-state-status');
  const accountStateSubmit = document.getElementById('account-state-submit');
  const accountStateCancel = document.getElementById('account-state-cancel');

  if (accountStateDialog && accountStateForm) {
    document.querySelectorAll('.cf-account-state').forEach(function (button) {
      button.addEventListener('click', function () {
        const shouldPause = button.dataset.paused !== 'true';
        accountStateID.value = button.dataset.accountId || '';
        accountStatePaused.value = shouldPause ? 'true' : 'false';
        accountStateLabel.textContent = button.dataset.accountLabel || button.dataset.accountId || '';
        accountStateTitle.textContent = shouldPause ? '暂停账号写入' : '恢复账号写入';
        accountStateDescription.textContent = shouldPause
          ? '该账号下的全部 R2 Bucket 将立即退出新对象调度。'
          : '该账号下的全部 R2 Bucket 将重新进入新对象调度。';
        const impact = accountStateDialog.querySelectorAll('.account-state-impact div');
        const messages = shouldPause ? [
          ['停止', '新建直传、S3 写入与自动副本目标'],
          ['保留', '已有对象读取、签名、列举和上游删除'],
          ['注意', '已签发的 R2 临时 URL 会持续到原过期时间，无法提前撤销']
        ] : [
          ['恢复', '新的上传请求可以再次选择该账号'],
          ['不迁移', '已有对象保持原位置，不会触发自动搬运'],
          ['校验', '实际分配仍受健康状态、安全容量与路由策略约束']
        ];
        impact.forEach(function (row, index) {
          row.querySelector('b').textContent = messages[index][0];
          row.querySelector('span').textContent = messages[index][1];
        });
        accountStateSubmit.textContent = shouldPause ? '确认暂停' : '恢复写入';
        accountStateSubmit.classList.toggle('btn-danger', shouldPause);
        accountStateSubmit.classList.toggle('btn-primary', !shouldPause);
        accountStateDialog.classList.toggle('resume-mode', !shouldPause);
        accountStateSubmit.disabled = false;
        setStatus(accountStateStatus, '', '');
        accountStateDialog.showModal();
      });
    });

    accountStateCancel.addEventListener('click', function () { accountStateDialog.close(); });
    accountStateForm.addEventListener('submit', function (event) {
      event.preventDefault();
      const shouldPause = accountStatePaused.value === 'true';
      accountStateSubmit.disabled = true;
      setStatus(accountStateStatus, 'checking', shouldPause ? '正在停止该账号的新对象分配…' : '正在恢复该账号的新对象分配…');
      postJSON('api/r2-account-state', {account_id: accountStateID.value, paused: shouldPause})
        .then(function (body) {
          const count = Array.isArray(body.accounts) ? body.accounts.length : 0;
          setStatus(accountStateStatus, 'success', (shouldPause ? '已暂停 ' : '已恢复 ') + count + ' 个 Backend，状态已持久化。');
          setTimeout(function () { location.reload(); }, 700);
        }).catch(function (error) {
          setStatus(accountStateStatus, 'error', error.message);
          accountStateSubmit.disabled = false;
        });
    });
  }

  const proxyDialog = document.getElementById('proxy-dialog');
  const proxyForm = document.getElementById('proxy-form');
  if (!proxyDialog || !proxyForm) return;
  const proxyAccountID = document.getElementById('proxy-account-id');
  const proxyAccountLabel = document.getElementById('proxy-account-label');
  const proxyCurrentValue = document.getElementById('proxy-current-value');
  const proxyURL = document.getElementById('proxy-url');
  const proxyStatus = document.getElementById('proxy-status');
  const proxySubmit = document.getElementById('proxy-submit');
  const proxyCancel = document.getElementById('proxy-cancel');

  document.querySelectorAll('.cf-proxy-edit').forEach(function (button) {
    button.addEventListener('click', function () {
      proxyAccountID.value = button.dataset.accountId || '';
      proxyAccountLabel.textContent = button.dataset.accountLabel || button.dataset.accountId || '';
      proxyCurrentValue.textContent = button.dataset.currentProxy || '直连';
      proxyURL.value = '';
      setStatus(proxyStatus, '', '');
      proxySubmit.disabled = false;
      proxySubmit.textContent = '验证并应用';
      proxyDialog.showModal();
      proxyURL.focus();
    });
  });
  proxyCancel.addEventListener('click', function () { proxyDialog.close(); });
  proxyURL.addEventListener('input', function () { proxySubmit.textContent = proxyURL.value.trim() ? '验证并应用' : '恢复服务器直连'; });
  proxyForm.addEventListener('submit', function (event) {
    event.preventDefault();
    proxySubmit.disabled = true;
    setStatus(proxyStatus, 'checking', '正在经由新出口验证该账号下全部 Bucket…');
    postJSON('api/r2-account-proxy', {account_id: proxyAccountID.value, proxy_url: proxyURL.value})
      .then(function (body) {
        const count = Array.isArray(body.accounts) ? body.accounts.length : 0;
        setStatus(proxyStatus, 'success', '验证通过，已热切换 ' + count + ' 个 Backend。浏览器接入账号会使用新的隔离 Profile。');
        setTimeout(function () { location.reload(); }, 900);
      }).catch(function (error) {
        setStatus(proxyStatus, 'error', error.message);
        proxySubmit.disabled = false;
      });
  });
})();
