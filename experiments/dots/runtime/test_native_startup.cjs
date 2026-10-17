'use strict';

const assert = require('node:assert/strict');
const childProcess = require('node:child_process');
const {EventEmitter} = require('node:events');
const fs = require('node:fs');
const net = require('node:net');
const os = require('node:os');
const path = require('node:path');
const test = require('node:test');
const vm = require('node:vm');
const startup = require('./native_startup.cjs');

function fixture(t) {
  const directory = fs.mkdtempSync(path.join(process.platform === 'darwin' ? '/private/tmp' : os.tmpdir(), 'cqns-'));
  fs.chmodSync(directory, 0o700);
  const runtime = path.join(directory, 'runtime');
  fs.mkdirSync(runtime, {mode: 0o700});
  const filename = path.join(directory, 'settings.json');
  const config = {integration_mode: 'native', runtime_root: runtime, app_path: '/Applications/ChatGPT.app', system_auth: '/Users/example/.codex/auth.json'};
  fs.writeFileSync(filename, JSON.stringify(config), {mode: 0o600});
  t.after(() => fs.rmSync(directory, {recursive: true, force: true}));
  return {installation: directory, runtime, filename, systemHome: '/Users/example/.codex', config};
}

function commandLine(initial = {}) {
  const values = new Map(Object.entries(initial));
  return {
    values,
    hasSwitch: name => values.has(name),
    getSwitchValue: name => values.get(name),
    appendSwitch: (name, value) => values.set(name, value),
    removeSwitch: name => values.delete(name),
  };
}

function plan(context, environment = {}) {
  return {
    env: {
      CQ_DOTS_CONFIG: context.filename,
      CODEX_API_BASE_URL: 'https://localhost:12345/backend-api',
      CODEX_APP_SERVER_CHATGPT_BASE_URL: 'https://localhost:12345/backend-api',
      NODE_EXTRA_CA_CERTS: path.join(context.runtime, 'native-extra-ca.pem'),
      SSL_CERT_FILE: path.join(context.runtime, 'native-trust.pem'),
      CODEX_APP_SERVER_FORCE_CLI: '1',
      CODEX_CLI_PATH: path.join(context.runtime, 'cloud-stdio-launch'),
      NO_PROXY: [...new Set([...(environment.NO_PROXY || '').split(','), 'localhost', '127.0.0.1'].filter(Boolean))].join(','),
      PATH: environment.PATH || '/usr/bin:/bin',
      ...(environment.CODEX_HOME ? {CODEX_HOME: environment.CODEX_HOME} : {}),
    },
    switches: {
      'cq-dots-runtime': context.runtime,
      'ignore-certificate-errors-spki-list': Buffer.alloc(32, 7).toString('base64'),
      'proxy-server': 'http://127.0.0.1:23456',
      'proxy-bypass-list': 'localhost;127.0.0.1',
    },
  };
}

function electron(pages = []) {
  const app = new EventEmitter();
  app.getPath = name => { assert.equal(name, 'appData'); return '/Users/example/Library/Application Support'; };
  app.commandLine = commandLine({'user-data-dir': '/Users/example/Library/Application Support/Codex'});
  return {app, webContents: {getAllWebContents: () => pages}};
}

function page(id, url, evaluate = expression => expression) {
  return {id, getURL: () => url, isDestroyed: () => false, executeJavaScript: evaluate};
}

async function bridge(t, pages = []) {
  const context = fixture(t);
  const app = electron(pages);
  const result = startup._test.createBridge(context.runtime, app);
  await result.ready;
  t.after(result.close);
  const token = fs.readFileSync(path.join(context.runtime, 'native-debug-token'), 'utf8').trim();
  return {...context, ...result, app, token};
}

function request(root, value) {
  return new Promise((resolve, reject) => {
    const client = net.createConnection(path.join(root, 'native-debug.sock'));
    let result = '';
    client.on('connect', () => client.write(Buffer.isBuffer(value) ? value : JSON.stringify(value) + '\n'));
    client.on('data', chunk => { result += chunk.toString(); });
    client.on('end', () => { try { resolve(JSON.parse(result)); } catch (error) { reject(error); } });
    client.on('error', reject);
  });
}

function configureHarness(context, environment, app, prepare) {
  const calls = [];
  const module = {exports: {}};
  const processStub = {
    pid: process.pid, execPath: '/Applications/ChatGPT.app/Contents/MacOS/ChatGPT', argv: [], env: environment,
    getuid: () => process.getuid(), kill: process.kill.bind(process), umask: process.umask.bind(process),
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, 'native_startup.cjs'), 'utf8'), {
    require: name => name === 'electron' ? app : name === 'node:child_process' ? {spawnSync: (...args) => {
      calls.push(args);
      return prepare ? prepare() : {status: 0, signal: null, stdout: JSON.stringify(plan(context, environment))};
    }} : require(name),
    module, process: processStub, Buffer, setTimeout, clearTimeout,
  });
  return {startup: module.exports, calls};
}

test('configuration requires the exact executable and normal profile', t => {
  const context = fixture(t);
  const executable = '/Applications/ChatGPT.app/Contents/MacOS/ChatGPT';
  assert.equal(startup._test.readConfiguration(context.installation, executable, [], {}, commandLine()).runtime, context.runtime);
  for (const args of [
    [context.installation, '/Applications/Other.app/Contents/MacOS/ChatGPT', [], {}, commandLine()],
    [context.installation, executable, ['--user-data-dir=/tmp/other'], {}, commandLine()],
    [context.installation, executable, [], {CODEX_ELECTRON_USER_DATA_PATH: '/tmp/other'}, commandLine()],
    [context.installation, executable, [], {CODEX_HOME: '/tmp/other'}, commandLine()],
    [context.installation, executable, [], {}, commandLine({'remote-debugging-port': '9222'})],
  ]) assert.throws(() => startup._test.readConfiguration(...args));
  fs.chmodSync(context.filename, 0o644);
  assert.throws(() => startup._test.readConfiguration(context.installation, executable, [], {}, commandLine()), /0600/);
});

test('Electron internal normal-profile switch is allowed but explicit or alternate profiles are rejected', t => {
  const context = fixture(t);
  const executable = '/Applications/ChatGPT.app/Contents/MacOS/ChatGPT';
  const appData = '/Users/example/Library/Application Support';
  const normalProfile = path.join(appData, 'Codex');
  const internal = commandLine({'user-data-dir': normalProfile});
  assert.equal(startup._test.readConfiguration(context.installation, executable, [executable], {}, internal, appData).runtime, context.runtime);
  for (const argv of [['--user-data-dir=' + normalProfile], ['--user-data-dir', normalProfile]]) {
    assert.throws(() => startup._test.readConfiguration(context.installation, executable, argv, {}, internal, appData));
  }
  for (const profile of ['/tmp/other', normalProfile + '-other', path.join(appData, 'ChatGPT'), '']) {
    assert.throws(() => startup._test.readConfiguration(context.installation, executable, [executable], {}, commandLine({'user-data-dir': profile}), appData));
  }
  for (const name of ['remote-debugging-port', 'remote-debugging-pipe', 'remote-debugging-address']) {
    assert.throws(() => startup._test.readConfiguration(context.installation, executable, [executable], {}, commandLine({'user-data-dir': normalProfile, [name]: '1'}), appData));
  }
  const actualAppData = path.join(context.installation, 'appdata');
  fs.mkdirSync(path.join(actualAppData, 'Codex'), {recursive: true});
  const aliasAppData = path.join(context.installation, 'appdata-alias');
  fs.symlinkSync(actualAppData, aliasAppData);
  const alias = commandLine({'user-data-dir': path.join(aliasAppData, 'Codex')});
  assert.equal(startup._test.readConfiguration(context.installation, executable, [executable], {}, alias, actualAppData).runtime, context.runtime);
});

test('configuration rejects linked and non-private boundaries', t => {
  const context = fixture(t);
  fs.renameSync(context.filename, context.filename + '.real');
  fs.symlinkSync(context.filename + '.real', context.filename);
  assert.throws(() => startup._test.readConfiguration(context.installation, '', [], {}, commandLine()));
  fs.chmodSync(context.runtime, 0o755);
  assert.throws(() => startup._test.readConfiguration(context.installation, '', [], {}, commandLine()), /0700/);
});

test('preparation permits only the existing loopback plan', t => {
  const context = fixture(t);
  const environment = {PATH: '/custom/bin:/usr/bin', CODEX_HOME: context.systemHome, NO_PROXY: 'example.test,localhost'};
  assert.equal(startup._test.validatePlan(plan(context, environment), context, environment).env.PATH, environment.PATH);
  for (const mutate of [
    value => { value.env.NODE_OPTIONS = '--require=/tmp/code'; },
    value => { value.env.CODEX_API_BASE_URL = 'https://example.test/backend-api'; },
    value => { value.env.CODEX_API_BASE_URL = 'https://localhost:65536/backend-api'; },
    value => { value.env.CODEX_APP_SERVER_CHATGPT_BASE_URL += '/other'; },
    value => { value.env.CODEX_CLI_PATH = '/tmp/other'; },
    value => { value.env.PATH = '/tmp/other'; },
    value => { value.env.CODEX_HOME = '/tmp/other'; },
    value => { value.env.NO_PROXY = '*'; },
    value => { value.switches['remote-debugging-port'] = '9222'; },
    value => { value.switches['proxy-server'] = 'http://example.test:23456'; },
    value => { value.switches['proxy-bypass-list'] = '*'; },
    value => { value.switches['ignore-certificate-errors-spki-list'] = 'a'.repeat(44); },
  ]) {
    const value = plan(context, environment);
    mutate(value);
    assert.throws(() => startup._test.validatePlan(value, context, environment));
  }
});

test('partial switch failure restores the exact environment and switches', t => {
  const context = fixture(t);
  const environment = {PATH: '/usr/bin', EXISTING: 'preserved'};
  const before = {...environment};
  const switches = commandLine({'proxy-server': 'original', existing: 'value'});
  const append = switches.appendSwitch;
  let failed = false;
  switches.appendSwitch = (name, value) => {
    if (name === 'proxy-server' && !failed) { failed = true; throw new Error('append failed'); }
    return append(name, value);
  };
  assert.throws(() => startup._test.applyPlan(plan(context, environment), environment, switches), /append failed/);
  assert.deepEqual(environment, before);
  assert.deepEqual([...switches.values].sort(), [['existing', 'value'], ['proxy-server', 'original']]);
});

test('embedded identity and evaluation require the private token and exact app pid', async t => {
  const executed = [];
  const value = await bridge(t, [page(5, 'app://-/index.html', expression => { executed.push(expression); return {answer: 42}; })]);
  assert.equal(fs.statSync(path.join(value.runtime, 'native-debug.sock')).mode & 0o7777, 0o600);
  for (const name of ['native-debug.json', 'native-debug-token']) assert.equal(fs.statSync(path.join(value.runtime, name)).mode & 0o7777, 0o600);
  assert.deepEqual(await request(value.runtime, {operation: 'identity', token: value.token}), {ok: true, debug_transport: 'embedded', app_pid: process.pid, broker_pid: process.pid});
  for (const rejected of [
    {operation: 'identity'},
    {operation: 'identity', token: 'a'.repeat(64)},
    {operation: 'identity', token: value.token + 'x'},
    {operation: 'identity', token: value.token, extra: true},
    {operation: 'evaluate', token: value.token, app_pid: process.pid + 1, expression: '42'},
  ]) assert.equal((await request(value.runtime, rejected)).ok, false);
  assert.deepEqual(executed, []);
  assert.deepEqual(await request(value.runtime, {operation: 'evaluate', token: value.token, app_pid: process.pid, expression: '40+2'}), {ok: true, app_pid: process.pid, value: {answer: 42}});
  assert.deepEqual(executed, ['40+2']);
});

test('evaluation selects only the exact renderer, then the lowest web contents id', async t => {
  const executed = [];
  const value = await bridge(t, [
    page(1, 'https://chatgpt.com/', () => { throw new Error('wrong page'); }),
    page(7, 'app://-/index.html', () => { executed.push(7); }),
    page(3, 'app://-/index.html', () => { executed.push(3); return undefined; }),
    {...page(2, 'app://-/index.html'), isDestroyed: () => true},
  ]);
  const result = await request(value.runtime, {operation: 'evaluate', token: value.token, app_pid: process.pid, expression: 'undefined'});
  assert.equal(result.ok, true);
  assert.equal(result.value, null);
  assert.deepEqual(executed, [3]);
});

test('missing renderer and renderer exceptions become bounded failures', async t => {
  const value = await bridge(t, [page(1, 'app://-/index.html?other')]);
  const evaluate = () => request(value.runtime, {operation: 'evaluate', token: value.token, app_pid: process.pid, expression: '42'});
  assert.equal((await evaluate()).ok, false);
  value.app.webContents.getAllWebContents = () => { throw new Error('private renderer details'); };
  const failed = await evaluate();
  assert.equal(failed.ok, false);
  assert.equal(JSON.stringify(failed).includes('private renderer details'), false);
});

test('oversized requests and results are rejected', async t => {
  const value = await bridge(t, [page(1, 'app://-/index.html', () => 'x'.repeat(startup._test.MAX_RESPONSE))]);
  assert.equal((await request(value.runtime, Buffer.alloc(startup._test.MAX_REQUEST + 1, 120))).ok, false);
  const result = await request(value.runtime, {operation: 'evaluate', token: value.token, app_pid: process.pid, expression: 'large'});
  assert.equal(result.ok, false);
  assert.match(result.error, /too large/);
});

test('the bridge admits at most eight simultaneous clients', async t => {
  const value = await bridge(t);
  const clients = [];
  t.after(() => clients.forEach(client => client.destroy()));
  for (let index = 0; index < 8; index++) {
    const client = net.createConnection(path.join(value.runtime, 'native-debug.sock'));
    clients.push(client);
    await new Promise((resolve, reject) => { client.once('connect', resolve); client.once('error', reject); });
  }
  const extra = net.createConnection(path.join(value.runtime, 'native-debug.sock'));
  clients.push(extra);
  await new Promise((resolve, reject) => { extra.once('close', resolve); extra.once('error', reject); });
  assert.equal(extra.destroyed, true);
  assert.equal(clients.slice(0, 8).some(client => client.destroyed), false);
});

test('live endpoints are not overwritten and app quit removes only owned files', async t => {
  const value = await bridge(t);
  assert.throws(() => startup._test.createBridge(value.runtime, electron()), /already running/);
  value.app.app.emit('will-quit');
  for (const name of ['native-debug.sock', 'native-debug.json', 'native-debug-token']) assert.equal(fs.existsSync(path.join(value.runtime, name)), false);
});

test('cleanup preserves replacement metadata and token', async t => {
  const value = await bridge(t);
  const replacement = {debug_transport: 'embedded', app_pid: process.pid + 1, broker_pid: process.pid + 1};
  fs.writeFileSync(path.join(value.runtime, 'native-debug.json'), JSON.stringify(replacement));
  fs.writeFileSync(path.join(value.runtime, 'native-debug-token'), 'b'.repeat(64));
  value.close();
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(value.runtime, 'native-debug.json'))), replacement);
  assert.equal(fs.readFileSync(path.join(value.runtime, 'native-debug-token'), 'utf8'), 'b'.repeat(64));
});

test('cleanup preserves another socket published at the well-known path', async t => {
  const value = await bridge(t);
  const socket = path.join(value.runtime, 'native-debug.sock');
  fs.unlinkSync(socket);
  const replacement = net.createServer();
  await new Promise(resolve => replacement.listen(socket, resolve));
  t.after(() => replacement.close());
  const inode = fs.statSync(socket).ino;
  value.close();
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(fs.statSync(socket).ino, inode);
});

test('only a private socket with a dead recorded owner can be removed as stale', t => {
  const context = fixture(t);
  const socket = path.join(context.runtime, 'native-debug.sock');
  const child = childProcess.spawnSync(process.execPath, ['-e',
    "require('net').createServer().listen(process.argv[1],()=>{require('fs').chmodSync(process.argv[1],0o600);process.stdout.write(String(process.pid));process.exit(0);});", socket], {encoding: 'utf8'});
  assert.equal(child.status, 0, child.stderr);
  assert.throws(() => startup._test.removeStaleEndpoint(context.runtime), /unverified/);
  assert.equal(fs.existsSync(socket), true);
  const pid = Number(child.stdout);
  fs.writeFileSync(path.join(context.runtime, 'native-debug.json'), JSON.stringify({debug_transport: 'embedded', app_pid: pid, broker_pid: pid}), {mode: 0o600});
  startup._test.removeStaleEndpoint(context.runtime);
  assert.equal(fs.existsSync(socket), false);
});

test('failed preparation leaves the original startup environment untouched', t => {
  const context = fixture(t);
  for (const prepare of [
    () => ({status: 1, signal: null, stdout: ''}),
    () => ({status: null, signal: 'SIGTERM', stdout: ''}),
    () => ({status: 0, signal: null, stdout: JSON.stringify({env: {NODE_OPTIONS: 'untrusted'}, switches: {}})}),
  ]) {
    const app = electron();
    const environment = {PATH: '/usr/bin', PRESERVED: 'yes'};
    const before = {...environment};
    const harness = configureHarness(context, environment, app, prepare);
    assert.throws(() => harness.startup.configure(context.installation));
    assert.deepEqual(environment, before);
    assert.deepEqual([...app.app.commandLine.values], [['user-data-dir', '/Users/example/Library/Application Support/Codex']]);
    assert.equal(fs.existsSync(path.join(context.runtime, 'native-debug.sock')), false);
  }
});

test('full synchronous configure validates preparation before applying it', async t => {
  const context = fixture(t);
  const app = electron();
  const environment = {PATH: '/usr/bin:/bin', PYTHONPATH: '/untrusted', PYTHONHOME: '/untrusted', PYTHONSTARTUP: '/untrusted', PYTHONINSPECT: '1'};
  const harness = configureHarness(context, environment, app);
  const configured = harness.startup.configure(context.installation);
  await configured.ready;
  t.after(configured.close);
  const {calls} = harness;
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], path.join(context.runtime, 'venv/bin/python'));
  assert.equal(calls[0][1].join('|'), [path.join(context.runtime, 'native_bootstrap.py'), '--parent-pid', String(process.pid)].join('|'));
  assert.equal(calls[0][2].timeout, 20000);
  assert.equal(calls[0][2].maxBuffer, 65536);
  assert.equal(calls[0][2].env.CQ_DOTS_CONFIG, context.filename);
  for (const name of ['PYTHONPATH', 'PYTHONHOME', 'PYTHONSTARTUP', 'PYTHONINSPECT']) assert.equal(name in calls[0][2].env, false);
  assert.equal(calls[0][2].env.PYTHONNOUSERSITE, '1');
  assert.equal(calls[0][2].env.PYTHONDONTWRITEBYTECODE, '1');
  assert.equal(environment.CODEX_CLI_PATH, path.join(context.runtime, 'cloud-stdio-launch'));
  assert.equal(app.app.commandLine.values.has('remote-debugging-port'), false);
  assert.equal(harness.startup.configure(context.installation), configured);
});
