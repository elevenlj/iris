let irisBots = [];
let editingBotID = "";
let botSavePending = false;

async function loadBots() {
  irisBots = await api('/api/bots');
  const selected = BOT_BASE.split('/')[2] || 'default';
  const select = $('bot-select');
  select.replaceChildren();
  for (const bot of irisBots) {
    const option = document.createElement('option');
    option.value = bot.id;
    option.textContent = bot.name;
    select.appendChild(option);
  }
  select.value = selected;
  $('bot-settings').disabled = !irisBots.some(bot => bot.id === selected);
}

function botError(error) { $('bot-error').textContent = error.message || String(error); }

async function openBotEditor(edit) {
  if (!await ensureSettingsAccess(false)) return;
  await loadBots();
  const bot = edit ? irisBots.find(bot => bot.id === ($('bot-select').value || 'default')) : null;
  editingBotID = bot?.id || '';
  $('bot-dialog-title').textContent = edit ? '机器人设置' : '添加机器人';
  $('bot-name').value = bot?.name || '';
  renderAgentSelect($('bot-agent'), bot?.default_agent_id || state.config.default_agent_id);
  $('bot-directory').value = bot?.default_workspace_dir || state.config.default_workspace_dir || '';
  $('bot-app-name').value = bot?.app_name || '';
  $('bot-app-id').value = bot?.app_id || '';
  $('bot-app-secret').value = bot?.app_secret || '';
  $('bot-app-secret').type = 'password';
  $('bot-show-secret').textContent = '显示 Secret';
  $('bot-receive-id').value = bot?.receive_id || '';
  $('bot-app-fields').hidden = !edit;
  $('bot-create-methods').hidden = edit;
  $('bot-error').textContent = '';
  $('bot-save').textContent = edit ? '保存' : '创建';
  $('bot-save').hidden = !edit;
  $('bot-scan').disabled = false;
  updateBotConsole();
  $('bot-dialog').showModal();
}

function readBot() {
  if (!$('bot-name').value.trim()) throw new Error('请填写机器人名称');
  if (!$('bot-agent').value) throw new Error('请选择可用的 Agent');
  return {
    id: editingBotID, name: $('bot-name').value.trim(),
    default_agent_id: $('bot-agent').value,
    default_workspace_dir: $('bot-directory').value.trim(),
    app_id: $('bot-app-id').value.trim(), app_secret: $('bot-app-secret').value.trim(),
    receive_id: $('bot-receive-id').value.trim(),
  };
}

async function saveBot() {
  if (botSavePending) return;
  const bot = readBot();
  botSavePending = true;
  $('bot-save').disabled = true;
  $('bot-error').textContent = editingBotID ? '正在保存…' : '正在验证连接和权限…';
  try {
    const result = await api('/api/bots', { method: editingBotID ? 'PATCH' : 'POST', body: JSON.stringify(bot) });
    $('bot-dialog').close();
    // Reloading closes the old WebSocket and prevents late responses from the
    // previous bot from replacing the newly selected bot's terminal.
    location.assign(result.id === 'default' ? '/' : `/bots/${result.id}/`);
  } finally { botSavePending = false; $('bot-save').disabled = false; }
}

async function createBotWithQR() {
  if (botSavePending) return;
  const bot = readBot();
  botSavePending = true;
  $('bot-scan').disabled = true;
  $('bot-error').textContent = '请在自动打开的飞书窗口扫码登录；登录后会自动创建应用、配置权限并验证连接。';
  try {
    const result = await api('/api/bots/create', {method:'POST',body:JSON.stringify(bot)});
    $('bot-dialog').close();
    location.assign(result.id === 'default' ? '/' : `/bots/${result.id}/`);
  } finally {
    botSavePending = false;
    $('bot-scan').disabled = false;
  }
}

function updateBotConsole() {
  const id = $('bot-app-id').value.trim();
  $('bot-console').href = larkAppConsoleURL(id);
}

$('bot-select').onchange = () => location.assign($('bot-select').value === 'default' ? '/' : `/bots/${encodeURIComponent($('bot-select').value)}/`);
$('bot-settings').onclick = () => openBotEditor(true).catch(botError);
$('bot-add').onclick = () => openBotEditor(false).catch(botError);
$('bot-form').onsubmit = event => {event.preventDefault();saveBot().catch(botError)};
$('bot-scan').onclick = () => createBotWithQR().catch(botError);
$('bot-existing').onclick = () => {$('bot-app-fields').hidden = false;$('bot-save').hidden = false;$('bot-create-methods').hidden = true};
$('bot-cancel').onclick = () => {if (!botSavePending) $('bot-dialog').close()};
$('bot-dialog').addEventListener('cancel', event => {if (botSavePending) event.preventDefault()});
$('bot-app-id').oninput = () => {$('bot-app-name').value='';updateBotConsole()};
$('bot-show-secret').onclick = () => {const show=$('bot-app-secret').type==='password';$('bot-app-secret').type=show?'text':'password';$('bot-show-secret').textContent=show?'隐藏 Secret':'显示 Secret'};
loadBots().catch(console.error);
