"use strict";
function filterRecords(){
  const query=$("monitorSearch").value.trim().toLowerCase(),kind=$("monitorFilter").value;
  let total=0,visible=0;
  for(const row of $("records").querySelectorAll("tr[data-account-id]")){
    total++;row.hidden=!row.dataset.search.includes(query)||(kind==="held"&&row.dataset.held!=="true")||(kind==="active"&&row.dataset.active!=="true")||(kind==="attention"&&row.dataset.attention!=="true");if(!row.hidden)visible++;
  }
  $("monitorToolbar").hidden=!total;
  $("monitorSummary").textContent=total?(visible===total?`${total} 个账号`:`显示 ${visible} / ${total} 个账号`):"";
  if(total&&!visible){$("monitorSummary").textContent="没有符合条件的账号，请更换筛选。";}
}
function duration(seconds){const n=Number(seconds)||0;return n>=3600&&n%3600===0?`${n/3600} 小时`:n>=60&&n%60===0?`${n/60} 分钟`:`${n} 秒`;}
function openStep(id,focusID){
  $("configPanel").open=true;$("manageAccounts").setAttribute("aria-expanded","true");
  $(id).scrollIntoView?.({block:"start",behavior:window.matchMedia?.("(prefers-reduced-motion: reduce)").matches?"auto":"smooth"});
  if(focusID)$(focusID)?.focus({preventScroll:true});
}
function readinessBlock(){
  if(lastStatus?.runtime_error)return lastStatus.runtime_error;
  if(lastStatus?.storage_error)return lastStatus.storage_error;
  const selected=Array.from(drafts.values()).filter(t=>t.enabled).map(t=>t.account_id);
  const checks=(lastStatus?.readiness?.checks||[]).filter(check=>check.state==="blocked");
  const global=checks.find(check=>!check.account_ids?.length&&!(check.code==="targets_required"&&selected.length));
  if(global)return global.next_action||global.message||"检测准备尚未完成";
  const affected=new Set(checks.flatMap(check=>check.account_ids||[]));
  if(selected.length&&selected.every(id=>affected.has(id)))return checks.find(check=>check.account_ids?.some(id=>selected.includes(id)))?.next_action||"所选账号暂时无法检测，请处理账号提示";
  if(lastStatus?.readiness?.state==="blocked"&&!checks.length)return lastStatus.readiness.next_action||"检测准备尚未完成";
  return "";
}
function updateFlow(){
  const groupMode=groupIDs().length>0;
  const count=groupMode?(lastStatus?.target_count||0):Array.from(drafts.values()).filter(t=>t.enabled).length;
  const hasConnection=!!lastStatus&&Object.prototype.hasOwnProperty.call(lastStatus,"connection");
  const checked=lastStatus?.accounts_checked_at;
  const legacyChecked=typeof checked==="string"&&!checked.startsWith("0001")&&Number.isFinite(Date.parse(checked))&&Date.parse(checked)>0;
  const connected=!!savedConfig.admin_key&&(hasConnection?lastStatus.connection?.state==="ready":!lastStatus?.accounts_error&&lastStatus?.monitoring_state!=="connection_error"&&(accounts.length>0||legacyChecked));
  const blocked=readinessBlock();
  const step=!connected?1:count?3:2;
  $("app").dataset.onboarding=String(onboarding);$("setupSteps").hidden=!onboarding;
  $("accountsPending").hidden=!!savedConfig.admin_key;
  $("stepConnect").dataset.current=String(step===1);$("stepConnect").dataset.complete=String(connected);
  $("stepAccounts").dataset.current=String(step===2);$("stepAccounts").dataset.complete=String(count>0);
  $("stepReview").dataset.current=String(step===3);
  $("stepConnectHint").textContent=connected?"管理端已连接":"填写地址与访问密钥";
  $("stepAccountsHint").textContent=count?`已选择 ${count} 个账号`:"使用推荐的检测设置";
  $("startSetup").hidden=!onboarding;$("save").hidden=onboarding&&!groupMode;$("runNow").hidden=onboarding;
  $("startSetup").disabled=busy||manualBusy||batchBusy||!runtimeReady||(!count&&!groupMode)||!connected||!!blocked;
  $("startSetup").title=blocked||(!connected?"先保存连接并验证管理端":"保存账号后开始首次检测");
  $("reviewSelection").hidden=!onboarding;
  $("reviewSummary").textContent=groupMode?`按分组同步，当前 ${count} 个活动目标；新成员${$("group_model").value.trim()&&$("group_expected").value.trim()?"使用指定模板":"先等待配置"}。管理账号即使被外部暂停，也在两轮有效通过后按自动恢复开关处理；删除或移出分组不会恢复。`:count?`本次监测 ${count} 个账号，每轮至少 ${count*3} 次采样请求。${$("enabled").checked?"开启后后台定时巡检。":"定时巡检关闭，仅在点击检测时运行。"} ${$("auto_pause").checked?"确认指纹不符后自动暂停新调度。":"不新增自动暂停。"}${$("auto_recover").checked?"管理中的暂停账号会继续自动复核，连续两轮有效通过后恢复。":"已有暂停不会自动放行。"}`:"先选择账号，再确认是否开启自动处理。";
  $("timingSummary").textContent=`失败后 ${duration($("retry_seconds").value)}重试 · 暂停后每 ${duration($("quarantine_seconds").value)}复测${Number($("recovery_spacing_seconds").value)>0?" · 恢复两轮至少间隔 "+duration($("recovery_spacing_seconds").value):""}`;
  const preset=String($("normal_seconds").value);$("intervalPreset").value=["1800","900","3600"].includes(preset)?preset:"custom";
  const guide=blocked||lastStatus?.connection?.next_action||lastStatus?.readiness?.next_action||"";
  $("connectionGuide").textContent=guide;$("connectionGuide").hidden=!guide||(connected&&!blocked);
  $("discard").hidden=!dirty;$("discard").disabled=busy;
  $("connectAccounts").disabled=busy||manualBusy||batchBusy||!loaded;
  $("refreshAccounts").disabled=busy||!savedConfig.admin_key;
  $("diagnose").disabled=busy||!savedConfig.admin_key;
}
function updateControls(){
  const hardBlocked=(manualBusy||batchBusy)&&hardConfigChanged();
  $("settings").disabled=busy;$("save").disabled=busy||hardBlocked||!dirty;
  $("stopNow").hidden=!(manualBusy||batchBusy||lastStatus?.manual_queued);
  $("stopNow").disabled=busy||!runtimeReady;
  $("runNow").disabled=busy||manualBusy||(batchBusy&&dirty&&hardConfigChanged())||!runtimeReady||!loaded||!!lastStatus?.runtime_error;
  $("runLabel").textContent=manualBusy?"检测中…":dirty?"保存并立即检测":"立即检测";
  $("runNow").setAttribute("aria-busy",String(busy||manualBusy));
  $("saveState").textContent=busy?"正在保存…":dirty?(hardBlocked?"连接或目录修改需停止当前检测后保存":(batchBusy||manualBusy)?"有未保存的修改 · 保存后立即生效":"有未保存的修改"):"已保存";$("settingsHint").textContent=dirty?"有未保存修改":"按需调整";
  $("saveState").dataset.dirty=String(dirty);
  $("runNow").title=lastStatus?.runtime_error?"监测服务需要修复；可修改配置并重新保存":runtimeReady?"立即检测已选择的账号":"请先在 Sub2API 插件管理页启用插件";
  $("manageAccounts").setAttribute("aria-expanded",String($("configPanel").open));updateFlow();
}
