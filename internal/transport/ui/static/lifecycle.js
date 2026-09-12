(function () {
  'use strict';

  function csrfToken() {
    const match = /(?:^|; )s3orch_csrf=([^;]*)/.exec(document.cookie);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function postPolicy(payload) {
    return fetch('api/lifecycle-policies', {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken()},
      body: JSON.stringify(payload)
    }).then(async function (response) {
      const body = await response.json().catch(function () { return {}; });
      if (!response.ok) throw new Error(body.error || '请求失败（HTTP ' + response.status + '）');
      return body;
    });
  }

  function setStatus(element, state, message) {
    if (!element) return;
    element.className = 'form-status' + (state ? ' ' + state : '');
    element.textContent = message || '';
  }

  const createForm = document.getElementById('lifecycle-policy-form');
  if (createForm) {
    createForm.addEventListener('submit', function (event) {
      event.preventDefault();
      const button = document.getElementById('lifecycle-submit');
      const status = document.getElementById('lifecycle-status');
      const hours = Number(document.getElementById('lifecycle-hours').value);
      const acknowledged = document.getElementById('lifecycle-ack').checked;
      if (!Number.isInteger(hours) || hours < 1 || hours > 87600) {
        setStatus(status, 'error', '保留时间必须是 1–87600 之间的整数小时。');
        return;
      }
      if (!acknowledged) {
        setStatus(status, 'error', '请先确认永久删除影响。');
        return;
      }

      button.disabled = true;
      setStatus(status, 'checking', '正在保存策略…');
      postPolicy({
        action: 'create',
        bucket: document.getElementById('lifecycle-bucket').value,
        directory: document.getElementById('lifecycle-directory').value,
        expiration_hours: hours,
        acknowledged: acknowledged
      }).then(function (body) {
        setStatus(status, 'success', '策略已启用：' + body.policy.prefix + ' · ' + body.policy.expiration_hours + ' 小时');
        window.setTimeout(function () { location.reload(); }, 650);
      }).catch(function (error) {
        setStatus(status, 'error', error.message);
        button.disabled = false;
      });
    });
  }

  const dialog = document.getElementById('lifecycle-policy-dialog');
  const actionForm = document.getElementById('lifecycle-policy-action-form');
  if (!dialog || !actionForm) return;

  const actionID = document.getElementById('lifecycle-action-id');
  const actionName = document.getElementById('lifecycle-action-name');
  const actionEnabled = document.getElementById('lifecycle-action-enabled');
  const actionPrefix = document.getElementById('lifecycle-action-prefix');
  const actionTitle = document.getElementById('lifecycle-action-title');
  const actionDescription = document.getElementById('lifecycle-action-description');
  const actionAckWrap = document.getElementById('lifecycle-action-ack-wrap');
  const actionAck = document.getElementById('lifecycle-action-ack');
  const actionStatus = document.getElementById('lifecycle-action-status');
  const actionSubmit = document.getElementById('lifecycle-action-submit');

  function openAction(options) {
    actionID.value = options.id;
    actionName.value = options.action;
    actionEnabled.value = options.enabled ? 'true' : 'false';
    actionPrefix.textContent = options.prefix;
    actionTitle.textContent = options.title;
    actionDescription.textContent = options.description;
    actionSubmit.textContent = options.submit;
    actionSubmit.classList.toggle('btn-danger', options.action !== 'set_enabled' || !options.enabled);
    actionSubmit.classList.toggle('btn-primary', options.action === 'set_enabled' && options.enabled);
    actionAckWrap.hidden = !(options.action === 'set_enabled' && options.enabled);
    actionAck.checked = false;
    actionSubmit.disabled = false;
    setStatus(actionStatus, '', '');
    dialog.showModal();
  }

  document.querySelectorAll('.lifecycle-toggle').forEach(function (button) {
    button.addEventListener('click', function () {
      const enable = button.dataset.enabled !== 'true';
      openAction({
        id: button.dataset.id,
        action: 'set_enabled',
        enabled: enable,
        prefix: button.dataset.prefix,
        title: enable ? '恢复自动清理' : '暂停自动清理',
        description: enable
          ? '恢复后，已经超过保留窗口的对象可能在下一轮扫描中立即被永久删除。'
          : '暂停只阻止后续自动删除，不会恢复此前已经删除的对象。',
        submit: enable ? '确认恢复' : '确认暂停'
      });
    });
  });

  document.querySelectorAll('.lifecycle-remove').forEach(function (button) {
    button.addEventListener('click', function () {
      openAction({
        id: button.dataset.id,
        action: 'delete',
        enabled: false,
        prefix: button.dataset.prefix,
        title: '删除自动清理策略',
        description: '这里只删除策略本身，不会删除当前仍保留的对象；已删除对象无法恢复。',
        submit: '删除策略'
      });
    });
  });

  document.getElementById('lifecycle-action-cancel').addEventListener('click', function () {
    dialog.close();
  });

  actionForm.addEventListener('submit', function (event) {
    event.preventDefault();
    const action = actionName.value;
    const enabled = actionEnabled.value === 'true';
    if (action === 'set_enabled' && enabled && !actionAck.checked) {
      setStatus(actionStatus, 'error', '请确认恢复策略的永久删除影响。');
      return;
    }

    actionSubmit.disabled = true;
    setStatus(actionStatus, 'checking', action === 'delete' ? '正在删除策略…' : '正在更新策略状态…');
    postPolicy({
      action: action,
      id: Number(actionID.value),
      enabled: enabled,
      acknowledged: actionAck.checked
    }).then(function () {
      setStatus(actionStatus, 'success', action === 'delete' ? '策略已删除。' : (enabled ? '策略已恢复。' : '策略已暂停。'));
      window.setTimeout(function () { location.reload(); }, 550);
    }).catch(function (error) {
      setStatus(actionStatus, 'error', error.message);
      actionSubmit.disabled = false;
    });
  });
}());
