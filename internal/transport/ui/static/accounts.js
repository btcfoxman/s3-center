(function () {
  'use strict';

  const openButton = document.getElementById('account-add-btn');
  const dialog = document.getElementById('account-dialog');
  const form = document.getElementById('account-form');
  if (!openButton || !dialog || !form) return;

  const cancelButton = document.getElementById('account-cancel');
  const submitButton = document.getElementById('account-submit');
  const status = document.getElementById('account-add-status');
  const warning = document.getElementById('warning-percent');
  const warningOutput = document.getElementById('warning-output');

  function csrfToken() {
    const match = /(?:^|; )s3orch_csrf=([^;]*)/.exec(document.cookie);
    return match ? match[1] : '';
  }

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
    fetch('api/r2-accounts', {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken()},
      body: JSON.stringify(payload)
    }).then(function (response) {
      return response.json().then(function (body) { return {ok: response.ok, status: response.status, body: body}; });
    }).then(function (result) {
      if (!result.ok) throw new Error(result.body.error || ('HTTP ' + result.status));
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
})();
