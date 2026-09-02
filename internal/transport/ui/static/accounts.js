(function () {
  'use strict';

  function csrfToken() {
    const match = /(?:^|; )s3orch_csrf=([^;]*)/.exec(document.cookie);
    return match ? match[1] : '';
  }

  function postJSON(url, payload) {
    return fetch(url, {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken()},
      body: JSON.stringify(payload)
    }).then(function (response) {
      return response.json().then(function (body) {
        return {ok: response.ok, status: response.status, body: body};
      });
    }).then(function (result) {
      if (!result.ok) throw new Error(result.body.error || ('HTTP ' + result.status));
      return result.body;
    });
  }

  const openButton = document.getElementById('account-add-btn');
  const dialog = document.getElementById('account-dialog');
  const form = document.getElementById('account-form');
  if (openButton && dialog && form) {
    const cancelButton = document.getElementById('account-cancel');
    const submitButton = document.getElementById('account-submit');
    const status = document.getElementById('account-add-status');
    const warning = document.getElementById('warning-percent');
    const warningOutput = document.getElementById('warning-output');

    openButton.addEventListener('click', function () {
      status.textContent = '';
      status.className = 'account-add-status';
      dialog.showModal();
    });
    cancelButton.addEventListener('click', function () { dialog.close(); });
    warning.addEventListener('input', function () { warningOutput.textContent = warning.value + '%'; });

    form.addEventListener('submit', function (event) {
      event.preventDefault();
      const values = new FormData(form);
      const payload = {
        name: values.get('name'),
        account_label: values.get('account_label'),
        account_id: values.get('account_id'),
        jurisdiction: values.get('jurisdiction'),
        bucket: values.get('bucket'),
        access_key_id: values.get('access_key_id'),
        secret_access_key: values.get('secret_access_key'),
        proxy_url: values.get('proxy_url'),
        warning_percent: Number(values.get('warning_percent'))
      };

      submitButton.disabled = true;
      status.className = 'account-add-status checking';
      status.textContent = '正在通过指定出口验证 R2 Bucket…';
      postJSON('api/r2-accounts', payload).then(function () {
        status.className = 'account-add-status success';
        status.textContent = '验证通过，账号已加入调度。正在刷新额度视图…';
        form.reset();
        setTimeout(function () { location.reload(); }, 700);
      }).catch(function (error) {
        status.className = 'account-add-status error';
        status.textContent = error.message;
      }).finally(function () {
        submitButton.disabled = false;
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
      proxyStatus.textContent = '';
      proxyStatus.className = 'account-add-status';
      proxySubmit.disabled = false;
      proxySubmit.textContent = '验证并应用';
      proxyDialog.showModal();
      proxyURL.focus();
    });
  });

  proxyCancel.addEventListener('click', function () { proxyDialog.close(); });
  proxyURL.addEventListener('input', function () {
    proxySubmit.textContent = proxyURL.value.trim() ? '验证并应用' : '恢复服务器直连';
  });

  proxyForm.addEventListener('submit', function (event) {
    event.preventDefault();
    proxySubmit.disabled = true;
    proxyStatus.className = 'account-add-status checking';
    proxyStatus.textContent = '正在经由新出口验证该账号下全部 Bucket…';
    postJSON('api/r2-account-proxy', {
      account_id: proxyAccountID.value,
      proxy_url: proxyURL.value
    }).then(function (body) {
      const count = Array.isArray(body.accounts) ? body.accounts.length : 0;
      proxyStatus.className = 'account-add-status success';
      proxyStatus.textContent = '验证通过，已热切换 ' + count + ' 个 Backend。';
      setTimeout(function () { location.reload(); }, 900);
    }).catch(function (error) {
      proxyStatus.className = 'account-add-status error';
      proxyStatus.textContent = error.message;
      proxySubmit.disabled = false;
    });
  });
})();
