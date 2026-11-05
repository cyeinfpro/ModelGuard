"use strict";
const $ = id => document.getElementById(id);
const token = new URLSearchParams(location.hash.slice(1)).get("bridge_token");
const pending = new Map();
let lastStatus = null;
let models = [], accounts = [], loaded = false, polling = false, accountSignature = "";
let accountsByID = new Map(), statusCatalog = {}, defaults = {};
const accountRows = new Map(), monitorRows = new Map(), historyRows = new Map(), eventRows = new Map();
let configured = new Map(), drafts = new Map();
let savedConfig = {}, dirty = false, busy = false, manualBusy = false, batchBusy = false, runtimeReady = false, lastRefresh = 0;
let stateEpoch=0, refreshSequence=0, lastFetchError="", onboarding=false;
const bools = ["enabled", "auto_pause", "auto_recover", "family_match"];
const strings = ["admin_url", "admin_key", "data_dir", "group_model", "group_expected"];
const nums = ["concurrency","normal_seconds", "retry_seconds", "recovery_spacing_seconds", "quarantine_seconds", "timeout_seconds", "daily_requests"];
const names = {paused_observation:"暂停中 · 仅复测",passed:"通过",suspect:"待复核",unavailable:"采样失败",inconclusive:"评分不确定",quarantined:"指纹不符",recovering:"恢复复核中",recovery_ready:"等待自动恢复",pause_recommended:"建议隔离",external_change:"旧保护记录 · 待复测",skipped:"跳过",blocked:"需处理",unsupported:"不支持",pause_uncertain:"暂停待核对 · 待复测",unverified:"待检测",pending:"等待配置",archived:"已归档"};
const modes = {disabled:"监测未开启",running:"监测已启用",waiting_targets:"等待选择账号",connection_error:"管理连接异常",loading_accounts:"正在加载账号"};

function statusLabel(status) {
  return statusCatalog[status]?.label || names[status] || "待同步";
}

function setText(node, value) {
  const text = String(value ?? "");
  if (node.textContent !== text) node.textContent = text;
}

function syncRows(container, nodes, cache) {
  const keep = new Set(nodes);
  for (const child of Array.from(container.children)) if (!keep.has(child)) child.remove();
  nodes.forEach((node, index) => {
    if (container.children[index] !== node) container.insertBefore(node, container.children[index] || null);
  });
  if (cache) for (const [id, row] of cache) if (!keep.has(row)) cache.delete(id);
}

function emptyTable(container, text, columns) {
  let row = container.querySelector("tr[data-empty]");
  if (!row) {
    row = document.createElement("tr");
    row.dataset.empty = "true";
    const td = cell(row, text);
    td.colSpan = columns;
    td.className = "empty";
  } else setText(row.cells[0], text);
  syncRows(container, [row]);
}

function recommendedTarget(account) {
  const api = account.type === "apikey";
  return {
    model: account.suggested_model || defaults[api ? "api_model" : "oauth_model"] || "",
    expected: account.suggested_expected || defaults[api ? "api_expected" : "oauth_expected"] || ""
  };
}

function hardConfigChanged() {
  return ["admin_url", "admin_key", "data_dir"].some(key => $(key).value.trim() !== (savedConfig[key] || ""));
}
