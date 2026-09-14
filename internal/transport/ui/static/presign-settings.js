(function () {
  'use strict';

  const openButton = document.getElementById('presign-settings-btn');
  const dialog = document.getElementById('presign-settings-dialog');
  const form = document.getElementById('presign-settings-form');
  if (!openButton || !dialog || !form) return;

  const input = document.getElementById('presign-hours');
  const current = document.getElementById('presign-current-hours');
  const defaultValue = document.getElementById('presign-default-hours');
  const maximum = document.getElementById('presign-maximum-hours');
  const source = document.getElementById('presign-source');
  const chip = document.getElementById('presign-settings-chip');
  const status = document.getElementById('presign-settings-status');
  const saveButton = document.getElementById('presign-save');
  const resetButton = document.getElementById('presign-reset');
  const cancelButton = document.getElementById('presign-cancel');
  const presetButtons = Array.from(dialog.querySelectorAll('.presign-presets button'));
  let productDefault = 3;
  let maximumHours = 168;
  let requestInFlight = false;
  let issuedRequest = 0;

  function csrfToken() {
    const match = /(?:^|; )s3orch_csrf=([^;]*)/.exec(document.cookie);
    return match ? decodeURIComponent(match[1]) : '';
  }

  function setStatus(state, message) {
    status.className = 'form-status presign-settings-status' + (state ? ' ' + state : '');
    status.textContent = message || '';
  }

  async function requestSettings(options) {
    const response = await fetch('api/presign-settings', options);
    const body = await response.json().catch(function () { return {}; });
    if (!response.ok) throw new Error(body.error || '请求失败（HTTP ' + response.status + '）');
    return body;
  }

  function sourceLabel(value) {
    return value === 'management' ? '管理端动态设置' : '部署配置';
  }

  function markPreset(hours) {
    presetButtons.forEach(function (button) {
      button.classList.toggle('active', Number(button.dataset.hours) === hours);
    });
  }

  function render(settings, syncInput) {
    const hours = Number(settings.access_url_expiration_hours);
    productDefault = Number(settings.default_hours) || 3;
    maximumHours = Number(settings.maximum_hours) || 168;
    current.textContent = String(hours);
    defaultValue.textContent = productDefault + ' 小时';
    maximum.textContent = maximumHours + ' 小时 / 7 天';
    source.textContent = sourceLabel(settings.source);
    chip.textContent = hours + 'H';
    chip.setAttribute('aria-label', '当前有效期 ' + hours + ' 小时');
    input.max = String(maximumHours);
    if (syncInput) input.value = String(hours);
    markPreset(Number(input.value));
  }

  async function loadSettings(syncInput) {
    const requestID = ++issuedRequest;
    const settings = await requestSettings({headers: {'Accept': 'application/json'}});
    if (requestID === issuedRequest) render(settings, syncInput);
    return settings;
  }

  function setBusy(busy) {
    requestInFlight = busy;
    saveButton.disabled = busy;
    resetButton.disabled = busy;
    input.disabled = busy;
    presetButtons.forEach(function (button) { button.disabled = busy; });
  }

  async function save(hours, message) {
    if (requestInFlight) return;
    if (!Number.isInteger(hours) || hours < 1 || hours > maximumHours) {
      setStatus('error', '有效期必须是 1–' + maximumHours + ' 之间的整数小时。');
      input.focus();
      return;
    }
    setBusy(true);
    setStatus('checking', '正在更新授权策略…');
    const requestID = ++issuedRequest;
    try {
      const settings = await requestSettings({
        method: 'POST',
        headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrfToken()},
        body: JSON.stringify({access_url_expiration_hours: hours})
      });
      if (requestID === issuedRequest) render(settings, true);
      setStatus('success', message || '已保存；之后签发的新链接立即使用 ' + hours + ' 小时有效期。');
    } catch (error) {
      setStatus('error', error.message);
    } finally {
      setBusy(false);
    }
  }

  openButton.addEventListener('click', function () {
    setBusy(true);
    setStatus('checking', '正在读取当前策略…');
    dialog.showModal();
    loadSettings(true).then(function () {
      setStatus('', '');
      setBusy(false);
      input.focus();
      input.select();
    }).catch(function (error) {
      setStatus('error', error.message);
      setBusy(false);
    });
  });

  cancelButton.addEventListener('click', function () { dialog.close(); });

  form.addEventListener('submit', function (event) {
    event.preventDefault();
    save(Number(input.value));
  });

  resetButton.addEventListener('click', function () {
    save(productDefault, '已恢复产品默认值：新签发链接有效期为 ' + productDefault + ' 小时。');
  });

  presetButtons.forEach(function (button) {
    button.addEventListener('click', function () {
      input.value = button.dataset.hours;
      markPreset(Number(input.value));
      setStatus('', '点击“保存并立即生效”应用此有效期。');
    });
  });
  input.addEventListener('input', function () { markPreset(Number(input.value)); });

  dialog.addEventListener('click', function (event) {
    if (event.target === dialog && !requestInFlight) dialog.close();
  });

  loadSettings(false).catch(function () {
    chip.textContent = '设置';
    chip.setAttribute('aria-label', '打开临时链接设置');
  });
}());
