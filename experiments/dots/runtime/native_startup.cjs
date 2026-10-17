'use strict';

// Loaded before the desktop app's original early bootstrap. No debugger port
// or replacement app process is needed: only the existing renderer is exposed
// through an authenticated socket inside CQ's private runtime directory.
const fs = require('node:fs');
const path = require('node:path');
const net = require('node:net');
const crypto = require('node:crypto');
const childProcess = require('node:child_process');

const MAX_REQUEST = 1024 * 1024;
const MAX_RESPONSE = 2 * 1024 * 1024;
const TIMEOUT = 35000;
const SOCKET = 'native-debug.sock';
const STATE = 'native-debug.json';
const TOKEN = 'native-debug-token';
let configured;

function absolute(value) {
  if (typeof value !== 'string' || !path.isAbsolute(value) || value.includes('\0') ||
      value.split(path.sep).includes('..') || path.resolve(value) !== value) {
    throw new Error('Native startup path must be absolute and normalised');
  }
  return value;
}

function privateDirectory(directory) {
  const info = fs.lstatSync(directory);
  if (!info.isDirectory() || info.uid !== process.getuid() || (info.mode & 0o7777) !== 0o700) {
    throw new Error('Native startup directory must be owned and mode 0700');
  }
}

function privateFile(filename, limit) {
  const fd = fs.openSync(filename, fs.constants.O_RDONLY | fs.constants.O_NOFOLLOW | fs.constants.O_NONBLOCK);
  try {
    const before = fs.fstatSync(fd);
    if (!before.isFile() || before.uid !== process.getuid() || (before.mode & 0o7777) !== 0o600 || before.size > limit) {
      throw new Error('Native startup file must be owned regular mode 0600');
    }
    const buffer = Buffer.alloc(limit + 1);
    const count = fs.readSync(fd, buffer, 0, buffer.length, 0);
    const after = fs.fstatSync(fd);
    if (count > limit || ['dev', 'ino', 'size', 'mtimeMs', 'ctimeMs'].some(key => before[key] !== after[key])) {
      throw new Error('Native startup file changed during read');
    }
    return buffer.subarray(0, count).toString('utf8');
  } finally {
    fs.closeSync(fd);
  }
}

function atomicPrivate(root, name, content) {
  const target = path.join(root, name);
  try { privateFile(target, MAX_RESPONSE); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  const temporary = target + '.' + process.pid + '.' + crypto.randomBytes(8).toString('hex');
  const fd = fs.openSync(temporary, fs.constants.O_WRONLY | fs.constants.O_CREAT | fs.constants.O_EXCL | fs.constants.O_NOFOLLOW, 0o600);
  try {
    try { fs.writeFileSync(fd, content); fs.fsyncSync(fd); } finally { fs.closeSync(fd); }
    fs.renameSync(temporary, target);
  } finally {
    try { fs.unlinkSync(temporary); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
}

function sameProfile(actual, expected) {
  if (actual === expected) return true;
  try { return fs.realpathSync(absolute(actual)) === fs.realpathSync(expected); } catch (_) { return false; }
}

function readConfiguration(installationRoot, executable, argv, environment, commandLine, appData) {
  const installation = absolute(installationRoot);
  privateDirectory(installation);
  const runtime = path.join(installation, 'runtime');
  privateDirectory(runtime);
  const filename = path.join(installation, 'settings.json');
  const config = JSON.parse(privateFile(filename, MAX_REQUEST));
  if (!config || Array.isArray(config) || config.integration_mode !== 'native' || config.runtime_root !== runtime) {
    throw new Error('Native startup requires native settings and the installation runtime');
  }
  const app = absolute(config.app_path);
  if (executable !== path.join(app, 'Contents', 'MacOS', 'ChatGPT')) {
    throw new Error('Native startup executable identity mismatch');
  }
  const systemHome = path.dirname(absolute(config.system_auth));
  // Electron adds its selected profile to the internal command line before
  // JavaScript runs. The pinned app's normal profile is appData/Codex; only an
  // explicit argv override or a different internal profile is forbidden.
  const alternateProfile = commandLine.hasSwitch('user-data-dir') &&
    !sameProfile(commandLine.getSwitchValue('user-data-dir'), path.join(absolute(appData), 'Codex'));
  if ('CODEX_ELECTRON_USER_DATA_PATH' in environment ||
      ('CODEX_HOME' in environment && environment.CODEX_HOME !== systemHome) ||
      argv.some(value => /^--(?:user-data-dir|remote-debugging-(?:port|pipe|address))(?:=|$)/.test(value)) ||
      alternateProfile || ['remote-debugging-port', 'remote-debugging-pipe', 'remote-debugging-address'].some(name => commandLine.hasSwitch(name))) {
    throw new Error('Native startup requires the normal desktop profile without a debugger');
  }
  return {installation, runtime, filename, systemHome};
}

function validPort(value) {
  return /^[1-9][0-9]{0,4}$/.test(value) && Number(value) <= 65535;
}

function validatePlan(plan, context, environment) {
  if (!plan || Array.isArray(plan) || Object.keys(plan).sort().join(',') !== 'env,switches' ||
      !plan.env || Array.isArray(plan.env) || typeof plan.env !== 'object' ||
      !plan.switches || Array.isArray(plan.switches) || typeof plan.switches !== 'object') {
    throw new Error('Native startup preparation returned an invalid plan');
  }
  const required = {
    CQ_DOTS_CONFIG: context.filename,
    NODE_EXTRA_CA_CERTS: path.join(context.runtime, 'native-extra-ca.pem'),
    SSL_CERT_FILE: path.join(context.runtime, 'native-trust.pem'),
    CODEX_APP_SERVER_FORCE_CLI: '1',
    CODEX_CLI_PATH: path.join(context.runtime, 'cloud-stdio-launch'),
    NO_PROXY: [...new Set([...(environment.NO_PROXY || '').split(','), 'localhost', '127.0.0.1'].filter(Boolean))].join(','),
  };
  const allowed = new Set([...Object.keys(required), 'CODEX_API_BASE_URL', 'CODEX_APP_SERVER_CHATGPT_BASE_URL', 'PATH', 'CODEX_HOME']);
  for (const [name, value] of Object.entries(plan.env)) {
    if (!allowed.has(name) || typeof value !== 'string' || value.length > 16384 || /[\x00-\x1f]/.test(value)) {
      throw new Error('Native startup preparation returned an unsupported environment');
    }
  }
  if (Object.entries(required).some(([name, value]) => plan.env[name] !== value)) {
    throw new Error('Native startup preparation changed a required environment value');
  }
  const base = /^https:\/\/localhost:([0-9]+)\/backend-api$/.exec(plan.env.CODEX_API_BASE_URL || '');
  if (!base || !validPort(base[1]) || plan.env.CODEX_APP_SERVER_CHATGPT_BASE_URL !== plan.env.CODEX_API_BASE_URL ||
      ('PATH' in plan.env && plan.env.PATH !== (environment.PATH || '/usr/bin:/bin')) ||
      ('CODEX_HOME' in plan.env && (plan.env.CODEX_HOME !== context.systemHome || plan.env.CODEX_HOME !== environment.CODEX_HOME))) {
    throw new Error('Native startup preparation changed a protected environment value');
  }
  const switches = plan.switches;
  if (Object.keys(switches).sort().join(',') !== 'cq-dots-runtime,ignore-certificate-errors-spki-list,proxy-bypass-list,proxy-server' ||
      switches['cq-dots-runtime'] !== context.runtime || switches['proxy-bypass-list'] !== 'localhost;127.0.0.1') {
    throw new Error('Native startup preparation returned unsupported switches');
  }
  const proxy = /^http:\/\/127\.0\.0\.1:([0-9]+)$/.exec(switches['proxy-server'] || '');
  const spki = switches['ignore-certificate-errors-spki-list'];
  if (!proxy || !validPort(proxy[1]) || typeof spki !== 'string' || !/^[A-Za-z0-9+/]{43}=$/.test(spki) ||
      Buffer.from(spki, 'base64').length !== 32 || Buffer.from(spki, 'base64').toString('base64') !== spki) {
    throw new Error('Native startup preparation requires loopback proxy and one certificate');
  }
  return plan;
}

function applyPlan(plan, environment, commandLine) {
  const oldEnvironment = new Map(Object.keys(plan.env).map(name => [name, environment[name]]));
  const oldSwitches = new Map(Object.keys(plan.switches).map(name => [name, commandLine.hasSwitch(name) ? commandLine.getSwitchValue(name) : undefined]));
  const restore = () => {
    for (const [name, value] of oldEnvironment) {
      if (value === undefined) delete environment[name]; else environment[name] = value;
    }
    for (const [name, value] of oldSwitches) {
      commandLine.removeSwitch(name);
      if (value !== undefined) commandLine.appendSwitch(name, value);
    }
  };
  try {
    Object.assign(environment, plan.env);
    for (const [name, value] of Object.entries(plan.switches)) commandLine.appendSwitch(name, value);
  } catch (error) {
    restore();
    throw error;
  }
  return restore;
}

function pidAlive(pid) {
  try { process.kill(pid, 0); return true; } catch (error) { if (error.code === 'ESRCH') return false; throw error; }
}

function removeStaleEndpoint(root) {
  let state;
  try { state = JSON.parse(privateFile(path.join(root, STATE), 4096)); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (state) {
    if (!Number.isSafeInteger(state.app_pid) || state.app_pid <= 0 || !Number.isSafeInteger(state.broker_pid) || state.broker_pid <= 0) {
      throw new Error('Native startup bridge identity is invalid');
    }
    if (pidAlive(state.app_pid) || pidAlive(state.broker_pid)) throw new Error('Native startup bridge is already running');
  }
  let socket;
  try { socket = fs.lstatSync(path.join(root, SOCKET)); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (socket) {
    if (!state || !socket.isSocket() || socket.uid !== process.getuid() || (socket.mode & 0o7777) !== 0o600) {
      throw new Error('Native startup cannot replace an unverified socket');
    }
    fs.unlinkSync(path.join(root, SOCKET));
  }
  try { privateFile(path.join(root, TOKEN), 128); } catch (error) { if (error.code !== 'ENOENT') throw error; }
}

function createBridge(root, electron, appPid = process.pid) {
  privateDirectory(root);
  if (!Number.isSafeInteger(appPid) || appPid <= 0 || Buffer.byteLength(path.join(root, SOCKET)) > 103) {
    throw new Error('Native startup bridge identity or socket path is invalid');
  }
  removeStaleEndpoint(root);
  const token = crypto.randomBytes(32).toString('hex');
  // libuv unlinks its bound pathname on close, even if that path was replaced.
  // Bind a unique owned name and publish a hard link so cleanup cannot remove
  // a subsequent instance's well-known endpoint.
  const boundSocket = path.join(root, crypto.randomBytes(5).toString('hex') + '.sock');
  const identity = {debug_transport: 'embedded', app_pid: appPid, broker_pid: appPid};
  const clients = new Set();
  let inode;
  let closed = false;
  const reply = (client, value) => {
    let encoded;
    try { encoded = JSON.stringify(value) + '\n'; } catch (_) { encoded = '{"ok":false,"error":"Native bridge result is not serialisable"}\n'; }
    if (Buffer.byteLength(encoded) > MAX_RESPONSE) encoded = '{"ok":false,"error":"Native bridge result is too large"}\n';
    client.end(encoded);
  };
  const server = net.createServer(client => {
    if (closed || clients.size >= 8) { client.destroy(); return; }
    clients.add(client);
    const deadline = setTimeout(() => client.destroy(), TIMEOUT);
    client.on('close', () => { clients.delete(client); clearTimeout(deadline); });
    client.on('error', () => {});
    let buffer = Buffer.alloc(0);
    let accepted = false;
    client.on('data', chunk => {
      if (accepted) return;
      buffer = Buffer.concat([buffer, chunk]);
      if (buffer.length > MAX_REQUEST) { accepted = true; reply(client, {ok: false, error: 'Native bridge request is too large'}); return; }
      const newline = buffer.indexOf(10);
      if (newline < 0) return;
      accepted = true;
      let request;
      try {
        if (buffer.subarray(newline + 1).length) throw new Error();
        request = JSON.parse(buffer.subarray(0, newline).toString('utf8'));
        if (!request || Array.isArray(request) || typeof request.token !== 'string' || !/^[0-9a-f]{64}$/.test(request.token) ||
            !crypto.timingSafeEqual(Buffer.from(request.token, 'hex'), Buffer.from(token, 'hex'))) throw new Error();
        if (request.operation === 'identity' && Object.keys(request).sort().join(',') === 'operation,token') {
          reply(client, {ok: true, ...identity});
          return;
        }
        if (Object.keys(request).sort().join(',') !== 'app_pid,expression,operation,token' || request.operation !== 'evaluate' ||
            request.app_pid !== appPid || !Number.isSafeInteger(request.app_pid) || typeof request.expression !== 'string' || !request.expression) throw new Error();
      } catch (_) { reply(client, {ok: false, error: 'Native bridge request rejected'}); return; }
      Promise.resolve().then(() => {
        const pages = electron.webContents.getAllWebContents().filter(page => !page.isDestroyed() && page.getURL() === 'app://-/index.html').sort((a, b) => a.id - b.id);
        if (!pages.length) throw new Error('Native renderer unavailable');
        return pages[0].executeJavaScript(request.expression);
      }).then(value => {
        if (!client.destroyed) reply(client, {ok: true, app_pid: appPid, value: value === undefined ? null : value});
      }, () => { if (!client.destroyed) reply(client, {ok: false, error: 'Native renderer evaluation failed'}); });
    });
  });
  const cleanup = () => {
    if (closed) return;
    closed = true;
    for (const client of clients) client.destroy();
    try { server.close(); } catch (_) {}
    try {
      const info = fs.lstatSync(path.join(root, SOCKET));
      if (inode !== undefined && info.ino === inode) fs.unlinkSync(path.join(root, SOCKET));
    } catch (_) {}
    try {
      const current = JSON.parse(privateFile(path.join(root, STATE), 4096));
      if (current.debug_transport === 'embedded' && current.app_pid === appPid &&
          privateFile(path.join(root, TOKEN), 128).trim() === token) {
        fs.unlinkSync(path.join(root, STATE));
        fs.unlinkSync(path.join(root, TOKEN));
      }
    } catch (_) {}
  };
  const ready = new Promise((resolve, reject) => {
    server.once('error', error => { cleanup(); reject(error); });
    const mask = process.umask(0o077);
    try {
      server.listen(boundSocket, () => {
        try {
          const socket = fs.lstatSync(boundSocket);
          inode = socket.ino;
          if (!socket.isSocket() || socket.uid !== process.getuid()) throw new Error('Native bridge socket identity changed');
          fs.chmodSync(boundSocket, 0o600);
          fs.linkSync(boundSocket, path.join(root, SOCKET));
          atomicPrivate(root, TOKEN, token + '\n');
          atomicPrivate(root, STATE, JSON.stringify(identity) + '\n');
          resolve(identity);
        } catch (error) { cleanup(); reject(error); }
      });
    } catch (error) { cleanup(); reject(error); } finally { process.umask(mask); }
  });
  electron.app.once('will-quit', cleanup);
  return {ready, close: cleanup};
}

function configure(installationRoot) {
  if (configured) return configured;
  const electron = require('electron');
  const context = readConfiguration(installationRoot, process.execPath, process.argv, process.env,
    electron.app.commandLine, electron.app.getPath('appData'));
  removeStaleEndpoint(context.runtime);
  const childEnvironment = {...process.env, CQ_DOTS_CONFIG: context.filename, PYTHONNOUSERSITE: '1', PYTHONDONTWRITEBYTECODE: '1'};
  for (const name of ['PYTHONPATH', 'PYTHONHOME', 'PYTHONSTARTUP', 'PYTHONINSPECT']) delete childEnvironment[name];
  const result = childProcess.spawnSync(path.join(context.runtime, 'venv', 'bin', 'python'),
    [path.join(context.runtime, 'native_bootstrap.py'), '--parent-pid', String(process.pid)],
    {env: childEnvironment, encoding: 'utf8', timeout: 20000, maxBuffer: 65536, windowsHide: true});
  if (result.error || result.status !== 0 || result.signal) throw new Error('Native startup preparation failed');
  const plan = validatePlan(JSON.parse(result.stdout), context, process.env);
  const restore = applyPlan(plan, process.env, electron.app.commandLine);
  try {
    configured = createBridge(context.runtime, electron);
    configured.ready.catch(() => { restore(); configured = undefined; });
    return configured;
  } catch (error) {
    restore();
    throw error;
  }
}

module.exports = {configure, _test: {readConfiguration, validatePlan, applyPlan, createBridge, removeStaleEndpoint, privateFile, MAX_REQUEST, MAX_RESPONSE}};
