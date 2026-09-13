let irisBots = [];
let editingBotID = "";
let botSavePending = false;
let botCreationBlocked = false;

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
  select.hidden = irisBots.length === 0;
  $('bot-settings').hidden = irisBots.length === 0;
  if (irisBots.length && !irisBots.some(bot => bot.id === selected)) {
    const first = irisBots[0].id;
    location.replace(first === 'default' ? '/' : `/bots/${encodeURIComponent(first)}/`);
  }
}

function botStatus(message, kind = '') {
  $('bot-error').textContent = message;
  $('bot-error').className = kind;
  $('bot-error').setAttribute('aria-busy', String(kind === 'pending'));
}
function botError(error) { botStatus(error.message || String(error), 'error'); }

function setBotPending(pending) {
  botSavePending = pending;
  for (const id of ['bot-scan','bot-link','bot-connect','bot-select-app','bot-existing','bot-save','bot-delete','bot-cancel','bot-name','bot-agent','bot-directory']) $(id).disabled = pending;
  $('bot-scan').disabled = pending || botCreationBlocked;
  if (!pending) {
    $('bot-error').classList.remove('pending');
    $('bot-error').setAttribute('aria-busy','false');
  }
}

function showManualBotFields(manual) {
  $('bot-app-fields').hidden = !manual;
  $('bot-save').hidden = !manual;
  $('bot-link-fields').hidden = true;
  botStatus('');
}

async function openBotEditor(edit) {
  if (!await ensureSettingsAccess(false)) return;
  await loadBots();
  const bot = edit ? irisBots.find(bot => bot.id === ($('bot-select').value || 'default')) : null;
  editingBotID = bot?.id || '';
  $('bot-delete').hidden = !editingBotID;
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
  botStatus('');
  $('bot-created-app').hidden = true;
  $('bot-link-fields').hidden = true;
  botCreationBlocked = false;
  setBotPending(false);
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
  setBotPending(true);
  botStatus(editingBotID ? '正在保存…' : '正在验证连接和权限…', 'pending');
  try {
    const result = await api('/api/bots', { method: editingBotID ? 'PATCH' : 'POST', body: JSON.stringify(bot) });
    $('bot-dialog').close();
    // Reloading closes the old WebSocket and prevents late responses from the
    // previous bot from replacing the newly selected bot's terminal.
    location.assign(result.id === 'default' ? '/' : `/bots/${result.id}/`);
  } finally { setBotPending(false); }
}

async function deleteBot() {
  if (botSavePending || !editingBotID) return;
  const id = editingBotID;
  const bot = irisBots.find(bot => bot.id === id);
  if (!bot) throw new Error('机器人已删除，请刷新页面');
  setBotPending(true);
  try {
    const info = await api(`/api/bots?delete_id=${encodeURIComponent(id)}`);
    if (!confirm(`删除机器人「${bot.name}」？\n\n共 ${info.sessions} 个会话，${info.running} 个运行中任务。所有所属会话将停止，群和话题绑定将移除。\n\n本地会话数据会先备份；项目文件、飞书应用、群聊及聊天记录不受影响。`)) return;
    botStatus('正在备份并删除…', 'pending');
    const result = await api('/api/bots', {method:'DELETE', body:JSON.stringify({id})});
    if (result.warning) alert(result.warning);
    const remaining = await api('/api/bots');
    const next = remaining[0]?.id;
    location.replace(!next || next === 'default' ? '/' : `/bots/${encodeURIComponent(next)}/`);
  } finally { setBotPending(false); }
}

async function streamBotSetup(bot, receive) {
  const response = await fetch('/api/bots/create', {method:'POST',headers:{'Content-Type':'application/json',Accept:'application/x-ndjson'},body:JSON.stringify(bot)});
  if (!response.ok) throw new Error((await response.json()).error || '请求失败');
  const reader = response.body.getReader(), decoder = new TextDecoder();
  let buffer = '';
  const line = text => { if (text.trim()) receive(JSON.parse(text)); };
  try {
    while (true) {
      const {value,done} = await reader.read();
      buffer += decoder.decode(value, {stream:!done});
      const lines = buffer.split('\n'); buffer = lines.pop();
      for (const text of lines) line(text);
      if (done) { line(buffer); break; }
    }
  } finally { await reader.cancel().catch(()=>{}); }
}

async function listExistingBots() {
  if (botSavePending) return;
  showManualBotFields(false);
  setBotPending(true);
  botStatus('正在检查登录状态…', 'pending');
  let apps;
  try {
    await streamBotSetup({list_apps:true}, event => {
      if (event.stage === 'error') throw new Error(event.error);
      if (event.message) botStatus(event.message,'pending');
      if (event.stage === 'apps') apps = event.apps || [];
    });
    if (!apps) throw new Error('连接已中断，请重试读取应用列表');
    const select = $('bot-select-app'); select.replaceChildren();
    for (const app of apps) {
      if (irisBots.some(bot=>bot.app_id===app.app_id)) continue;
      const option = document.createElement('option');
      option.value = app.app_id; option.textContent = `${app.name} (${app.app_id})`;
      option.dataset.name = app.name; select.appendChild(option);
    }
    $('bot-link-fields').hidden = select.options.length === 0;
    botStatus(select.options.length ? '请选择要关联的应用' : '当前账号没有可关联的新应用');
  } finally { setBotPending(false); }
}

async function createBot(connect = false) {
  if (botSavePending || (!connect && botCreationBlocked)) return;
  if (!connect) showManualBotFields(false);
  const bot = readBot();
  bot.app_id = connect ? $('bot-select-app').value : '';
  if (connect && !bot.app_id) throw new Error('请选择已有应用');
  setBotPending(true);
  botStatus('正在检查登录状态…', 'pending');
  try {
    let resultID = '';
    const receive = event => {
      if (event.app_id && /^cli_[a-zA-Z0-9]+$/.test(event.app_id)) {
        // Preserve this guard across creation-method switches after a failure.
        botCreationBlocked = true;
        $('bot-created-app').href = larkAppConsoleURL(event.app_id);
        $('bot-created-app').textContent = connect ? '查看应用 ↗' : '查看已创建的应用 ↗';
        $('bot-created-app').hidden = false;
        $('bot-app-id').value = event.app_id;
        updateBotConsole();
      }
      if (event.stage === 'error') throw new Error(event.error);
      if (event.stage === 'done') resultID = event.bot_id;
      if (event.message) botStatus(event.message, 'pending');
    };
    await streamBotSetup(bot, receive);
    if (!resultID) throw new Error('连接已中断，创建结果尚未确认，请先查看应用后台，勿重复创建。');
    botStatus(connect ? '关联完成' : '创建完成');
    location.assign(resultID === 'default' ? '/' : `/bots/${encodeURIComponent(resultID)}/`);
  } finally {
    setBotPending(false);
  }
}

function updateBotConsole() {
  const id = $('bot-app-id').value.trim();
  $('bot-console').href = larkAppConsoleURL(id);
}

$('bot-select').onchange = () => location.assign($('bot-select').value === 'default' ? '/' : `/bots/${encodeURIComponent($('bot-select').value)}/`);
$('bot-settings').onclick = () => openBotEditor(true).catch(botError);
$('bot-delete').onclick = () => deleteBot().catch(botError);
$('bot-add').onclick = () => openBotEditor(false).catch(botError);
$('bot-form').onsubmit = event => {
  event.preventDefault();
  if (botSavePending) return;
  if (!$('bot-save').hidden) saveBot().catch(botError);
  else if (!$('bot-link-fields').hidden) $('bot-connect').click();
  else if (!$('bot-scan').disabled) createBot().catch(botError);
};
$('bot-scan').onclick = () => createBot().catch(botError);
$('bot-link').onclick = () => listExistingBots().catch(botError);
$('bot-connect').onclick = () => {
  if (!$('bot-name').value.trim()) $('bot-name').value = $('bot-select-app').selectedOptions[0]?.dataset.name || '';
  createBot(true).catch(botError);
};
$('bot-existing').onclick = () => {if (!botSavePending) showManualBotFields(true)};
$('bot-cancel').onclick = () => {if (!botSavePending) $('bot-dialog').close()};
$('bot-dialog').addEventListener('cancel', event => {if (botSavePending) event.preventDefault()});
$('bot-app-id').oninput = () => {$('bot-app-name').value='';updateBotConsole()};
$('bot-show-secret').onclick = () => {const show=$('bot-app-secret').type==='password';$('bot-app-secret').type=show?'text':'password';$('bot-show-secret').textContent=show?'隐藏 Secret':'显示 Secret'};
loadBots().catch(console.error);
