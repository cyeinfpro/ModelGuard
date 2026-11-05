import test from 'node:test';
import assert from 'node:assert/strict';
import {setup, tick, scriptNames, js} from './ui-fixture.mjs';

test('inconclusive samples show top candidates and their own counter', async () => {
  const {dom, w, state} = await setup();
  state.status_catalog = {inconclusive: {label: '评分不确定', category: 'attention'}};
  state.state.records[1] = {
    account_id: 1, status: 'inconclusive', hold: true, inconclusive: 2, unavailable: 0,
    checked_at: '2026-11-05T00:00:00Z', reason: '候选模型接近',
    analysis: {predicted: 'model-a', candidates: [
      {model: 'model-a', name: '模型 A', probability: 0.41},
      {model: 'model-b', name: '模型 B', probability: 0.37},
      {model: 'model-c', name: '<b>模型 C</b>', probability: 0.18},
      {model: 'model-d', name: '不应显示的第四名', probability: 0.04}
    ]}
  };
  w.document.getElementById('refresh').click();
  await tick();
  const row = w.document.querySelector('#records tr[data-account-id="1"]');
  assert.equal(row.querySelector('.tag').textContent, '评分不确定');
  assert.match(row.textContent, /已完成采样，但参考库无法可靠区分/);
  assert.match(row.textContent, /模型 A 41%.*模型 B 37%.*模型 C/);
  assert.match(row.textContent, /0 次采样失败 \/ 2 次评分不确定/);
  assert(!row.textContent.includes('不应显示的第四名'));
  assert.equal(row.querySelectorAll('b').length, 0);
  assert.equal(row.dataset.attention, 'true');
  dom.window.close();
});

test('audit remains the default view even when sampling activity is very busy', async () => {
  const {dom, w, state} = await setup();
  state.state.audit_events = [{at: '2026-11-05T00:00:00Z', account_id: 1, action: 'paused', reason: '已暂停，等待复测'}];
  state.state.activity_events = Array.from({length: 400}, (_, i) => ({
    at: new Date(Date.parse('2026-11-05T01:00:00Z') + i * 1000).toISOString(),
    account_id: 1, action: i % 2 ? 'sample_retry' : 'check', reason: `采样日志 ${i}`
  }));
  w.document.getElementById('refresh').click();
  await tick();
  const auditNode = w.document.querySelector('#events .event');
  assert.match(auditNode.textContent, /已暂停/);
  assert(!w.document.getElementById('events').textContent.includes('采样日志'));
  w.document.getElementById('refresh').click();
  await tick();
  assert.equal(w.document.querySelector('#events .event'), auditNode);
  w.document.getElementById('showSamples').click();
  assert.match(w.document.getElementById('events').textContent, /采样日志 399/);
  w.document.getElementById('showSamples').click();
  assert.match(w.document.getElementById('events').textContent, /已暂停/);
  dom.window.close();
});

test('account revisions and keyed rows retain focus and unchanged cells', async () => {
  const {dom, w, state} = await setup({state: {accounts_revision: 'one'}});
  const account = w.document.querySelector('#targets tr[data-account-id="1"]');
  const input = account.querySelector('[data-field="model"]');
  const record = w.document.querySelector('#records tr[data-account-id="1"]');
  const details = record.querySelector('details');
  details.open = true;
  input.focus();
  input.setSelectionRange(2, 4);
  state.accounts[0].name = '修订号未变的内容';
  w.document.getElementById('refresh').click();
  await tick();
  assert(!account.textContent.includes('修订号未变的内容'));
  state.accounts_revision = 'two';
  state.accounts[0].name = '账号新名称';
  state.state.records[1].reason = '已更新的原因';
  w.document.getElementById('refresh').click();
  await tick();
  assert.equal(w.document.querySelector('#targets tr[data-account-id="1"]'), account);
  assert.equal(w.document.querySelector('#targets tr[data-account-id="1"] [data-field="model"]'), input);
  assert.equal(w.document.querySelector('#records tr[data-account-id="1"]'), record);
  assert.equal(record.querySelector('details'), details);
  assert.equal(details.open, true);
  assert.equal(w.document.activeElement, input);
  assert.equal(input.selectionStart, 2);
  assert.match(account.textContent, /账号新名称/);
  assert.match(record.textContent, /已更新的原因/);
  dom.window.close();
});

test('recommended models come from server defaults and unknown statuses use the catalog', async () => {
  const {dom, w, state} = await setup({state: {
    accounts: [{id: 1, name: '服务器推荐账号', type: 'oauth', supported: true, status: 'active', schedulable: true},
      {id: 2, name: '新账号', type: 'apikey', supported: true, status: 'active', schedulable: true}],
    defaults: {oauth_model: 'future-oauth', oauth_expected: 'future-reference', api_model: 'future-api', api_expected: 'api-reference'},
    models: [{id: 'future-reference'}, {id: 'api-reference'}],
    status_catalog: {pending: {label: '等待补齐模型', category: 'pending'}}
  }});
  const second = w.document.querySelector('#targets tr[data-account-id="2"]');
  assert.equal(second.querySelector('[data-field="model"]').value, 'future-api');
  assert.equal(second.querySelector('[data-field="expected"]').value, 'api-reference');
  state.state.records[1].status = 'pending';
  w.document.getElementById('refresh').click();
  await tick();
  assert.match(w.document.querySelector('#records tr[data-account-id="1"]').textContent, /等待补齐模型/);
  assert(!js.includes('gpt-6.1-terra'));
  assert(!js.includes('gpt-6.1-sol'));
  dom.window.close();
});

test('stop uses a fresh command and never commits unrelated draft settings', async () => {
  const {dom, w, state, calls, config} = await setup({state: {batch_busy: true, active_samples: [{account_id: 1, index: 1, phase: 'output'}]},
    respond({m, response, state, config}) {
      if (m.type === 'config.test' && config.stop_request_id) {
        state.batch_busy = false;
        state.manual_busy = false;
        state.active_samples = [];
        state.diagnostic = {kind: 'stop'};
        state.stop_request = {id: config.stop_request_id, accepted: true};
        response.result = {success: true, message: '当前检测已停止', status_json: JSON.stringify(state)};
      }
    }
  });
  const timing = w.document.getElementById('normal_seconds');
  timing.value = '2400';
  timing.dispatchEvent(new w.Event('input', {bubbles: true}));
  w.document.getElementById('stopNow').click();
  await tick();
  await tick();
  const command = calls.find(call => call.type === 'config.save');
  assert(command.config.stop_request_id);
  assert.equal(command.config.manual_request_id, '');
  assert.equal(command.config.normal_seconds, 1800);
  assert.equal(config.enabled, true);
  assert.equal(timing.value, '2400');
  assert.match(w.document.getElementById('saveState').textContent, /未保存/);
  assert.match(w.document.getElementById('message').textContent, /已停止/);
  assert.equal(w.document.getElementById('stopNow').hidden, true);
  dom.window.close();
});

test('a successful unrelated diagnostic is not a stop receipt', async () => {
  const {dom, w} = await setup({state: {batch_busy: true}, respond({m, response, state}) {
    if (m.type === 'config.test') response.result = {success: true, status_json: JSON.stringify({...state, diagnostic: {kind: 'connection'}})};
  }});
  w.document.getElementById('stopNow').click();
  await tick();
  await tick();
  assert.match(w.document.getElementById('message').textContent, /未收到本次停止请求的回执/);
  dom.window.close();
});

test('known parent origin is pinned for every bridge request', async () => {
  const origin = 'https://sub2api.invalid';
  const {dom, origins, w} = await setup({referrer: origin + '/admin/plugins', hostOrigin: origin});
  w.document.getElementById('configForm').dispatchEvent(new w.Event('submit', {cancelable: true}));
  await tick();
  assert(origins.length > 0);
  assert(origins.every(item => item.origin === origin));
  assert(origins.some(item => item.type === 'config.save'));
  dom.window.close();
});

test('wrong-origin responses cannot replace a configured admin secret', async () => {
  const origin = 'https://sub2api.invalid';
  const {dom, w} = await setup({referrer: origin + '/admin/plugins', hostOrigin: origin,
    respond({m, response, w, deliver}) {
      if (m.type === 'config.load') {
        const forged = {...response, config: {...response.config, admin_key: 'forged-origin-key'}};
        queueMicrotask(() => {
          w.dispatchEvent(new w.MessageEvent('message', {source: w.parent, origin: 'https://unrelated.invalid', data: forged}));
          deliver();
        });
        return false;
      }
    }
  });
  assert.equal(w.document.getElementById('admin_key').value, 'test-secret');
  dom.window.close();
});

test('optional family matching and recovery spacing stay explicit and default off', async () => {
  const {dom, w, calls} = await setup();
  assert.equal(w.document.getElementById('family_match').checked, false);
  assert.equal(w.document.getElementById('recovery_spacing_seconds').value, '0');
  w.document.getElementById('family_match').checked = true;
  w.document.getElementById('recovery_spacing_seconds').value = '1800';
  w.document.getElementById('family_match').dispatchEvent(new w.Event('change', {bubbles: true}));
  w.document.getElementById('configForm').dispatchEvent(new w.Event('submit', {cancelable: true}));
  await tick();
  const saved = calls.find(call => call.type === 'config.save').config;
  assert.equal(saved.family_match, true);
  assert.equal(saved.recovery_spacing_seconds, 1800);
  assert.equal(saved.auto_recover, true);
  assert.deepEqual(scriptNames, ['state.js', 'bridge.js', 'accounts.js', 'monitor.js', 'flow.js', 'app.js']);
  assert(js.includes('"pagehide"'));
  assert(!js.includes('"unload"'));
  dom.window.close();
});

test('continuous scheduling shows a time window without a false completion percentage', async () => {
  const {dom, w} = await setup({state: {
    batch_busy: true,
    batch: {kind: 'scheduled', window_seconds: 3600, completed: 96, errors: 5, inconclusive: 3, active_ids: [1], queued_ids: [2]},
    active_samples: [{account_id: 1, index: 1, phase: 'connecting'}]
  }});
  assert.equal(w.document.getElementById('runProgress').hidden, true);
  assert.match(w.document.getElementById('windowStats').textContent, /近 1 小时.*完成 96.*需关注 5（其中不确定 3）.*进行中 1/);
  assert.equal(w.document.getElementById('runCount').textContent, '1 个进行中');
  dom.window.close();
});
