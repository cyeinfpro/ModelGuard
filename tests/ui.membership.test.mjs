import test from 'node:test';
import assert from 'node:assert/strict';
import {setup, tick} from './ui-fixture.mjs';

test('three removed holds leave exactly three monitored accounts and retain history', async () => {
  const enabled = [13002, 13024, 13033], removed = [13025, 13026, 13027];
  const ids = [...enabled, ...removed];
  const targets = ids.map(account_id => ({account_id, enabled: enabled.includes(account_id), model: 'gpt-6.1-terra', expected: 'gpt-6.1-terra'}));
  const records = Object.fromEntries(ids.map(account_id => [account_id, {account_id, status: removed.includes(account_id) ? 'quarantined' : 'passed', hold: removed.includes(account_id), recovery_passes: removed.includes(account_id) ? 1 : 0}]));
  const before = JSON.stringify(records);
  const {dom, w, calls, state} = await setup({config: {targets}, state: {targets, configured_targets: targets, target_count: 3, accounts: ids.map(id => ({id, name: `账号 ${id}`, status: 'active', schedulable: enabled.includes(id), supported: true})), state: {records, events: []}}});
  const rows = selector => Array.from(w.document.querySelectorAll(selector), row => Number(row.dataset.accountId));
  assert.deepEqual(rows('#records tr[data-account-id]'), enabled);
  assert.deepEqual(rows('#historyRecords tr[data-account-id]'), removed);
  assert.equal(w.document.getElementById('monitorSummary').textContent, '3 个账号');
  assert.equal(w.document.getElementById('targetCount').textContent, '3');
  assert.equal(w.document.getElementById('holdCount').textContent, '0');
  assert.equal(w.document.getElementById('recoverCount').textContent, '0');
  assert.equal(w.document.getElementById('historyPanel').open, false);
  assert.equal(JSON.stringify(state.state.records), before);
  assert.equal(calls.filter(call => call.type === 'config.save').length, 0);
  dom.window.close();
});

test('save removes a hold, draft re-selection does not count it, and saved re-selection restores it', async () => {
  const {dom, w, state, calls} = await setup();
  state.state.records[1].recovery_passes = 1;
  const toggle = checked => {
    const box = w.document.querySelector('#targets tr[data-account-id="1"] [data-field="enabled"]');
    box.checked = checked;
    box.dispatchEvent(new w.Event('change', {bubbles: true}));
  };
  const save = async () => {
    w.document.getElementById('configForm').dispatchEvent(new w.Event('submit', {cancelable: true}));
    await tick();
  };
  toggle(false);
  assert(w.document.querySelector('#records tr[data-account-id="1"]'));
  await save();
  assert.equal(w.document.querySelector('#records tr[data-account-id="1"]'), null);
  assert(w.document.querySelector('#historyRecords tr[data-account-id="1"]'));
  assert.equal(w.document.getElementById('holdCount').textContent, '0');
  toggle(true);
  assert.match(w.document.querySelector('#records tr[data-account-id="1"]').textContent, /待保存/);
  assert.equal(w.document.getElementById('holdCount').textContent, '0');
  assert.equal(w.document.getElementById('recoverCount').textContent, '0');
  await save();
  assert.equal(w.document.querySelector('#historyRecords tr[data-account-id="1"]'), null);
  assert.equal(w.document.getElementById('holdCount').textContent, '1');
  assert.equal(w.document.getElementById('recoverCount').textContent, '1');
  assert.equal(state.state.records[1].hold, true);
  assert.equal(calls.filter(call => call.type === 'config.save').length, 2);
  dom.window.close();
});

test('unmanaged pending changes stay in history without claiming a confirmed pause', async () => {
  const {dom, w} = await setup({config: {targets: []}, state: {target_count: 0, targets: [], state: {records: {2: {account_id: 2, status: 'pause_uncertain', pending: true, hold: false}}, events: []}}});
  assert.equal(w.document.querySelectorAll('#records tr[data-account-id]').length, 0);
  const row = w.document.querySelector('#historyRecords tr[data-account-id="2"]');
  assert.match(row.textContent, /未监测 · 调度变更待核对.*不自动检测或恢复/);
  assert(!row.textContent.includes('保留暂停'));
  assert.equal(w.document.getElementById('holdCount').textContent, '0');
  dom.window.close();
});
