#!/usr/bin/env node
/*
 * Browser test of the panel UI against a LIVE panel, driven over the Chrome
 * DevTools Protocol.
 *
 *   chromium --headless=new --disable-gpu --remote-debugging-port=9222 \
 *            --user-data-dir=/tmp/uitest-profile about:blank &
 *   node test_unit/live/uitest.js [BASE_URL] [USER] [PASS]
 *
 * Everything it creates is named uitest-* / uitest@* and it tears all of it
 * down, so it is safe to point at a panel that is serving real customers.
 *
 * It exists because neither the Go tests nor functest.py can see any of this.
 * A template-parse test proves the Go template compiles; it does not prove the
 * page RENDERS, and a Vue error inside a render leaves a valid 200 on the wire
 * with a completely blank page behind v-cloak. Three real defects came out of
 * the first run:
 *
 *   - the membership modal had no Vue instance of its own, so it was inert
 *     markup: every button that opened it did nothing at all, silently
 *   - the enable toggle posted a client carrying no credential, which the
 *     server refuses with "empty client ID"
 *   - a bulk operation wrote settings.clients and client_traffics but left the
 *     accounts layer alone, so the Clients page showed the previous quota
 */
'use strict';

const BASE = process.argv[2] || 'http://127.0.0.1:12345';
const USER = process.argv[3] || 'a';
const PASS = process.argv[4] || 'a';
const CDP_PORT = Number(process.env.CDP_PORT || 9222);

const EMAIL = 'uitest@ui';
const REMARKS = ['uitest-vmess', 'uitest-trojan'];

const results = [];
const check = (name, ok, detail) => results.push({ name, ok: !!ok, detail: detail || '' });
const wait = (ms) => new Promise((r) => setTimeout(r, ms));

let ws;
let msgId = 0;
const problems = [];

function rpc(method, params) {
  const id = ++msgId;
  return new Promise((resolve, reject) => {
    const onMsg = (raw) => {
      const m = JSON.parse(raw.data ?? raw);
      if (m.id === id) {
        ws.removeEventListener('message', onMsg);
        resolve(m);
      }
    };
    ws.addEventListener('message', onMsg);
    ws.send(JSON.stringify({ id, method, params }));
    setTimeout(() => reject(new Error('CDP timeout: ' + method)), 30000);
  });
}

async function evalJs(expression) {
  const r = await rpc('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
  if (r.result && r.result.exceptionDetails) {
    const d = r.result.exceptionDetails;
    return { __threw: (d.exception && d.exception.description) || d.text };
  }
  return r.result && r.result.result ? r.result.result.value : undefined;
}

async function goto(path) {
  await rpc('Page.navigate', { url: BASE + path });
  await wait(3000);
}

// Re-injected after every navigation: a page load wipes them, and forgetting that
// makes a later step fail with "__openModals is not a function" rather than with
// whatever it was meant to be testing.
async function inject() {
  await evalJs(`window.__openModals = () =>
     Array.from(document.querySelectorAll('.ant-modal-wrap'))
       .filter(w => w.style.display !== 'none')
       .map(w => (w.querySelector('.ant-modal-title') || {}).innerText || '?');
   window.__closeAll = () => Array.from(document.querySelectorAll('.ant-modal-wrap'))
       .filter(w => w.style.display !== 'none')
       .forEach(w => { const b = w.querySelector('.ant-modal-close'); if (b) b.click(); });
   window.__rowFor = (email) => Array.from(document.querySelectorAll('.ant-table-tbody tr'))
       .find(r => (r.innerText || '').includes(email));
   true`);
}

// Ticks or unticks ONE inbound in the membership modal, addressed by inbound id.
// Never by checkbox index: the modal lists every assignable inbound, not just the
// account's, so an index picks a different inbound on any panel that has others.
async function toggleMembership(inboundId) {
  return await evalJs(`(() => {
     const i = clientMembershipModal.assignable.findIndex(a => a.inboundId === ${inboundId});
     if (i < 0) return false;
     const boxes = document.querySelectorAll('#client-membership-modal .ant-checkbox-group .ant-checkbox-input');
     if (!boxes[i]) return false;
     boxes[i].click();
     return true;
   })()`);
}

const seededInboundIds = async () => await evalJs(
  `clientMembershipModal.assignable.filter(a => ${JSON.stringify(REMARKS)}.includes(a.remark))
     .map(a => a.inboundId)`);

// Every API call runs inside the page so it carries the session cookie the panel
// issued to the browser, which is the same one the UI uses.
async function api(path, body) {
  return await evalJs(`(async () => {
    const opt = { credentials: 'include', headers: { 'X-Requested-With': 'XMLHttpRequest' } };
    ${body === undefined ? '' : `
    opt.method = 'POST';
    opt.headers['Content-Type'] = 'application/x-www-form-urlencoded';
    opt.body = ${JSON.stringify(body)};`}
    const r = await fetch(${JSON.stringify(BASE)} + ${JSON.stringify(path)}, opt);
    try { return await r.json(); } catch (e) { return { success: false, status: r.status }; }
  })()`);
}

const form = (obj) =>
  Object.entries(obj).map(([k, v]) => encodeURIComponent(k) + '=' + encodeURIComponent(v)).join('&');

const accounts = async () => {
  const j = await api('/panel/api/clients/list?page=1&size=200');
  return ((j && j.obj && j.obj.rows) || []).map((x) => ({
    email: x.email, enable: x.enable, totalGB: x.totalGB, n: (x.memberships || []).length,
  }));
};

async function seed() {
  const uuid = await evalJs('crypto.randomUUID()');
  const mk = (remark, protocol, port, settings) =>
    api('/panel/api/inbounds/add', form({
      up: 0, down: 0, total: 0, remark, enable: 'true', expiryTime: 0, listen: '', port, protocol,
      settings: JSON.stringify(settings),
      streamSettings: JSON.stringify({ network: 'tcp', security: 'none' }),
      sniffing: JSON.stringify({ enabled: false, destOverride: [] }),
      allocate: JSON.stringify({}),
    }));
  await mk(REMARKS[0], 'vmess', 34801, {
    clients: [{ id: uuid, email: 'uitest-seed@ui', enable: true, totalGB: 0,
                expiryTime: 0, limitIp: 0, subId: '', comment: '' }],
  });
  await mk(REMARKS[1], 'trojan', 34802, {
    clients: [{ password: 'uitest-seed-pw', email: 'uitest-seed2@ui', enable: true, totalGB: 0,
                expiryTime: 0, limitIp: 0, subId: '', comment: '' }],
    fallbacks: [],
  });
}

async function teardown() {
  const j = await api('/panel/api/inbounds/list');
  for (const ib of (j && j.obj) || []) {
    if (REMARKS.includes(ib.remark)) {
      await api('/panel/api/inbounds/del/' + ib.id, '');
    }
  }
}

async function main() {
  const targets = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
  const page = targets.find((t) => t.type === 'page');
  if (!page) throw new Error('no page target on the debugging port');
  ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise((r) => ws.addEventListener('open', r));
  ws.addEventListener('message', (raw) => {
    const m = JSON.parse(raw.data ?? raw);
    if (m.method === 'Runtime.exceptionThrown') {
      const d = m.params.exceptionDetails;
      problems.push('EXCEPTION: ' + ((d.exception && d.exception.description) || d.text));
    }
    if (m.method === 'Runtime.consoleAPICalled' && m.params.type === 'error') {
      problems.push('CONSOLE: ' + m.params.args.map((a) => a.description || a.value).join(' '));
    }
  });
  await rpc('Runtime.enable', {});
  await rpc('Page.enable', {});

  await goto('/');
  const login = await api('/login', form({ username: USER, password: PASS }));
  if (!login || !login.success) throw new Error('login failed: ' + JSON.stringify(login));

  await teardown();
  await seed();

  // ---------------------------------------------------------------- rendering
  console.log('\n=== rendering ===');
  for (const p of ['/panel/inbounds', '/panel/clients', '/panel/settings', '/panel/']) {
    problems.length = 0;
    await goto(p);
    const info = await evalJs(`(() => {
      const app = document.querySelector('#app');
      if (!app) return { ok: false, why: 'no #app' };
      return {
        // v-cloak is removed by the stylesheet only once Vue has mounted, so a
        // page that still carries it never rendered at all.
        ok: !app.hasAttribute('v-cloak') && app.innerText.trim().length > 40,
        chars: app.innerText.trim().length,
        title: (document.querySelector('.bo-topbar-title') || {}).innerText || '',
      };
    })()`);
    check(`${p} renders`, info && info.ok, JSON.stringify(info));
    check(`${p} logs no console error`, problems.length === 0, problems.slice(0, 2).join(' | '));
  }

  // ------------------------------------------------------------ clients page
  console.log('\n=== the Clients page ===');
  await goto('/panel/clients');
  await inject();

  await evalJs(`Array.from(document.querySelectorAll('button'))
     .find(b => (b.innerText || '').includes('Add Client')).click(); true`);
  await wait(900);
  let open = await evalJs('window.__openModals()');
  check('Add Client opens its modal', Array.isArray(open) && open.length > 0, JSON.stringify(open));
  const boxes = await evalJs(
    `document.querySelectorAll('#client-membership-modal .ant-checkbox-group .ant-checkbox-input').length`);
  check('the modal lists the assignable inbounds', boxes > 0, 'checkboxes=' + boxes);

  // --------------------------------------------------- create, through the UI
  await evalJs(`(() => {
     const inp = document.querySelector('#client-membership-modal input.ant-input');
     const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
     setter.call(inp, ${JSON.stringify(EMAIL)});
     inp.dispatchEvent(new Event('input', { bubbles: true }));
     return true;
   })()`);
  await wait(400);
  // One click per tick: a-checkbox-group only learns the new value through its
  // own watcher, so two clicks in the same tick both compute from the empty set
  // and the second REPLACES the first. A human cannot hit that; a loop can.
  const seeded = (await seededInboundIds()) || [];
  for (const id of seeded) {
    await toggleMembership(id);
    await wait(400);
  }
  await evalJs(`(() => {
     const b = Array.from(document.querySelectorAll('#client-membership-modal .ant-modal-footer button'))
       .find(x => !/close|cancel/i.test(x.innerText));
     if (b) b.click();
     return true;
   })()`);
  await wait(2500);
  let made = (await accounts()).find((a) => a.email === EMAIL);
  check('creating from the Clients page writes the account', !!made);
  check('and it lands on every inbound that was ticked', made && made.n === 2,
        made ? 'memberships=' + made.n : 'missing');

  // ------------------------------------------------------------- row actions
  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const b = r && r.querySelector('button .anticon-edit');
     if (b) b.closest('button').click();
     return true;
   })()`);
  await wait(1200);
  open = await evalJs('window.__openModals()');
  check('Edit opens the full client form', Array.isArray(open) && open.length > 0, JSON.stringify(open));
  const inputs = await evalJs(`document.querySelectorAll('#client-modal input').length`);
  check('the client form carries its protocol fields', inputs >= 3, 'inputs=' + inputs);
  await evalJs('window.__closeAll()');
  await wait(600);

  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const b = r && r.querySelector('button .anticon-cluster');
     if (b) b.closest('button').click();
     return true;
   })()`);
  await wait(900);
  open = await evalJs('window.__openModals()');
  check('the Inbounds button opens the membership modal',
        Array.isArray(open) && open.length > 0, JSON.stringify(open));
  await evalJs('window.__closeAll()');
  await wait(600);

  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const b = r && r.querySelector('button .anticon-pause');
     if (b) b.closest('button').click();
     return true;
   })()`);
  await wait(2500);
  const toggled = (await accounts()).find((a) => a.email === EMAIL);
  check('the enable toggle reaches every membership', toggled && toggled.enable === false,
        toggled ? 'enable=' + toggled.enable : 'missing');

  // ------------------------------------------- the per-inbound controls, inline
  const inline = await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     if (!r) return { lines: 0, icons: 0 };
     return {
       lines: r.querySelectorAll('.bo-client-line').length,
       icons: r.querySelectorAll('.bo-client-line .bo-client-actions .anticon').length,
       expanders: document.querySelectorAll('.ant-table-row-expand-icon').length,
     };
   })()`);
  check('the row shows a line per inbound serving the account',
        inline && inline.lines >= 2, JSON.stringify(inline));
  check('each line carries the client action icons, with no expander to open',
        inline && inline.icons > 0 && inline.expanders === 0, JSON.stringify(inline));

  // ------------------------------------------- changing which inbounds serve it
  // This is what answered "empty client ID": the write used to be addressed to
  // the lowest id in the NEW set, which is an inbound the account is not on yet.
  const both = (await seededInboundIds()) || [];
  const drop = both[0];

  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const b = r && r.querySelector('button .anticon-cluster');
     if (b) b.closest('button').click();
     return true;
   })()`);
  await wait(1000);
  // Untick the inbound the account's identity came from, so the write has to move
  // its anchor AND drop the membership it was anchored on.
  await toggleMembership(drop);
  await wait(500);
  await evalJs(`(() => {
     const b = Array.from(document.querySelectorAll('#client-membership-modal .ant-modal-footer button'))
       .find(x => !/close|cancel/i.test(x.innerText));
     if (b) b.click();
     return true;
   })()`);
  await wait(3000);
  const shrunk = (await accounts()).find((a) => a.email === EMAIL);
  check('dropping an inbound leaves the account on exactly the rest',
        shrunk && shrunk.n === 1, shrunk ? 'memberships=' + shrunk.n : 'missing');

  // Put it back. That is the other direction of the same bug: ADDING an inbound
  // whose id is lower than the one the account currently lives on.
  await goto('/panel/clients');
  await inject();
  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const b = r && r.querySelector('button .anticon-cluster');
     if (b) b.closest('button').click();
     return true;
   })()`);
  await wait(1000);
  await toggleMembership(drop);
  await wait(500);
  await evalJs(`(() => {
     const b = Array.from(document.querySelectorAll('#client-membership-modal .ant-modal-footer button'))
       .find(x => !/close|cancel/i.test(x.innerText));
     if (b) b.click();
     return true;
   })()`);
  await wait(3000);
  const regrown = (await accounts()).find((a) => a.email === EMAIL);
  check('adding an inbound back succeeds rather than answering "empty client ID"',
        regrown && regrown.n === 2, regrown ? 'memberships=' + regrown.n : 'missing');

  // --------------------------------------------------------- bulk operations
  console.log('\n=== bulk operations ===');
  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const cb = r && r.querySelector('.ant-checkbox-input');
     if (cb) cb.click();
     return true;
   })()`);
  await wait(700);
  const bar = await evalJs(`(() => {
     const a = document.querySelector('.ant-alert');
     return a ? a.innerText.replace(/\\s+/g, ' ').trim() : '';
   })()`);
  check('selecting a row shows the bulk bar', /selected/i.test(bar || ''), bar);

  await evalJs(`Array.from(document.querySelectorAll('.ant-alert button'))
     .find(b => /bulk/i.test(b.innerText)).click(); true`);
  await wait(900);
  open = await evalJs('window.__openModals()');
  check('Bulk operations opens its modal',
        Array.isArray(open) && open.some((t) => /bulk/i.test(t)), JSON.stringify(open));
  // The operation is chosen on the model rather than by clicking: an antd select
  // is a listbox in a detached overlay, and driving it by click is brittle in a
  // way the write path under test is not.
  await evalJs(`app.bulkOps.op = 'addTraffic'; app.bulkOps.amount = 3; app.bulkOps.unit = 'GB'; true`);
  await wait(300);
  await evalJs(`(() => {
     const b = Array.from(document.querySelectorAll('#bulk-ops-modal .ant-modal-footer button'))
       .find(x => !/close|cancel/i.test(x.innerText));
     if (b) b.click();
     return true;
   })()`);
  await wait(3500);
  const bulked = (await accounts()).find((a) => a.email === EMAIL);
  check('a bulk add-traffic reaches the account AND the accounts layer',
        bulked && bulked.totalGB === 3 * 1073741824,
        bulked ? 'totalGB=' + bulked.totalGB : 'missing');

  // ---------------------------------------------------------------- deleting
  await goto('/panel/clients');
  await inject();
  await evalJs(`(() => {
     const r = window.__rowFor(${JSON.stringify(EMAIL)});
     const b = r && r.querySelector('button .anticon-delete');
     if (b) b.closest('button').click();
     return true;
   })()`);
  await wait(900);
  await evalJs(`(() => {
     const b = Array.from(document.querySelectorAll('.ant-modal-confirm-btns button'))
       .find(x => /delete/i.test(x.innerText));
     if (b) b.click();
     return true;
   })()`);
  await wait(3000);
  const left = await accounts();
  check('deleting removes it from every inbound', !left.some((a) => a.email === EMAIL),
        JSON.stringify(left.map((a) => a.email)));

  // --------------------------------------- identity minting, per protocol
  // Three protocol families whose identity field is NOT a uuid, each of which
  // used to answer "empty client ID". credentialsFor is checked directly rather
  // than through a create, because wg-c, awg, gre and mtproto cannot be created
  // on a panel whose cores are not installed, and the mapping is the thing under
  // test either way.
  const creds = await evalJs(`(() => {
     const f = (p) => clientMembershipModal.credentialsFor(p, 'probe@x');
     return { mtproto: f('mtproto'), wgc: f('wg-c'), awg: f('awg'), gre: f('gre'),
              hy2: f('hysteria2'), ss: f('shadowsocks'), vmess: f('vmess') };
   })()`);
  check('the email-identity protocols mint id = email, not a uuid',
        creds && ['mtproto', 'wgc', 'awg', 'gre'].every(k => creds[k] && creds[k].id === 'probe@x'),
        JSON.stringify(creds));
  check('hysteria2 mints auth and vmess mints a uuid',
        creds && creds.hy2 && creds.hy2.auth && creds.vmess && creds.vmess.id
          && creds.vmess.id !== 'probe@x',
        JSON.stringify(creds && { hy2: creds.hy2, vmess: creds.vmess }));

  // storedClient serializes through toJson(). Stringifying the instance drops a
  // class getter, and those same four protocols expose `id` as exactly that, so
  // every write built from one arrived with no identity.
  const keepsId = await evalJs(`(() => {
     const row = app.clients[0];
     if (!row || !row.memberships.length) return { none: true };
     const s = app.storedClient(row, row.memberships[0].inboundId);
     return { hasId: !!(s && s.id), email: s && s.email };
   })()`);
  check('storedClient keeps the identity field', keepsId && keepsId.hasId, JSON.stringify(keepsId));

  // ------------------------------------------------- inbounds has no clients
  console.log('\n=== the Inbounds page holds no clients ===');
  await goto('/panel/inbounds');
  const inb = await evalJs(`(() => ({
     expandIcons: document.querySelectorAll('.ant-table-row-expand-icon').length,
     clientTables: document.querySelectorAll('.bo-client-actions').length,
     rows: document.querySelectorAll('.ant-table-tbody tr').length,
   }))()`);
  check('it still lists inbounds', inb && inb.rows > 0, JSON.stringify(inb));
  check('it has no expandable client rows', inb && inb.expandIcons === 0, JSON.stringify(inb));
  check('it renders no client action table', inb && inb.clientTables === 0, JSON.stringify(inb));

  const menu = await evalJs(`(async () => {
     const t = document.querySelector('.ant-table-tbody tr .ant-dropdown-trigger');
     if (t) t.click();
     await new Promise(r => setTimeout(r, 800));
     return Array.from(document.querySelectorAll('.ant-dropdown-menu-item'))
       .map(i => (i.innerText || '').trim()).filter(Boolean);
   })()`);
  const clientish = (menu || []).filter((t) => /add client|bulk|copy client|reset client|depleted/i.test(t));
  check('and its row menu offers no client entries', clientish.length === 0,
        'offending=' + JSON.stringify(clientish));

  await teardown();

  console.log('');
  let bad = 0;
  for (const r of results) {
    console.log(`  [${r.ok ? 'PASS' : 'FAIL'}] ${r.name}${r.ok ? '' : '  <- ' + r.detail}`);
    if (!r.ok) bad++;
  }
  console.log(`\n  ${results.length - bad} passed, ${bad} failed`);
  ws.close();
  process.exit(bad ? 1 : 0);
}

main().catch((e) => {
  console.error('uitest failed to run:', e.message);
  process.exit(2);
});
