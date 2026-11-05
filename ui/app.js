"use strict";
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
$("manageAccounts").onclick=()=>{
  $("configPanel").open=!$("configPanel").open;
  if($("configPanel").open){
    $("configPanel").scrollIntoView?.({block:"start",behavior:window.matchMedia?.("(prefers-reduced-motion: reduce)").matches?"auto":"smooth"});
    if(runtimeReady&&savedConfig.admin_key&&!busy)discover($("refreshAccounts"));
  }
};
$("showSamples").onchange=()=>{if(lastStatus)render(lastStatus);};
$("configForm").addEventListener("invalid",e=>{for(let n=e.target.parentElement;n;n=n.parentElement)if(n.tagName==="DETAILS")n.open=true;},true);
$("refresh").onclick=async()=>{
  if(!loaded){try{const r=await send("config.load");fill(r.config||{});}catch(e){message("配置读取失败，请重试："+e.message,true);return;}}
  await refresh(true);
};
$("refreshAccounts").onclick=()=>discover($("refreshAccounts"));
$("diagnose").onclick=()=>discover($("diagnose"));
$("stopNow").onclick=async()=>{
  if(busy||!runtimeReady)return;
  busy=true;stateEpoch++;updateControls();
  try{
    // Stop is an explicit command; preserve both committed settings and UI drafts.
    const current=await send("config.load");
    const config={...current.config,manual_request_id:"",stop_request_id:crypto.randomUUID()};
    await send("config.save",{config});
    savedConfig.manual_request_id="";
    savedConfig.stop_request_id=config.stop_request_id;
    const response=await send("config.test"),result=response.result||{};
    if(!result.success)throw new Error(result.message||"停止请求未被接收");
    const status=snapshot(result),receipt=status.stop_request;
    if(status.diagnostic?.kind!=="stop"||!receipt?.accepted||receipt.id!==config.stop_request_id){
      throw new Error("未收到本次停止请求的回执，请刷新状态后重试");
    }
    render(status);message(result.message||"停止请求已接收，已有暂停保持不变");
    await refresh(true);
  }catch(error){message(error.message,true);}
  finally{busy=false;updateControls();}
};
async function saveConfig(){
  if(!validateGroups())throw new Error("请修正分组或新成员模板配置");
  const draft=readConfig(),current=await send("config.load"),config={...(current.config||{})};
  for(const key of [...bools,...strings,...nums,"targets","group_ids"])if(JSON.stringify(draft[key])!==JSON.stringify(savedConfig[key]))config[key]=draft[key];
  const r=await send("config.save",{config});fill(r.config||config);
}
$("configForm").onsubmit=async e=>{
  e.preventDefault();if(!$("configForm").reportValidity())return;if(busy)return;if((manualBusy||batchBusy)&&hardConfigChanged()){message("连接或状态目录正在使用，请先停止当前检测再保存；其他设置可立即保存");return;}busy=true;stateEpoch++;$("save").setAttribute("aria-busy","true");updateControls();
  try{await saveConfig();message("配置已保存");}
  catch(e){message(e.message,true);return;}
  finally{busy=false;$("save").removeAttribute("aria-busy");updateControls();}
  await refresh(true);
};
$("runNow").onclick=async()=>{
  if(busy||manualBusy||!runtimeReady||lastStatus?.runtime_error)return;
  if(!$("configForm").reportValidity())return;
  if(!validateGroups())return;
  const draft=readConfig(),wasOnboarding=onboarding;
  if(!draft.targets.some(t=>t.enabled)&&!draft.group_ids.length){message("请先勾选至少一个检测账号",true);$("accountSearch").focus();return;}
  busy=true;stateEpoch++;updateControls();
  try{
    if(dirty)await saveConfig();
    // Load the committed host config; do not overwrite it with stale UI state.
    const current=await send("config.load");
    const config={...current.config,manual_request_id:crypto.randomUUID(),stop_request_id:""};
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
  for(const a of accounts){const t=drafts.get(a.id),recommendation=recommendedTarget(a);if(t?.enabled&&recommendation.model&&recommendation.expected)drafts.set(a.id,{...t,...recommendation});}
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
function pollStatus(){
  if(document.hidden||busy)return;
  if(lastRefresh&&Date.now()-lastRefresh>15000&&runtimeReady)setStale("状态同步延迟，正在重新连接");
  refresh();
}
let interval=setInterval(pollStatus,3000);
window.addEventListener("beforeunload",e=>{if(dirty){e.preventDefault();e.returnValue="";}});
window.addEventListener("pagehide",()=>{clearInterval(interval);resizeObserver?.disconnect();for(const p of pending.values()){clearTimeout(p.timer);p.reject(new Error("页面已关闭"));}pending.clear();});
window.addEventListener("pageshow",event=>{if(event.persisted){clearInterval(interval);interval=setInterval(pollStatus,3000);resizeObserver?.observe(document.querySelector("main"));refresh(true);}});
document.addEventListener("keydown",()=>{document.documentElement.dataset.keyboard="true";});
document.addEventListener("pointerdown",()=>{document.documentElement.dataset.keyboard="false";});
for(const details of document.querySelectorAll("details"))details.addEventListener("toggle",()=>{if(details.open&&!window.matchMedia?.("(prefers-reduced-motion: reduce)").matches&&document.documentElement.dataset.keyboard!=="true")details.querySelector(".grid")?.animate?.([{opacity:0,transform:"translateY(-4px)"},{opacity:1,transform:"translateY(0)"}],{duration:180,easing:"cubic-bezier(.23,1,.32,1)"});});
let lastHeight=0;
function contentHeight(){return Math.ceil(document.querySelector("main").getBoundingClientRect().height);}
function resize(){const height=contentHeight();if(token&&parent!==window&&height!==lastHeight){lastHeight=height;postHost({source:"sub2api-plugin-ui",bridge_token:token,type:"ui.resize",request_id:crypto.randomUUID(),height});}}
const resizeObserver=typeof ResizeObserver!=="undefined"?new ResizeObserver(resize):null;resizeObserver?.observe(document.querySelector("main"));
async function init(){
  if(token&&parent!==window)postHost({source:"sub2api-plugin-ui",bridge_token:token,type:"sub2api.plugin.ready",request_id:crypto.randomUUID()});
  try{const r=await send("config.load");fill(r.config||{});}catch(e){message(e.message,true);}
  await refresh();
  if(token&&parent!==window)postHost({source:"sub2api-plugin-ui",bridge_token:token,type:"ui.resize",request_id:crypto.randomUUID(),height:contentHeight()});
}
init();
