"use strict";
function date(value){if(!value||value.startsWith("0001"))return "—";return new Date(value).toLocaleString("zh-CN",{hour12:false});}
function render(s,syncTargets=false){
  if(syncTargets&&!dirty&&!busy&&Array.isArray(s.group_ids)){savedConfig.group_ids=[...s.group_ids];$("group_ids").value=s.group_ids.join(", ");}
  const configuredTargets=s.configured_targets||(savedConfig.group_ids?.length?null:s.targets);
  if(syncTargets&&loaded&&!dirty&&!busy&&Array.isArray(configuredTargets)&&JSON.stringify(configuredTargets)!==JSON.stringify(Array.from(configured.values()))){
    configured=new Map(configuredTargets.map(t=>[t.account_id,{...t}]));drafts=new Map(configuredTargets.map(t=>[t.account_id,{...t}]));savedConfig.targets=configuredTargets.map(t=>({...t}));
    $("targets").replaceChildren();accountSignature="";
  }
  lastStatus=s;
  const pendingGroups=s.group_sync_pending_ids||[];
  $("groupSyncStatus").hidden=!savedConfig.group_ids?.length;
  $("groupSyncStatus").textContent=savedConfig.group_ids?.length?`分组同步管理 ${s.target_count||0} 个账号${pendingGroups.length?`；${pendingGroups.length} 个新成员等待配置：${pendingGroups.join(", ")}`:""}。${s.group_sync_error||s.accounts_error?"同步未确认，当前目标数可能是上次快照；不按旧成员关系恢复。"+(s.group_sync_error||""):""}`:"";
  models=s.models||models;
  statusCatalog=s.status_catalog||statusCatalog;
  defaults=s.defaults||defaults;
  updateConnection(s);
  $("lastError").textContent=s.runtime_error||s.storage_error||s.last_error||s.accounts_error||"";$("lastError").hidden=!$("lastError").textContent;
  $("bankVersion").textContent=s.detector_version||"";
  $("accountError").textContent=s.accounts_error||"";$("accountError").hidden=!$("accountError").textContent;
  $("accountSummary").textContent=s.accounts_loading?"正在读取账号…":s.accounts_error?"账号加载失败":`已读取 ${s.accounts?.length??s.accounts_count??accounts.length} 个账号 · 仅显示当前密钥授权范围${(s.accounts?.length||0)>6?" · 可搜索或滚动选择":""}`;
  // New runtimes provide a content revision. The small-field legacy fallback
  // keeps older hosts compatible without serializing the entire account payload.
  const source=Array.isArray(s.accounts)?s.accounts:accounts;
  const revision=s.accounts_revision??source.map(a=>[a.id,a.name,a.type,a.status,a.schedulable,a.supported,a.suggested_model,a.suggested_expected].join("\0")).join("\n");
  const signature=[revision,models.map(m=>m.id+":"+m.display_name).join("|"),s.api_bridge_ready,s.api_bridge_state,s.api_bridge_message,defaults.oauth_model,defaults.oauth_expected,defaults.api_model,defaults.api_expected].join("\0");
  if(signature!==accountSignature){accounts=source;accountsByID=new Map(accounts.map(a=>[a.id,a]));accountSignature=signature;renderAccounts();}
  manualBusy=!!s.manual_busy;batchBusy=!!s.batch_busy||!!s.active_samples?.length||!!s.active_sample;renderRun(s);
  updateControls();
  renderRecords(s);updateFlow();
  const state=s.state||{};
  renderEvents(state);
  $("windowStats").hidden=!s.batch?.window_seconds;
  setText($("windowStats"),s.batch?.window_seconds?`近 1 小时 · 完成 ${s.batch.completed||0} · 需关注 ${s.batch.errors||0}（其中不确定 ${s.batch.inconclusive||0}） · 进行中 ${s.batch.active_ids?.length||s.active_samples?.length||0}`:"");
}
function candidateSummary(record) {
  const candidates = (record.analysis?.candidates || []).slice(0, 3);
  return candidates.map(candidate => {
    const name = candidate.name || candidate.model || candidate.id || "未知参考类";
    const probability = Number(candidate.probability ?? candidate.confidence ?? 0);
    return `${shortModel(String(name))} ${Math.max(0, Math.min(100, probability * 100)).toFixed(1).replace(/\.0$/, "")}%`;
  }).join(" / ");
}

function renderEvents(state) {
  const activity = event => event.action === "check" || event.action?.startsWith("sample_");
  const audit = Array.isArray(state.audit_events) ? state.audit_events : (state.events || []).filter(event => !activity(event));
  const samples = Array.isArray(state.activity_events) ? state.activity_events : (state.events || []).filter(activity);
  const events = [...audit, ...($("showSamples").checked ? samples : [])]
    .sort((a, b) => Date.parse(a.at) - Date.parse(b.at)).slice(-40).reverse();
  const nodes = [], occurrences = new Map();
  for (const event of events) {
    const base = event.id || `${event.at}|${event.account_id}|${event.action}|${event.reason}`;
    const n = occurrences.get(base) || 0;
    occurrences.set(base, n + 1);
    const key = `${base}|${n}`;
    let row = eventRows.get(key);
    if (!row) {
      row = document.createElement("div");
      row.className = "event";
      row.append(document.createElement("time"), document.createElement("span"), document.createElement("span"));
      eventRows.set(key, row);
    }
    setText(row.children[0], date(event.at));
    setText(row.children[1], accountsByID.get(event.account_id)?.name || (event.account_id ? `账号 #${event.account_id}` : "系统"));
    setText(row.children[2], event.reason);
    nodes.push(row);
  }
  if (nodes.length) syncRows($("events"), nodes, eventRows);
  else {
    eventRows.clear();
    let empty = $("events").querySelector(".empty");
    if (!empty) { empty = document.createElement("p"); empty.className = "empty"; }
    setText(empty, $("showSamples").checked ? "暂无记录" : "暂无暂停、恢复或配置变更记录");
    syncRows($("events"), [empty]);
  }
}

function renderRecords(s) {
  const state = s.state || {}, saved = new Set(activeTargets(s).filter(target => target.enabled).map(target => target.account_id));
  const selected = new Set(saved), unsaved = new Set();
  if (dirty && !savedConfig.group_ids?.length) for (const target of drafts.values()) {
    if (target.enabled && !saved.has(target.account_id)) { selected.add(target.account_id); unsaved.add(target.account_id); }
  }
  const all = Object.values(state.records || {});
  const records = new Map(all.filter(record => selected.has(record.account_id) || record.hold || record.pending).map(record => [record.account_id, record]));
  for (const id of selected) if (!records.has(id)) records.set(id, {account_id: id, status: "unverified"});
  const rows = Array.from(records.values()).sort((a, b) => a.account_id - b.account_id);
  const active = new Map((s.active_samples || (s.active_sample ? [s.active_sample] : [])).map(sample => [sample.account_id, sample]));
  const queued = new Set(s.batch?.queued_ids || []);
  setText($("targetCount"), s.target_count ?? saved.size);
  setText($("holdCount"), rows.filter(record => record.hold).length);
  setText($("recoverCount"), rows.filter(record => record.hold && record.recovery_passes > 0).length);
  setText($("requestCount"), s.budget?.used ?? state.requests ?? 0);
  setText($("budgetLimit"), s.budget ? ` / ${s.budget.limit}` : "");
  for (const id of ["holdCount", "recoverCount"]) $(id).parentElement.hidden = Number($(id).textContent) === 0;
  const nodes = [];
  for (const record of rows) {
    const id = record.account_id, account = accountsByID.get(id), sample = active.get(id);
    let row = monitorRows.get(id);
    if (!row) {
      row = document.createElement("tr");
      row.dataset.accountId = String(id);
      const nameCell = cell(row, ""), title = document.createElement("span"), accountID = document.createElement("span");
      accountID.className = "account-id";
      nameCell.append(title, accountID);
      const scheduling = cell(row, ""), result = cell(row, "");
      const tag = document.createElement("span"), model = document.createElement("span"), candidates = document.createElement("span");
      candidates.className = "candidate-list";
      const details = document.createElement("details"), summary = document.createElement("summary"), text = document.createElement("p");
      details.className = "record-details";
      details.dataset.recordDetails = String(id);
      summary.textContent = "详情";
      details.append(summary, text);
      result.append(tag, model, candidates, details);
      const next = cell(row, "");
      row._parts = {title, accountID, scheduling, tag, model, candidates, details, text, next};
      monitorRows.set(id, row);
    }
    const p = row._parts, category = statusCatalog[record.status]?.category;
    row.dataset.search = `${account?.name || ""} ${id}`.toLowerCase();
    row.dataset.held = String(!!record.hold);
    row.dataset.active = String(!!sample || queued.has(id));
    row.dataset.attention = String(!!record.hold || category === "attention" || ["unavailable", "inconclusive", "blocked", "unsupported", "external_change", "pause_uncertain", "pause_recommended", "suspect"].includes(record.status));
    setText(p.title, account?.name || `账号 #${id}`);
    setText(p.accountID, `#${id}`);
    setText(p.scheduling, account ? `${s.accounts_error ? "上次：" : ""}${account.status !== "active" ? account.status : account.schedulable ? "可调度" : record.hold ? "本插件暂停" : "外部暂停"}` : "待同步");
    let label = statusLabel(record.status), reason = record.reason || "", next = nextAction(record, s, saved);
    if (record.status === "passed" && record.analysis?.match_scope === "family") label = "家族一致";
    if (record.status === "unavailable" && record.hold) label = "复测失败";
    if (record.status === "inconclusive") reason = "已完成采样，但参考库无法可靠区分。" + reason;
    if ((record.status === "unverified" || !record.status) && (!record.checked_at || record.checked_at.startsWith("0001"))) {
      label = s.enabled ? "待自动检测" : "等待手动检测";
      reason = s.enabled ? "已加入监测，等待后台调度" : "定时检测未开启";
      next = s.enabled ? "自动排队" : "手动检测";
    }
    if (record.status === "skipped" && account?.schedulable && !record.hold) {
      label = "待自动检测";
      reason = "账号已恢复可调度，等待下一轮检查；上次：" + (record.reason || "跳过");
    }
    if (unsaved.has(id)) { label = "待保存"; reason = "已勾选，保存配置后加入自动检测"; next = "保存后开始"; }
    if (!sample && queued.has(id)) { label = "排队中"; next = "有空位后自动开始"; }
    if (!sample && !record.hold && account?.status === "active" && !account.schedulable && saved.has(id)) {
      next = s.enabled ? "待插件复测；两轮通过后按恢复开关处理" : "等待手动复测；按恢复开关处理";
    }
    if (sample) {
      label = sample.phase === "retry_wait" ? "等待自动重试" : `检测中 · ${sample.index}/3`;
      if (sample.attempt > 1) label += ` · 重试 ${sample.attempt - 1}/2`;
      next = "正在检测";
    }
    p.tag.className = "tag " + (sample ? "active" : record.status === "inconclusive" ? "uncertain" : record.status === "passed" && runtimeReady ? "good" : record.hold ? "bad" : "");
    setText(p.tag, label);
    p.model.className = sample ? "reason" : "result-model";
    const modelText = sample ? `${phaseName(sample.phase)}${sample.attempt > 1 ? ` · 第 ${sample.attempt - 1}/2 次重试` : ""}` : record.analysis?.predicted ? shortModel(record.analysis.predicted) : "";
    setText(p.model, modelText);
    p.model.hidden = !modelText;
    const candidates = record.status === "inconclusive" && !sample ? candidateSummary(record) : "";
    setText(p.candidates, candidates ? "前三：" + candidates : "");
    p.candidates.hidden = !candidates;
    p.details.hidden = !reason && !record.checked_at;
    setText(p.text, `${reason}${record.hold && record.status === "unavailable" ? "；保留原隔离，稍后重试" : ""} · ${record.failures || 0} 次不符 / ${record.unavailable || 0} 次采样失败 / ${record.inconclusive || 0} 次评分不确定${record.hold ? ` · 恢复确认 ${record.recovery_passes || 0}/2` : ""}${record.checked_at && !record.checked_at.startsWith("0001") ? ` · 上次 ${date(record.checked_at)}` : ""}`);
    setText(p.next, next);
    p.next.title = date(record.next_at);
    nodes.push(row);
  }
  if (nodes.length) syncRows($("records"), nodes, monitorRows);
  else { monitorRows.clear(); emptyTable($("records"), "尚未选择检测账号。勾选并保存后会自动检测。", 4); }
  filterRecords();
  const history = all.filter(record => !selected.has(record.account_id) && !record.hold && !record.pending).sort((a, b) => a.account_id - b.account_id);
  setText($("historyCount"), history.length);
  $("historyPanel").hidden = !history.length;
  const historyNodes = history.map(record => {
    let row = historyRows.get(record.account_id);
    if (!row) { row = document.createElement("tr"); for (let i = 0; i < 4; i++) cell(row, ""); historyRows.set(record.account_id, row); }
    [accountsByID.get(record.account_id)?.name || `账号 #${record.account_id}`, statusLabel(record.status), record.reason || "—", date(record.checked_at)].forEach((value, i) => setText(row.cells[i], value));
    return row;
  });
  syncRows($("historyRecords"), historyNodes, historyRows);
}
function shortModel(model){return String(model).replace(/^gpt-/i,"").replaceAll("-"," ");}
function phaseName(phase){return {retry_wait:"等待重试",identity:"准备账号",connecting:"连接上游",receiving:"等待回答",reasoning:"正在生成",output:"接收回答"}[phase]||"正在检测";}
function relativeTime(value){if(!value||value.startsWith("0001"))return "等待调度";const seconds=Math.ceil((new Date(value).getTime()-Date.now())/1000);if(seconds<=0)return "即将开始";if(seconds<60)return `${seconds} 秒后`;if(seconds<3600)return `约 ${Math.ceil(seconds/60)} 分钟后`;return `约 ${Math.ceil(seconds/3600)} 小时后`;}
function nextAction(r,s=lastStatus,managedIDs){
  if(savedConfig.group_ids?.length&&s?.group_sync_error)return "等待分组同步确认；暂不恢复";
  if(!(managedIDs||new Set(activeTargets(s).filter(t=>t.enabled).map(t=>t.account_id))).has(r.account_id)&&(r.hold||r.pending))return savedConfig.group_ids?.length?"不在当前管理范围；不恢复，保留暂停":"未参与监测，重新勾选并保存后复测；保留暂停";
  if(s?.runtime_error)return "修复监测服务后继续";
  if(!runtimeReady)return "等待重新连接，显示上次结果";
  if(s?.accounts_error)return "等待账号范围同步；暂不恢复";
  const account=accountsByID.get(r.account_id);
  if(!account)return "等待账号同步；已删除账号不会重建";
  if(account.status!=="active")return "账号状态非 active，保留宿主状态";
  if(r.status==="external_change"||r.status==="pause_uncertain")return s?.enabled?"等待插件复测；连续两轮通过后按恢复开关处理":"等待手动复测；连续两轮通过后按恢复开关处理";
  if(!s?.enabled)return "等待手动检测";
  if(r.hold&&r.recovery_passes>0&&!s.auto_recover)return "继续复测；自动恢复已关闭";
  return relativeTime(r.next_at)+(r.hold&&r.recovery_passes>0?"确认恢复":r.status==="unavailable"||r.status==="blocked"?"重试":r.hold?"复测":"检测");
}
function updateConnection(s){
  const connection=$("connection");connection.hidden=false;
  const error=!runtimeReady||s?.runtime_error||s?.accounts_error||["error","stale"].includes(s?.connection?.state);
  connection.dataset.tone=error?"warning":s?.monitoring_state==="running"?"good":"neutral";
  connection.textContent=!runtimeReady?"宿主状态不可用":s?.runtime_error?"监测服务异常":s?.connection?.state==="needs_credentials"?"等待连接管理端":s?.connection?.state==="stale"?"管理端状态已过期":modes[s?.monitoring_state]||(s?.enabled?"监测已启用":"监测未开启");
  $("freshness").textContent=lastRefresh?(runtimeReady?"已同步 · ":"上次同步 · ")+new Date(lastRefresh).toLocaleTimeString("zh-CN",{hour12:false}):"等待首次同步";
  $("policySummary").textContent=s?.auto_pause?(s?.auto_recover?"指纹不符自动暂停 · 连续两轮通过后恢复":"指纹不符自动暂停 · 自动恢复已关闭"):s?.auto_recover?"不新增暂停 · 管理中暂停账号两轮通过后自动恢复":"只记录结果 · 不自动改变调度";
}
function setStale(error){
  runtimeReady=false;lastFetchError=error;updateControls();updateConnection(lastStatus);
  $("lastError").textContent=error;$("lastError").hidden=false;
  if(lastStatus)renderRecords(lastStatus);renderRun(lastStatus||{});
}
function renderRun(s){
  const batch=s.batch,active=s.active_samples||(s.active_sample?[s.active_sample]:[]),running=!!s.batch_busy||active.length>0,run=s.state?.manual;
  const managed=new Set(activeTargets(s).filter(target=>target.enabled).map(target=>target.account_id));
  $("runPanel").dataset.status=running?"running":s.manual_queued?"queued":s.enabled?"idle":"paused";
  const connectionError=s.group_sync_error||s.accounts_error||["error","stale","needs_credentials"].includes(s.connection?.state)||s.monitoring_state==="connection_error";
  if(!runtimeReady||s.runtime_error||s.storage_error||connectionError){
    $("runPanel").dataset.status="error";$("runProgress").hidden=true;$("runCount").textContent="";
    $("runTitle").textContent=!runtimeReady?"暂时无法同步状态":s.runtime_error?"监测服务需要修复":s.storage_error?"巡检记录暂时无法保存":s.group_sync_error?"分组同步需要处理":s.connection?.state==="needs_credentials"?"先连接管理端":"管理连接需要处理";
    $("runHint").textContent=!runtimeReady?(lastFetchError||"正在连接宿主，保留上次显示的结果。"):s.runtime_error||s.storage_error||s.group_sync_error||s.connection?.next_action||s.accounts_error||"检查管理地址和密钥，再重新加载账号。";return;
  }
  $("runProgress").hidden=!running;$("runCount").textContent="";
  if(running){
    const isManual=batch?.kind==="manual"||manualBusy&&!s.manual_queued;
    $("runTitle").textContent=isManual?"手动检测中":"自动检测中";
    $("runHint").textContent=`${batch?.active_ids?.length||active.length} 个账号并行${batch?.queued_ids?.length?` · ${batch.queued_ids.length} 个排队`:""}${s.manual_queued?" · 手动检测已排队":""}`;
    if(batch?.window_seconds&&!isManual){
      $("runProgress").hidden=true;
      $("runCount").textContent=`${batch.active_ids?.length||active.length} 个进行中`;
    }else{
      const total=(isManual?run?.total:0)||batch?.total||active.length;
      const done=(isManual?run?.completed:undefined)??batch?.completed??0;
      $("runCount").textContent=`${done} / ${total}`;
      const progress=total?Math.min(1,done/total):0;
      $("runFill").style.transform=`scaleX(${progress})`;
      $("runProgress").setAttribute("aria-valuenow",String(Math.round(progress*100)));
    }
    return;
  }
  if(s.manual_queued||manualBusy){$("runTitle").textContent="检测已排队";$("runHint").textContent="有并发空位后自动开始";return;}
  const awaitingRetest=Object.values(s.state?.records||{}).filter(r=>managed.has(r.account_id)&&accountsByID.get(r.account_id)?.status==="active"&&["external_change","pause_uncertain"].includes(r.status));
  if(awaitingRetest.length){
    $("runPanel").dataset.status="warning";$("runTitle").textContent="暂停账号待插件复测";
    $("runHint").textContent=`${awaitingRetest.length} 个已启用管理的账号等待${s.enabled?"自动":"手动"}复测；旧保护记录不再锁死恢复，连续两轮有效通过后按自动恢复开关处理，不立即解除暂停。`;return;
  }
  if(s.batch?.status==="completed_with_errors"||s.state?.manual?.status==="interrupted"||s.last_error){
    $("runPanel").dataset.status="warning";$("runTitle").textContent=s.state?.manual?.status==="interrupted"?"上次手动检测已中断":"部分账号需要重试";
    $("runHint").textContent=s.state?.manual?.status==="interrupted"?"服务重启中断了上次任务，可重新发起检测。":s.last_error||`本轮有 ${s.batch?.errors||0} 项未完成，已保留各账号结果，请查看下一步。`;return;
  }
  $("runTitle").textContent=!s.target_count?"选择账号后开始监测":s.enabled?"自动监测已开启":"自动监测已关闭";
  const times=Object.values(s.state?.records||{}).filter(r=>managed.has(r.account_id)).map(r=>new Date(r.next_at).getTime()).filter(n=>Number.isFinite(n)&&n>0);
  const ready=new Date(s.startup_ready_at||0).getTime(),next=times.length?Math.max(Math.min(...times),ready):ready;
  $("runHint").textContent=s.accounts_error?"账号列表同步失败，正在重试":!s.target_count?"打开管理账号，选择需要监测的账号":s.enabled?`下次检查${next>0?" · "+relativeTime(new Date(next).toISOString()):"自动排队"}`:"仍可手动检测已选择的账号";
}
