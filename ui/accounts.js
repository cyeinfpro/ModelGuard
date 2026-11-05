"use strict";
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
  $("selectVisible").disabled=visible===0||busy;
  $("clearSelection").disabled=count===0||busy;
  $("applyRecommended").disabled=count===0||busy;
  $("reviewSelection").disabled=count===0;
}
function renderAccounts() {
  if (!loaded) return;
  collectDrafts();
  const listed = new Map(accountsByID);
  for (const [id, target] of new Map([...configured, ...drafts])) {
    const record = lastStatus?.state?.records?.[id];
    if (!listed.has(id) && (target.enabled || record?.hold || record?.pending)) {
      listed.set(id, {id, name: `账号 #${id}`, type: "", status: "unavailable", supported: false, missing: true});
    }
  }
  const rows = [];
  for (const account of listed.values()) {
    const recommendation = recommendedTarget(account);
    const target = drafts.get(account.id) || {account_id: account.id, enabled: false, ...recommendation};
    const usable = !!account.supported && !(account.type === "apikey" && lastStatus?.api_bridge_ready === false);
    let row = accountRows.get(account.id);
    if (!row) {
      row = document.createElement("tr");
      row.dataset.accountId = String(account.id);
      const enabled = document.createElement("input");
      enabled.type = "checkbox";
      enabled.dataset.field = "enabled";
      cell(row, "").append(enabled);
      const name = cell(row, "");
      name.className = "account-name";
      const title = document.createElement("span"), id = document.createElement("span");
      id.className = "account-id";
      name.append(title, id);
      const status = cell(row, "");
      status.className = "account-status";
      const details = document.createElement("details");
      details.className = "target-details";
      details.dataset.accountDetails = String(account.id);
      const summary = document.createElement("summary"), summaryText = document.createElement("span");
      const label = document.createElement("span"), caption = document.createElement("span");
      caption.className = "model-caption";
      summaryText.append(label, caption);
      summary.append(summaryText);
      details.append(summary);
      const modelLabel = document.createElement("label");
      modelLabel.textContent = "发给渠道的模型名称";
      const model = document.createElement("input");
      model.dataset.field = "model";
      model.spellcheck = false;
      modelLabel.append(model);
      const expectedLabel = document.createElement("label");
      expectedLabel.textContent = "预期的指纹参考类";
      const expected = document.createElement("select");
      expected.dataset.field = "expected";
      expectedLabel.append(expected);
      const help = document.createElement("p");
      details.append(modelLabel, expectedLabel, help);
      cell(row, "").append(details);
      row._parts = {enabled, title, id, status, label, caption, model, expected, help};
      accountRows.set(account.id, row);
    }
    const p = row._parts;
    row.dataset.schedulable = String(!!account.schedulable);
    row.dataset.supported = String(usable);
    row.dataset.search = `${account.name} ${account.id}`.toLowerCase();
    p.enabled.checked = !!target.enabled;
    p.enabled.disabled = !usable && !target.enabled;
    p.enabled.setAttribute("aria-label", `监测 ${account.name}`);
    setText(p.title, account.name || `账号 #${account.id}`);
    setText(p.id, `#${account.id} · ${account.type === "oauth" ? "OAuth" : account.type === "apikey" ? "API Key" : account.type || "账号不可见"}`);
    const bridgeMissing = account.type === "apikey" && lastStatus?.api_bridge_ready === false;
    setText(p.status, bridgeMissing ? "本机桥接未就绪" : account.missing ? "当前密钥不可见" : !account.supported ? "暂不支持" : account.status !== "active" ? account.status : account.schedulable ? "可调度" : "已暂停");
    setText(p.label, target.model === recommendation.model && target.expected === recommendation.expected ? "按推荐设置" : "自定义模型");
    setText(p.caption, target.model);
    if (p.model.value !== (target.model || "")) p.model.value = target.model || "";
    p.model.required = !!target.enabled;
    p.model.setAttribute("aria-label", `${account.name} 请求模型`);
    p.expected.required = !!target.enabled;
    p.expected.setAttribute("aria-label", `${account.name} 预期指纹`);
    const choices = models.length ? [...models] : [];
    if (!choices.some(model => model.id === target.expected)) choices.push({id: target.expected || "", display_name: target.expected || "请选择参考类"});
    const signature = choices.map(model => `${model.id}\0${model.display_name || model.id}`).join("\n");
    if (p.expected.dataset.choices !== signature) {
      p.expected.replaceChildren(...choices.map(model => {
        const option = document.createElement("option");
        option.value = model.id;
        option.textContent = model.display_name || model.id;
        return option;
      }));
      p.expected.dataset.choices = signature;
    }
    p.expected.value = target.expected || "";
    setText(p.help, bridgeMissing ? (lastStatus.api_bridge_message || "先安装或启动本机凭据桥接；OAuth 账号可以直接监测。") : !recommendation.model ? "暂未收到推荐值，请重新加载账号或填写请求模型与预期指纹。" : "通常保持推荐值即可。渠道模型名可被映射，参考类用于比较回答的统计特征，两者不必同名。");
    rows.push(row);
  }
  if (rows.length) syncRows($("targets"), rows, accountRows);
  else {
    accountRows.clear();
    emptyTable($("targets"), savedConfig.admin_key ? "还没有可见账号。请检查管理密钥的账号授权范围，然后重新加载。" : "先连接管理端，授权账号会自动显示在这里。", 4);
  }
  filterAccounts();
}
function groupIDs(){return $("group_ids").value.trim().split(/[,，\s]+/).filter(Boolean).map(Number);}
function validateGroups(){const ids=groupIDs();if(ids.length>100||ids.some(id=>!Number.isSafeInteger(id)||id<=0)||new Set(ids).size!==ids.length){message("分组 ID 必须为不重复的正整数，最多 100 个",true);return false;}if(!!$("group_model").value.trim()!==!!$("group_expected").value.trim()){message("新成员请求模型与预期指纹需同时填写，或同时留空",true);return false;}return true;}
function activeTargets(s=lastStatus){return savedConfig.group_ids?.length?(s?.group_sync_error?[]:(s?.targets||[])):Array.from(configured.values());}
function readConfig(){
  collectDrafts();const c={...savedConfig};
  for(const k of bools)c[k]=$(k).checked;
  for(const k of strings)c[k]=$(k).value.trim();
  for(const k of nums)c[k]=Number($(k).value);
  c.group_ids=groupIDs();
  if(savedConfig.manual_request_id)c.manual_request_id=savedConfig.manual_request_id;
  c.targets=Array.from(drafts.values()).filter(t=>t.enabled||(configured.has(t.account_id)&&(accountsByID.has(t.account_id)||lastStatus?.accounts_error||lastStatus?.accounts_loading))||lastStatus?.state?.records?.[t.account_id]?.hold||lastStatus?.state?.records?.[t.account_id]?.pending);
  return c;
}
function fill(c){
  savedConfig=JSON.parse(JSON.stringify(c));dirty=false;
  onboarding=!c.admin_key||(!(c.targets||[]).some(t=>t.enabled)&&!c.group_ids?.length);
  for(const k of bools)$(k).checked=!!c[k];
  for(const k of [...strings,...nums])if(k in c)$(k).value=c[k];
  $("recovery_spacing_seconds").value=c.recovery_spacing_seconds||0;
  $("group_ids").value=(c.group_ids||[]).join(", ");
  for(const key of ["group_model","group_expected"])$(key).value=c[key]||"";
  configured=new Map((c.targets||[]).map(t=>[t.account_id,{...t}]));drafts=new Map((c.targets||[]).map(t=>[t.account_id,{...t}]));
  $("targets").replaceChildren();loaded=true;renderAccounts();updateControls();if(!c.admin_key||!(c.targets||[]).some(t=>t.enabled))$("configPanel").open=true;if(lastStatus)render(lastStatus);updateFlow();
}
