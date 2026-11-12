let irisBots = [];
let editingBotID = "";
let botSavePending = false;
let botCreationBlocked = false;
let botRegistrationAbort = null;

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
  for (const id of ['bot-scan','bot-link','bot-connect','bot-select-app','bot-existing','bot-save','bot-delete','bot-cancel','bot-name','bot-agent','bot-directory','bot-system-prompt']) $(id).disabled = pending;
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
  $('bot-system-prompt').value = bot?.system_prompt || '';
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
  $('bot-open').hidden = true;
  $('bot-registration').hidden = true;
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
    system_prompt: $('bot-system-prompt').value,
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
    await finishBotSave(result);
  } finally { setBotPending(false); }
}

async function finishBotSave(result) {
  const target = result.id === 'default' ? '/' : `/bots/${encodeURIComponent(result.id)}/`;
  if (!result.warning) {
    $('bot-dialog').close();
    location.assign(target);
    return;
  }
  editingBotID = result.id;
  if (!$('bot-app-secret').value) {
    try {
      const saved = (await api('/api/bots')).find(bot => bot.id === result.id);
      if (saved) {
        $('bot-app-id').value = saved.app_id || '';
        $('bot-app-secret').value = saved.app_secret || '';
        $('bot-receive-id').value = saved.receive_id || '';
        $('bot-app-name').value = saved.app_name || '';
      }
    } catch (error) { console.error(error); }
  }
  if (result.app_name) $('bot-app-name').value = result.app_name;
  $('bot-dialog-title').textContent = '机器人已保存';
  $('bot-create-methods').hidden = true;
  $('bot-app-fields').hidden = false;
  $('bot-save').hidden = false;
  $('bot-save').textContent = '保存';
  $('bot-created-app').href = larkAppConsoleURL(result.app_id);
  $('bot-created-app').textContent = '前往申请权限 ↗';
  $('bot-created-app').hidden = false;
  $('bot-open').href = target;
  $('bot-open').hidden = false;
  botStatus('已保存，可继续使用。\n'+result.warning,'warning');
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
  let registrationFallback = false;
  try {
    let resultID = '';
    let warning = '';
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
      if (event.stage === 'error') {
        registrationFallback = event.registration_fallback === true;
        throw new Error(event.error);
      }
      if (event.stage === 'done') { resultID = event.bot_id; warning = event.warning || ''; }
      if (event.message) botStatus(event.message, 'pending');
    };
    await streamBotSetup(bot, receive);
    if (!resultID) throw new Error('连接已中断，创建结果尚未确认，请先查看应用后台，勿重复创建。');
    botStatus(connect ? '关联完成' : '创建完成');
    await finishBotSave({id:resultID,app_id:$('bot-app-id').value,warning});
  } catch (error) {
    if (!connect && !botCreationBlocked && registrationFallback) {
      await registerBotWithoutBrowser(bot);
    } else {
      throw error;
    }
  } finally {
    setBotPending(false);
  }
}

async function registerBotWithoutBrowser(bot) {
  const controller = new AbortController();
  botRegistrationAbort = controller;
  botCreationBlocked = true;
  $('bot-cancel').disabled = false;
  botStatus('自动创建不可用，正在生成飞书确认二维码…', 'pending');
  try {
    const reg = await api('/api/lark-app-registration', {method:'POST',body:JSON.stringify({brand:'feishu'}),signal:controller.signal});
    $('bot-registration-link').href = reg.verification_uri_complete;
    $('bot-registration-qr').src = `/api/lark-app-registration/qr?text=${encodeURIComponent(reg.verification_uri_complete)}`;
    $('bot-registration').hidden = false;
    botStatus('请扫码或打开链接，在飞书确认创建；确认后自动检查功能权限。', 'pending');
    const expiresAt = Date.now() + Number(reg.expires_in || 3600) * 1000;
    while (!controller.signal.aborted) {
      if (Date.now() >= expiresAt) throw new Error('飞书确认已过期，请关闭后重新开始。');
      await new Promise((resolve,reject) => {
        const abort = () => {clearTimeout(timer);reject(new Error('已取消飞书确认'));};
        const timer = setTimeout(() => {controller.signal.removeEventListener('abort',abort);resolve();},Math.max(Number(reg.interval || 5),2)*1000);
        controller.signal.addEventListener('abort',abort,{once:true});
        if (controller.signal.aborted) abort();
      });
      const result = await api('/api/lark-app-registration/poll', {method:'POST',body:JSON.stringify({brand:reg.brand,device_code:reg.device_code}),signal:controller.signal});
      if (result.pending) continue;
      if (!/^cli_[a-zA-Z0-9]+$/.test(result.app_id || '') || !result.app_secret || !result.lark_notify_receive_id) throw new Error('飞书返回的应用信息不完整，请先检查应用后台，勿重复创建。');
      bot.app_id = result.app_id;
      bot.app_secret = result.app_secret;
      bot.receive_id = result.lark_notify_receive_id;
      $('bot-app-id').value = bot.app_id;
      $('bot-app-secret').value = bot.app_secret;
      $('bot-receive-id').value = bot.receive_id;
      $('bot-created-app').href = larkAppConsoleURL(bot.app_id);
      $('bot-created-app').textContent = '查看已创建的应用 ↗';
      $('bot-created-app').hidden = false;
      $('bot-registration').hidden = true;
      showManualBotFields(true);
      updateBotConsole();
      botStatus('应用已创建，正在验证凭证和功能权限…', 'pending');
      const saved = await api('/api/bots', {method:'POST',body:JSON.stringify(bot),signal:controller.signal});
      await finishBotSave(saved);
      return;
    }
    if (controller.signal.aborted) botStatus('已取消飞书确认');
  } catch (error) {
    if (!controller.signal.aborted) throw error;
    botStatus('已取消飞书确认');
  } finally {
    botRegistrationAbort = null;
    $('bot-registration').hidden = true;
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
$('bot-cancel').onclick = () => {if (botRegistrationAbort) botRegistrationAbort.abort();else if (!botSavePending) $('bot-dialog').close()};
$('bot-dialog').addEventListener('cancel', event => {if (botSavePending) {event.preventDefault();botRegistrationAbort?.abort()}});
$('bot-app-id').oninput = () => {$('bot-app-name').value='';updateBotConsole()};
$('bot-show-secret').onclick = () => {const show=$('bot-app-secret').type==='password';$('bot-app-secret').type=show?'text':'password';$('bot-show-secret').textContent=show?'隐藏 Secret':'显示 Secret'};
loadBots().catch(console.error);
