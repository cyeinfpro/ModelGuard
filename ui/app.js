"use strict";
(() => {
  const $ = id => document.getElementById(id);
  const token = new URLSearchParams(location.hash.slice(1)).get("bridge_token");
  const pending = new Map();
  let lastStatus = null;
  let models = [], accounts = [], loaded = false, polling = false, accountSignature = "";
  let configured = new Map(), drafts = new Map();
  let savedConfig = {}, dirty = false, busy = false, manualBusy = false, batchBusy = false, runtimeReady = false, lastRefresh = 0;
  let stateEpoch=0, refreshSequence=0, lastFetchError="", onboarding=false;
  const bools = ["enabled", "auto_pause", "auto_recover"];
  const strings = ["admin_url", "admin_key", "data_dir"];
  const nums = ["concurrency","normal_seconds", "retry_seconds", "quarantine_seconds", "timeout_seconds", "daily_requests"];
  const names = {paused_observation:"暂停中 · 仅复测",passed:"通过",suspect:"待复核",unavailable:"检测失败",quarantined:"指纹不符",recovering:"恢复复核中",recovery_ready:"等待自动恢复",pause_recommended:"建议隔离",external_change:"外部变更 · 已保护",skipped:"跳过",blocked:"需处理",unsupported:"不支持",pause_uncertain:"暂停回执不确定",unverified:"待检测"};
  const modes = {disabled:"监测未开启",running:"监测已启用",waiting_targets:"等待选择账号",connection_error:"管理连接异常",loading_accounts:"正在加载账号"};
  function message(text, error=false) { $("message").setAttribute("role",error?"alert":"status"); $("message").textContent=text; $("message").className=error?"error":""; }
  function send(type, payload={}) {
    if (!token || parent === window) return Promise.reject(new Error("请在 Sub2API 插件管理页打开此页面"));
    const id=crypto.randomUUID();
    return new Promise((resolve,reject) => {
      const timer=setTimeout(()=>{pending.delete(id);reject(new Error("宿主响应超时"));},["config.save","config.test"].includes(type)?120000:20000);
      pending.set(id,{resolve,reject,timer});
      parent.postMessage({source:"sub2api-plugin-ui",bridge_token:token,type,request_id:id,...payload},"*");
    });
  }
  window.addEventListener("message",e=>{
    const m=e.data;
    if(e.source!==parent||!m||m.source!=="sub2api-plugin-host"||m.bridge_token!==token)return;
    const p=pending.get(m.request_id);if(!p)return;
    clearTimeout(p.timer);pending.delete(m.request_id);
    m.ok?p.resolve(m):p.reject(new Error(typeof m.error==="string"?m.error:(m.result?.message||"宿主拒绝了请求")));
  });
  function cell(row,text){const td=document.createElement("td");td.textContent=text;row.append(td);return td;}
  function collectDrafts(){
    for(const row of $("targets").querySelectorAll("tr[data-account-id]")){
      const id=Number(row.dataset.accountId);
      drafts.set(id,{account_id:id,enabled:row.querySelector('[data-field="enabled"]').checked,model:row.querySelector('[data-field="model"]').value.trim(),expected:row.querySelector('[data-field="expected"]').value});
    }
  }
  function filterAccounts(){
    const query=$("accountSearch").value.trim().toLowerCase();
    let visible=0;
    for(const row of $("targets").querySelectorAll("tr[data-account-id]")){
      const selected=row.querySelector('[data-field="enabled"]').checked;
      row.dataset.selected=String(selected);
      const kind=$("accountFilter").value;
      row.hidden=!row.dataset.search.includes(query)||(kind==="selected"&&!selected)||(kind==="schedulable"&&row.dataset.schedulable!=="true")||(kind==="supported"&&row.dataset.supported!=="true");
      if(!row.hidden)visible++;
    }
    $("noResults").hidden=visible>0||accounts.length===0;
    const count=$("targets").querySelectorAll('tr[data-selected="true"]').length;
    $("selectionSummary").textContent=`已选择 ${count} 个 · 当前显示 ${visible} 个`;
    $("selectVisible").disabled=visible===0||busy||manualBusy;
    $("clearSelection").disabled=count===0||busy||manualBusy;
    $("applyRecommended").disabled=count===0||busy;
    $("reviewSelection").disabled=count===0;
  }
  function renderAccounts(){
    if(!loaded)return;
    const active=document.activeElement, focusRow=active?.closest?.("tr[data-account-id]");
    const focus=focusRow?{id:focusRow.dataset.accountId,field:active.dataset.field,summary:active.tagName==="SUMMARY",start:active.selectionStart,end:active.selectionEnd}:null;
    const opened=new Set(Array.from($("targets").querySelectorAll("details[open]")).map(d=>d.dataset.accountDetails));
    collectDrafts();$("targets").replaceChildren();
    const listed=new Map(accounts.map(a=>[a.id,a]));
    for(const [id,t] of new Map([...configured,...drafts]))if(!listed.has(id)&&(t.enabled||lastStatus?.state?.records?.[id]?.hold||lastStatus?.state?.records?.[id]?.pending))listed.set(id,{id,name:`账号 #${id}`,type:"",status:"unavailable",supported:false,missing:true});
    if(!listed.size){const row=document.createElement("tr");const c=cell(row,savedConfig.admin_key?"还没有可见账号。请检查管理密钥的账号授权范围，然后重新加载。":"先连接管理端，授权账号会自动显示在这里。");c.colSpan=4;c.className="empty";$("targets").append(row);}
    for(const a of listed.values()){
      const t=drafts.get(a.id)||{account_id:a.id,enabled:false,model:a.suggested_model||"gpt-6.1-terra",expected:a.suggested_expected||"gpt-6.1-terra"};
      const usable=!!a.supported&&!(a.type==="apikey"&&lastStatus?.api_bridge_ready===false);
      const row=document.createElement("tr");row.dataset.accountId=String(a.id);row.dataset.schedulable=String(!!a.schedulable);row.dataset.supported=String(usable);row.dataset.search=`${a.name} ${a.id}`.toLowerCase();
      const enabled=document.createElement("input");enabled.type="checkbox";enabled.checked=t.enabled;enabled.dataset.field="enabled";enabled.setAttribute("aria-label",`监测 ${a.name}`);enabled.disabled=!usable&&!t.enabled;cell(row,"").append(enabled);
      const name=cell(row,a.name||`账号 #${a.id}`);name.className="account-name";const small=document.createElement("span");small.className="account-id";small.textContent=`#${a.id} · ${a.type==="oauth"?"OAuth":a.type==="apikey"?"API Key":a.type||"账号不可见"}`;name.append(small);
      const status=a.type==="apikey"&&lastStatus?.api_bridge_ready===false?"需安装本机桥接":a.missing?"当前密钥不可见":!a.supported?"暂不支持":a.status!=="active"?a.status:a.schedulable?"可调度":"已暂停";
      cell(row,status).className="account-status";
      const details=document.createElement("details");details.className="target-details";details.dataset.accountDetails=String(a.id);details.open=opened.has(String(a.id));
      const summary=document.createElement("summary"),summaryText=document.createElement("span");
      const recommended=t.model===(a.suggested_model||"gpt-6.1-terra")&&t.expected===(a.suggested_expected||"gpt-6.1-terra");
      summaryText.textContent=recommended?"按推荐设置":"自定义模型";const caption=document.createElement("span");caption.className="model-caption";caption.textContent=t.model;summaryText.append(caption);summary.append(summaryText);details.append(summary);
      const modelLabel=document.createElement("label");modelLabel.textContent="发给渠道的模型名称";
      const model=document.createElement("input");model.value=t.model;model.dataset.field="model";model.required=!!t.enabled;model.spellcheck=false;model.setAttribute("aria-label",`${a.name} 请求模型`);modelLabel.append(model);
      const expectedLabel=document.createElement("label");expectedLabel.textContent="预期的指纹参考类";
      const expected=document.createElement("select");expected.dataset.field="expected";expected.required=!!t.enabled;expected.setAttribute("aria-label",`${a.name} 预期指纹`);
      const choices=models.length?models:[{id:t.expected,display_name:t.expected}];
      for(const m of choices){const opt=document.createElement("option");opt.value=m.id;opt.textContent=m.display_name||m.id;expected.append(opt);}
      if(!choices.some(m=>m.id===t.expected)){const opt=document.createElement("option");opt.value=t.expected;opt.textContent=t.expected;expected.append(opt);}expected.value=t.expected;expectedLabel.append(expected);
      const help=document.createElement("p");help.textContent=a.type==="apikey"&&lastStatus?.api_bridge_ready===false?(lastStatus.api_bridge_message||"先安装本机凭据桥接；OAuth 账号可以直接监测。"):"通常保持推荐值即可。渠道模型名可被映射，参考类用于比较回答的统计特征，两者不必同名。";
      details.append(modelLabel,expectedLabel,help);cell(row,"").append(details);$("targets").append(row);
    }
    filterAccounts();
    if(focus){const row=$("targets").querySelector(`tr[data-account-id="${focus.id}"]`);const el=focus.summary?row?.querySelector("summary"):row?.querySelector(`[data-field="${focus.field}"]`);el?.focus({preventScroll:true});if(el?.type==="text"&&focus.start!==null)el.setSelectionRange(focus.start,focus.end);}
  }
  function readConfig(){
    collectDrafts();const c={...savedConfig};
    for(const k of bools)c[k]=$(k).checked;
    for(const k of strings)c[k]=$(k).value.trim();
    for(const k of nums)c[k]=Number($(k).value);
    if(savedConfig.manual_request_id)c.manual_request_id=savedConfig.manual_request_id;
    c.targets=Array.from(drafts.values()).filter(t=>t.enabled||(configured.has(t.account_id)&&(accounts.some(a=>a.id===t.account_id)||lastStatus?.accounts_error||lastStatus?.accounts_loading))||lastStatus?.state?.records?.[t.account_id]?.hold||lastStatus?.state?.records?.[t.account_id]?.pending);
    return c;
  }
  function fill(c){
    savedConfig=JSON.parse(JSON.stringify(c));dirty=false;
    onboarding=!c.admin_key||!(c.targets||[]).some(t=>t.enabled);
    for(const k of bools)if(k in c)$(k).checked=c[k];
    for(const k of [...strings,...nums])if(k in c)$(k).value=c[k];
    configured=new Map((c.targets||[]).map(t=>[t.account_id,{...t}]));drafts=new Map((c.targets||[]).map(t=>[t.account_id,{...t}]));
    $("targets").replaceChildren();loaded=true;renderAccounts();updateControls();if(!c.admin_key||!(c.targets||[]).some(t=>t.enabled))$("configPanel").open=true;if(lastStatus)render(lastStatus);updateFlow();
  }
  function date(value){if(!value||value.startsWith("0001"))return "—";return new Date(value).toLocaleString("zh-CN",{hour12:false});}
  function render(s,syncTargets=false){
    if(syncTargets&&loaded&&!dirty&&!busy&&Array.isArray(s.targets)&&JSON.stringify(s.targets)!==JSON.stringify(Array.from(configured.values()))){
      configured=new Map(s.targets.map(t=>[t.account_id,{...t}]));drafts=new Map(s.targets.map(t=>[t.account_id,{...t}]));savedConfig.targets=s.targets.map(t=>({...t}));
      $("targets").replaceChildren();accountSignature="";
    }
    lastStatus=s;
    models=s.models||models;
    updateConnection(s);
    $("lastError").textContent=s.runtime_error||s.storage_error||s.last_error||s.accounts_error||"";$("lastError").hidden=!$("lastError").textContent;
    $("bankVersion").textContent=s.detector_version||"";
    $("accountError").textContent=s.accounts_error||"";$("accountError").hidden=!$("accountError").textContent;
    $("accountSummary").textContent=s.accounts_loading?"正在读取账号…":s.accounts_error?"账号加载失败":`已读取 ${s.accounts?.length||0} 个账号 · 仅显示当前密钥授权范围${(s.accounts?.length||0)>6?" · 可搜索或滚动选择":""}`;
    const signature=JSON.stringify([s.accounts||[],models,s.api_bridge_ready]);
    if(signature!==accountSignature){accounts=s.accounts||[];accountSignature=signature;renderAccounts();}
    manualBusy=!!s.manual_busy;batchBusy=!!s.batch_busy||!!s.active_samples?.length||!!s.active_sample;renderRun(s);
    updateControls();
    renderRecords(s);updateFlow();
    const state=s.state||{};
    $("events").replaceChildren();const events=(state.events||[]).filter(e=>$("showSamples").checked||!["sample_started","sample_finished"].includes(e.action)).slice(-40).reverse();
    if(!events.length){const p=document.createElement("p");p.className="empty";p.textContent="暂无记录";$("events").append(p);}
    for(const event of events){const row=document.createElement("div");row.className="event";for(const [tag,text] of [["time",date(event.at)],["span",accounts.find(a=>a.id===event.account_id)?.name||String(event.account_id)],["span",event.reason]]){const el=document.createElement(tag);el.textContent=text;row.append(el);}$("events").append(row);}
  }
  function renderRecords(s){
    const state=s.state||{},saved=new Set(Array.from(configured.values()).filter(t=>t.enabled).map(t=>t.account_id));
    const selected=new Set(saved),unsaved=new Set();
    if(dirty)for(const t of drafts.values())if(t.enabled&&!saved.has(t.account_id)){selected.add(t.account_id);unsaved.add(t.account_id);}
    const all=Object.values(state.records||{}),map=new Map(all.filter(r=>selected.has(r.account_id)||r.hold||r.pending).map(r=>[r.account_id,r]));
    for(const id of selected)if(!map.has(id))map.set(id,{account_id:id,status:"unverified"});
    const rows=Array.from(map.values()).sort((a,b)=>a.account_id-b.account_id),byID=new Map(accounts.map(a=>[a.id,a]));
    const active=new Map((s.active_samples||(s.active_sample?[s.active_sample]:[])).map(a=>[a.account_id,a]));
    $("targetCount").textContent=s.target_count??saved.size;$("holdCount").textContent=rows.filter(r=>r.hold).length;
    $("recoverCount").textContent=rows.filter(r=>r.hold&&r.recovery_passes>0).length;$("requestCount").textContent=s.budget?.used??state.requests??0;$("budgetLimit").textContent=s.budget?` / ${s.budget.limit}`:"";for(const id of ["holdCount","recoverCount"])$(id).parentElement.hidden=Number($(id).textContent)===0;
    const focused=document.activeElement?.closest?.("details[data-record-details]")?.dataset.recordDetails;
    const opened=new Set(Array.from($("records").querySelectorAll("details[open]")).map(x=>x.dataset.recordDetails));$("records").replaceChildren();
    if(!rows.length){const row=document.createElement("tr");const td=cell(row,"尚未选择检测账号。勾选并保存后会自动检测。");td.colSpan=4;td.className="empty";$("records").append(row);}
    for(const r of rows){
      const a=byID.get(r.account_id),sample=active.get(r.account_id),row=document.createElement("tr");row.dataset.accountId=String(r.account_id);
      row.dataset.search=`${a?.name||""} ${r.account_id}`.toLowerCase();row.dataset.held=String(!!r.hold);row.dataset.active=String(!!sample||!!s.batch?.queued_ids?.includes(r.account_id));row.dataset.attention=String(!!r.hold||["unavailable","blocked","unsupported","external_change","pause_uncertain","pause_recommended","suspect"].includes(r.status));
      const nameCell=cell(row,a?.name||`账号 #${r.account_id}`);const accountID=document.createElement("span");accountID.className="account-id";accountID.textContent=`#${r.account_id}`;nameCell.append(accountID);
      cell(row,a?`${s.accounts_error?"上次：":""}${a.status!=="active"?a.status:a.schedulable?"可调度":r.hold?"本插件暂停":"外部暂停"}`:"待同步");
      let label=names[r.status]||r.status,reason=r.reason||"",next=nextAction(r,s);
      if(r.status==="unavailable"&&r.hold)label="复测失败";
      if((r.status==="unverified"||!r.status)&&(!r.checked_at||r.checked_at.startsWith("0001"))){label=s.enabled?"待自动检测":"等待手动检测";reason=s.enabled?"已加入监测，等待后台调度":"定时检测未开启";next=s.enabled?"自动排队":"手动检测";}
      if(r.status==="skipped"&&a?.schedulable&&!r.hold){label="待自动检测";reason="账号已恢复可调度，等待下一轮检查；上次："+(r.reason||"跳过");}
      if(unsaved.has(r.account_id)){label="待保存";reason="已勾选，保存配置后加入自动检测";next="保存后开始";}
      if(!sample&&s.batch?.queued_ids?.includes(r.account_id)){label="本轮排队中";next="有空位后自动开始";}
      if(!sample&&!r.hold&&a&&a.status==="active"&&!a.schedulable)next="仅手动复测，不改变调度";
      if(sample){label=sample.phase==="retry_wait"?"等待自动重试":`检测中 · ${sample.index}/3`;if(sample.attempt>1)label+=` · 重试 ${sample.attempt-1}/2`;next="正在检测";}
      const result=cell(row,"");const tag=document.createElement("span");tag.className="tag "+(sample?"active":r.status==="passed"&&runtimeReady?"good":r.hold?"bad":"");tag.textContent=label;result.append(tag);
      if(sample){const progress=document.createElement("span");progress.className="reason";progress.textContent=`${phaseName(sample.phase)}${sample.attempt>1?` · 第 ${sample.attempt-1}/2 次重试`:""}`;result.append(progress);}
      else if(r.analysis?.predicted){const model=document.createElement("span");model.className="result-model";model.textContent=shortModel(r.analysis.predicted);result.append(model);}
      if(reason||r.checked_at){const details=document.createElement("details");details.className="record-details";details.dataset.recordDetails=String(r.account_id);details.open=opened.has(String(r.account_id));const summary=document.createElement("summary");summary.textContent="详情";const text=document.createElement("p");text.textContent=`${reason}${r.hold&&r.status==="unavailable"?"；保留原隔离，稍后重试":""} · ${r.failures||0} 次不符 / ${r.unavailable||0} 次检测失败${r.hold?` · 恢复确认 ${r.recovery_passes||0}/2`:""}${r.checked_at&&!r.checked_at.startsWith("0001")?` · 上次 ${date(r.checked_at)}`:""}`;details.append(summary,text);result.append(details);}
      const nextCell=cell(row,next);nextCell.title=date(r.next_at);$("records").append(row);
    }
    filterRecords();
    if(focused)$("records").querySelector(`details[data-record-details="${focused}"] summary`)?.focus({preventScroll:true});
    const history=all.filter(r=>!selected.has(r.account_id)&&!r.hold&&!r.pending).sort((a,b)=>a.account_id-b.account_id);
    $("historyCount").textContent=history.length;$("historyPanel").hidden=!history.length;$("historyRecords").replaceChildren();
    for(const r of history){const row=document.createElement("tr");cell(row,byID.get(r.account_id)?.name||`账号 #${r.account_id}`);cell(row,names[r.status]||r.status);cell(row,r.reason||"—");cell(row,date(r.checked_at));$("historyRecords").append(row);}
  }
  function shortModel(model){return model.replace(/^gpt-/i,"").replaceAll("-"," ");}
  function phaseName(phase){return {retry_wait:"等待重试",identity:"准备账号",connecting:"连接上游",receiving:"等待回答",reasoning:"正在生成",output:"接收回答"}[phase]||"正在检测";}
  function relativeTime(value){if(!value||value.startsWith("0001"))return "等待调度";const seconds=Math.ceil((new Date(value).getTime()-Date.now())/1000);if(seconds<=0)return "即将开始";if(seconds<60)return `${seconds} 秒后`;if(seconds<3600)return `约 ${Math.ceil(seconds/60)} 分钟后`;return `约 ${Math.ceil(seconds/3600)} 小时后`;}
  function nextAction(r,s=lastStatus){
    if(!configured.get(r.account_id)?.enabled&&(r.hold||r.pending))return "未参与监测，重新勾选并保存后复测；保留暂停";
    if(s?.runtime_error)return "修复监测服务后继续";
    if(!runtimeReady)return "等待重新连接，显示上次结果";
    if(r.status==="external_change"||r.status==="pause_uncertain")return "检查调度变更，不自动恢复";
    if(!s?.enabled)return "等待手动检测";
    if(r.hold&&r.recovery_passes>0&&!s.auto_recover)return "继续复测；自动恢复已关闭";
    return relativeTime(r.next_at)+(r.hold&&r.recovery_passes>0?"确认恢复":r.status==="unavailable"||r.status==="blocked"?"重试":r.hold?"复测":"检测");
  }
  function snapshot(result){
    const value=result?.status_json;if(!value)throw new Error(result?.message||"插件未运行，请在宿主中启用插件");
    const status=typeof value==="string"?JSON.parse(value):value;
    if(!status||typeof status!=="object"||Array.isArray(status))throw new Error("监测状态格式异常，请重试");
    return status;
  }
  function updateConnection(s){
    const connection=$("connection");connection.hidden=false;
    const error=!runtimeReady||s?.runtime_error||s?.accounts_error||["error","stale"].includes(s?.connection?.state);
    connection.dataset.tone=error?"warning":s?.monitoring_state==="running"?"good":"neutral";
    connection.textContent=!runtimeReady?"宿主状态不可用":s?.runtime_error?"监测服务异常":s?.connection?.state==="needs_credentials"?"等待连接管理端":s?.connection?.state==="stale"?"管理端状态已过期":modes[s?.monitoring_state]||(s?.enabled?"监测已启用":"监测未开启");
    $("freshness").textContent=lastRefresh?(runtimeReady?"已同步 · ":"上次同步 · ")+new Date(lastRefresh).toLocaleTimeString("zh-CN",{hour12:false}):"等待首次同步";
    $("policySummary").textContent=s?.auto_pause?(s?.auto_recover?"指纹不符自动暂停 · 连续两轮通过后恢复":"指纹不符自动暂停 · 自动恢复已关闭"):s?.auto_recover?"不新增暂停 · 已有本插件暂停仍自动恢复":"只记录结果 · 不自动改变调度";
  }
  function setStale(error){
    runtimeReady=false;lastFetchError=error;updateControls();updateConnection(lastStatus);
    $("lastError").textContent=error;$("lastError").hidden=false;
    if(lastStatus)renderRecords(lastStatus);renderRun(lastStatus||{});
  }
  async function refresh(force=false){
    if(polling&&!force)return;
    const sequence=++refreshSequence,epoch=stateEpoch;polling=true;
    try{
      const r=await send("plugin.status");
      if(sequence!==refreshSequence||epoch!==stateEpoch)return;
      const s=snapshot(r.result);runtimeReady=true;lastFetchError="";lastRefresh=Date.now();render(s,true);
    }catch(e){if(sequence===refreshSequence&&epoch===stateEpoch)setStale(e.message);}
    finally{if(sequence===refreshSequence)polling=false;}
  }
  async function testSavedConnection(){
    // A pure connection check must never consume a saved manual command.
    // Read current committed config, clear only its nonce, and leave UI drafts alone.
    const current=await send("config.load");
    if(current.config?.manual_request_id){
      const config={...current.config,manual_request_id:""};
      const saved=await send("config.save",{config});
      savedConfig.manual_request_id=saved.config?.manual_request_id||"";
    }
    const response=await send("config.test"),result=response.result||{};
    const status=result.status_json?snapshot(result):null;
    if(status)render(status);
    if(!result.success)throw new Error(result.message||"管理连接未通过检查");
    if(status?.diagnostic?.kind!=="connection")throw new Error("未收到连接诊断回执，请刷新状态后重试");
    return result;
  }
  async function discover(button){
    if(busy||manualBusy)return;busy=true;stateEpoch++;button.setAttribute("aria-busy","true");updateControls();
    try{
      const result=await testSavedConnection();message(result.message||"管理连接检查完成");
      await refresh(true);
    }catch(e){message(e.message,true);await refresh(true);}
    finally{busy=false;button.removeAttribute("aria-busy");updateControls();}
  }
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
    const count=Array.from(drafts.values()).filter(t=>t.enabled).length;
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
    $("startSetup").hidden=!onboarding;$("save").hidden=onboarding;$("runNow").hidden=onboarding;
    $("startSetup").disabled=busy||manualBusy||batchBusy||!runtimeReady||!count||!connected||!!blocked;
    $("startSetup").title=blocked||(!connected?"先保存连接并验证管理端":"保存账号后开始首次检测");
    $("reviewSelection").hidden=!onboarding;
    $("reviewSummary").textContent=count?`本次监测 ${count} 个账号，每轮至少 ${count*3} 次采样请求。${$("enabled").checked?"开启后后台定时巡检。":"定时巡检关闭，仅在点击检测时运行。"} ${$("auto_pause").checked?"确认指纹不符后自动暂停新调度。":"不新增自动暂停。"}${$("auto_recover").checked?"已有本插件暂停会继续自动复核恢复。":"已有暂停不会自动放行。"}`:"先选择账号，再确认是否开启自动处理。";
    $("timingSummary").textContent=`失败后 ${duration($("retry_seconds").value)}重试 · 暂停后每 ${duration($("quarantine_seconds").value)}复测`;
    const preset=String($("normal_seconds").value);$("intervalPreset").value=["1800","900","3600"].includes(preset)?preset:"custom";
    const guide=blocked||lastStatus?.connection?.next_action||lastStatus?.readiness?.next_action||"";
    $("connectionGuide").textContent=guide;$("connectionGuide").hidden=!guide||(connected&&!blocked);
    $("discard").hidden=!dirty;$("discard").disabled=busy;
    $("connectAccounts").disabled=busy||manualBusy||batchBusy||!loaded;
    $("refreshAccounts").disabled=busy||manualBusy||!savedConfig.admin_key;
    $("diagnose").disabled=busy||manualBusy||!savedConfig.admin_key;
  }
  function updateControls(){
    $("settings").disabled=busy;$("save").disabled=busy||manualBusy||batchBusy||!dirty;
    $("runNow").disabled=busy||manualBusy||(batchBusy&&dirty)||!runtimeReady||!loaded||!!lastStatus?.runtime_error;
    $("runLabel").textContent=manualBusy?"检测中…":dirty?"保存并立即检测":"立即检测";
    $("runNow").setAttribute("aria-busy",String(busy||manualBusy));
    $("saveState").textContent=busy?"正在保存…":dirty?(batchBusy||manualBusy?"修改已保留，本轮完成后可保存":"有未保存的修改"):"已保存";$("settingsHint").textContent=dirty?"有未保存修改":"按需调整";
    $("saveState").dataset.dirty=String(dirty);
    $("runNow").title=lastStatus?.runtime_error?"监测服务需要修复；可修改配置并重新保存":runtimeReady?"立即检测已选择的账号":"请先在 Sub2API 插件管理页启用插件";
    $("manageAccounts").setAttribute("aria-expanded",String($("configPanel").open));updateFlow();
  }
  function renderRun(s){
    const batch=s.batch,active=s.active_samples||(s.active_sample?[s.active_sample]:[]),running=!!s.batch_busy||active.length>0,run=s.state?.manual;
    $("runPanel").dataset.status=running?"running":s.manual_queued?"queued":s.enabled?"idle":"paused";
    const connectionError=s.accounts_error||["error","stale","needs_credentials"].includes(s.connection?.state)||s.monitoring_state==="connection_error";
    if(!runtimeReady||s.runtime_error||s.storage_error||connectionError){
      $("runPanel").dataset.status="error";$("runProgress").hidden=true;$("runCount").textContent="";
      $("runTitle").textContent=!runtimeReady?"暂时无法同步状态":s.runtime_error?"监测服务需要修复":s.storage_error?"巡检记录暂时无法保存":s.connection?.state==="needs_credentials"?"先连接管理端":"管理连接需要处理";
      $("runHint").textContent=!runtimeReady?(lastFetchError||"正在连接宿主，保留上次显示的结果。"):s.runtime_error||s.storage_error||s.connection?.next_action||s.accounts_error||"检查管理地址和密钥，再重新加载账号。";return;
    }
    $("runProgress").hidden=!running;$("runCount").textContent="";
    if(running){const isManual=batch?.kind==="manual"||manualBusy&&!s.manual_queued;$("runTitle").textContent=isManual?"手动检测中":"自动检测中";$("runHint").textContent=`${batch?.active_ids?.length||active.length} 个账号并行${batch?.queued_ids?.length?` · ${batch.queued_ids.length} 个排队`:""}${s.manual_queued?" · 手动检测已排队":""}`;const total=batch?.total||run?.total||active.length,done=batch?.completed||run?.completed||0;$("runCount").textContent=`${done} / ${total}`;const progress=total?done/total:0;$("runFill").style.transform=`scaleX(${progress})`;$("runProgress").setAttribute("aria-valuenow",String(Math.round(progress*100)));return;}
    if(s.manual_queued||manualBusy){$("runTitle").textContent="检测已排队";$("runHint").textContent="等待当前任务完成后自动开始";return;}
    if(s.batch?.status==="completed_with_errors"||s.state?.manual?.status==="interrupted"||s.last_error){
      $("runPanel").dataset.status="warning";$("runTitle").textContent=s.state?.manual?.status==="interrupted"?"上次手动检测已中断":"部分账号需要重试";
      $("runHint").textContent=s.state?.manual?.status==="interrupted"?"服务重启中断了上次任务，可重新发起检测。":s.last_error||`本轮有 ${s.batch?.errors||0} 项未完成，已保留各账号结果，请查看下一步。`;return;
    }
    $("runTitle").textContent=!s.target_count?"选择账号后开始监测":s.enabled?"自动监测已开启":"自动监测已关闭";
    const times=Object.values(s.state?.records||{}).filter(r=>configured.get(r.account_id)?.enabled&&(r.hold||accounts.find(a=>a.id===r.account_id)?.schedulable)).map(r=>new Date(r.next_at).getTime()).filter(n=>Number.isFinite(n)&&n>0);
    const ready=new Date(s.startup_ready_at||0).getTime(),next=times.length?Math.max(Math.min(...times),ready):ready;
    $("runHint").textContent=s.accounts_error?"账号列表同步失败，正在重试":!s.target_count?"打开管理账号，选择需要监测的账号":s.enabled?`下次检查${next>0?" · "+relativeTime(new Date(next).toISOString()):"自动排队"}`:"仍可手动检测已选择的账号";
  }
  function markDirty(){
    dirty=true;collectDrafts();
    for(const row of $("targets").querySelectorAll("tr[data-account-id]")){
      const enabled=row.querySelector('[data-field="enabled"]').checked;
      row.querySelector('[data-field="model"]').required=enabled;row.querySelector('[data-field="expected"]').required=enabled;
    }
    filterAccounts();updateControls();if(lastStatus)renderRecords(lastStatus);
  }
  $("configForm").addEventListener("input",e=>{if(!["accountSearch","accountFilter"].includes(e.target.id))markDirty();});
  $("configForm").addEventListener("change",e=>{if(!["accountSearch","accountFilter"].includes(e.target.id))markDirty();});
  $("accountSearch").oninput=filterAccounts;$("accountFilter").onchange=filterAccounts;
  $("selectVisible").onclick=()=>{for(const row of $("targets").querySelectorAll("tr[data-account-id]")){const box=row.querySelector('[data-field="enabled"]');if(!row.hidden&&!box.disabled)box.checked=true;}markDirty();};
  $("clearSelection").onclick=()=>{for(const box of $("targets").querySelectorAll('[data-field="enabled"]'))if(!box.disabled)box.checked=false;markDirty();};
  $("toggleKey").onclick=()=>{const visible=$("admin_key").type==="password";$("admin_key").type=visible?"text":"password";$("toggleKey").textContent=visible?"隐藏":"显示";$("toggleKey").setAttribute("aria-label",visible?"隐藏管理员访问密钥":"显示管理员访问密钥");$("toggleKey").setAttribute("aria-pressed",String(visible));};
  $("manageAccounts").onclick=()=>{$("configPanel").open=!$("configPanel").open;if($("configPanel").open)$("configPanel").scrollIntoView?.({block:"start",behavior:window.matchMedia?.("(prefers-reduced-motion: reduce)").matches?"auto":"smooth"});};
  $("showSamples").onchange=()=>{if(lastStatus)render(lastStatus);};
  $("configForm").addEventListener("invalid",e=>{for(let n=e.target.parentElement;n;n=n.parentElement)if(n.tagName==="DETAILS")n.open=true;},true);
  $("refresh").onclick=async()=>{
    if(!loaded){try{const r=await send("config.load");fill(r.config||{});}catch(e){message("配置读取失败，请重试："+e.message,true);return;}}
    await refresh(true);
  };
  $("refreshAccounts").onclick=()=>discover($("refreshAccounts"));
  $("diagnose").onclick=()=>discover($("diagnose"));
  async function saveConfig(){
    const draft=readConfig(),current=await send("config.load"),config={...(current.config||{})};
    for(const key of [...bools,...strings,...nums,"targets"])if(JSON.stringify(draft[key])!==JSON.stringify(savedConfig[key]))config[key]=draft[key];
    const r=await send("config.save",{config});fill(r.config||config);
  }
  $("configForm").onsubmit=async e=>{
    e.preventDefault();if(!$("configForm").reportValidity())return;if(busy||manualBusy||batchBusy){if(dirty)message("编辑内容已保留，请在当前检测完成后保存");return;}busy=true;stateEpoch++;$("save").setAttribute("aria-busy","true");updateControls();
    try{await saveConfig();message("配置已保存");}
    catch(e){message(e.message,true);return;}
    finally{busy=false;$("save").removeAttribute("aria-busy");updateControls();}
    await discover($("refreshAccounts"));
  };
  $("runNow").onclick=async()=>{
    if(busy||manualBusy||!runtimeReady||lastStatus?.runtime_error)return;
    if(!$("configForm").reportValidity())return;
    const draft=readConfig(),wasOnboarding=onboarding;
    if(!draft.targets.some(t=>t.enabled)){message("请先勾选至少一个检测账号",true);$("accountSearch").focus();return;}
    busy=true;stateEpoch++;updateControls();
    try{
      if(dirty)await saveConfig();
      // Load the committed host config; do not overwrite it with stale UI state.
      const current=await send("config.load");
      const config={...current.config,manual_request_id:crypto.randomUUID()};
      const saved=await send("config.save",{config});fill(saved.config||config);
      const started=await send("config.test");
      if(!started.result.success)throw new Error(started.result.message||"无法开始检测");
      const status=snapshot(started.result);
      const receipt=status.manual_request;
      const accepted=receipt?.accepted&&receipt.id===config.manual_request_id;
      const matching=status.state?.manual?.id===config.manual_request_id;
      if(!accepted&&!matching&&!(status.manual_busy&&!status.diagnostic))throw new Error("未收到本次检测的入队回执，请刷新状态后重试");
      message(started.result.message||"已加入检测队列");
      render(status);
      if(wasOnboarding){$("configPanel").open=false;$("runTitle").setAttribute("tabindex","-1");$("runTitle").focus({preventScroll:true});}
      await refresh(true);
    }catch(e){message(e.message,true);}
    finally{busy=false;updateControls();}
  };
  $("monitorSearch").oninput=filterRecords;$("monitorFilter").onchange=filterRecords;
  $("configForm").addEventListener("keydown",e=>{if(e.key==="Enter"&&["accountSearch","monitorSearch"].includes(e.target.id))e.preventDefault();});
  $("stepConnect").onclick=()=>openStep("connectSection","admin_url");
  $("stepAccounts").onclick=()=>openStep("accountsSection","accountSearch");
  $("stepReview").onclick=()=>openStep("reviewSection","enabled");
  $("reviewSelection").onclick=()=>openStep("reviewSection","enabled");
  $("discard").onclick=()=>{fill(savedConfig);message("已撤销尚未保存的修改");};
  $("startSetup").onclick=()=>$("runNow").click();
  $("applyRecommended").onclick=()=>{
    collectDrafts();
    for(const a of accounts){const t=drafts.get(a.id);if(t?.enabled)drafts.set(a.id,{...t,model:a.suggested_model||"gpt-6.1-terra",expected:a.suggested_expected||"gpt-6.1-terra"});}
    $("targets").replaceChildren();renderAccounts();markDirty();message("已为所选账号应用推荐设置，保存后生效");
  };
  $("intervalPreset").onchange=()=>{if($("intervalPreset").value==="custom"){$("policyPanel").open=true;$("normal_seconds").focus();}else{$("normal_seconds").value=$("intervalPreset").value;markDirty();}};
  $("connectAccounts").onclick=async()=>{
    if(busy||manualBusy||batchBusy||!loaded)return;
    for(const key of ["admin_url","admin_key"])if(!$(key).reportValidity())return;
    const desired={admin_url:$("admin_url").value.trim(),admin_key:$("admin_key").value.trim()};
    if(!desired.admin_key){message("请填写有效的管理员访问密钥",true);$("admin_key").focus();return;}
    busy=true;stateEpoch++;updateControls();$("connectionFeedback").textContent="正在保存连接并读取账号…";
    let committed=false;
    try{
      const current=await send("config.load"),config={...(current.config||{}),...desired};
      const saved=await send("config.save",{config});savedConfig=JSON.parse(JSON.stringify(saved.config||config));committed=true;
      configured=new Map((savedConfig.targets||[]).map(t=>[t.account_id,{...t}]));
      $("admin_url").value=savedConfig.admin_url;$("admin_key").value=savedConfig.admin_key;
      dirty=JSON.stringify(readConfig())!==JSON.stringify(savedConfig);
      await testSavedConnection();
      await refresh(true);$("connectionFeedback").textContent="管理端已连接，账号已加载";message("连接已保存。现在选择需要监测的账号。");
      if(onboarding)openStep("accountsSection","accountSearch");
    }catch(e){$("connectionFeedback").textContent=(committed?"连接信息已保存，但尚未连通：":"")+e.message;message($("connectionFeedback").textContent,true);await refresh(true);}
    finally{busy=false;updateControls();}
  };
  document.addEventListener("visibilitychange",()=>{if(!document.hidden){if(lastRefresh&&Date.now()-lastRefresh>15000)setStale("正在重新同步，暂时显示上次结果");if(!busy)refresh(true);}});
  const interval=setInterval(()=>{
    if(document.hidden||busy)return;
    if(lastRefresh&&Date.now()-lastRefresh>15000&&runtimeReady)setStale("状态同步延迟，正在重新连接");
    refresh();
  },3000);
  window.addEventListener("beforeunload",e=>{if(dirty){e.preventDefault();e.returnValue="";}});
  window.addEventListener("unload",()=>{clearInterval(interval);resizeObserver?.disconnect();for(const p of pending.values()){clearTimeout(p.timer);p.reject(new Error("页面已关闭"));}pending.clear();});
  document.addEventListener("keydown",()=>{document.documentElement.dataset.keyboard="true";});
  document.addEventListener("pointerdown",()=>{document.documentElement.dataset.keyboard="false";});
  for(const details of document.querySelectorAll("details"))details.addEventListener("toggle",()=>{if(details.open&&!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches&&document.documentElement.dataset.keyboard!=="true")details.querySelector(".grid")?.animate?.([{opacity:0,transform:"translateY(-4px)"},{opacity:1,transform:"translateY(0)"}],{duration:180,easing:"cubic-bezier(.23,1,.32,1)"});});
  let lastHeight=0;
  function contentHeight(){return Math.ceil(document.querySelector("main").getBoundingClientRect().height);}
  function resize(){const height=contentHeight();if(token&&parent!==window&&height!==lastHeight){lastHeight=height;parent.postMessage({source:"sub2api-plugin-ui",bridge_token:token,type:"ui.resize",request_id:crypto.randomUUID(),height},"*");}}
  const resizeObserver=typeof ResizeObserver!=="undefined"?new ResizeObserver(resize):null;resizeObserver?.observe(document.querySelector("main"));
  async function init(){
    if(token&&parent!==window)parent.postMessage({source:"sub2api-plugin-ui",bridge_token:token,type:"sub2api.plugin.ready",request_id:crypto.randomUUID()},"*");
    try{const r=await send("config.load");fill(r.config||{});}catch(e){message(e.message,true);}
    await refresh();
    if(token&&parent!==window)parent.postMessage({source:"sub2api-plugin-ui",bridge_token:token,type:"ui.resize",request_id:crypto.randomUUID(),height:contentHeight()},"*");
  }
  init();
})();
