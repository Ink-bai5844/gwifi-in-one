#!/usr/bin/env node
'use strict';

// Keep subscription data, credentials and API responses out of terminal output.
const fs = require('node:fs');
const path = require('node:path');
const crypto = require('node:crypto');
const http = require('node:http');
const vm = require('node:vm');
const { execFileSync } = require('node:child_process');

const ROOT = path.resolve(__dirname, '..');
const MARKER = '// GWIFI-SYSTEM-INSTALL v1';
const GROUP = 'GWIFI-DOWNLOAD';
const NAMES = ['GWIFI-Wired-A', 'GWIFI-Wired-B', 'GWIFI-WiFi', 'GWIFI-Dual', GROUP];
const MODES = { dual: 'GWIFI-Dual', wired: 'GWIFI-Wired-A', wifi: 'GWIFI-WiFi', off: 'DIRECT' };
const ALLOWED_SELECTIONS = new Set([...NAMES, 'DIRECT']);

class SafeError extends Error {}
const fail = message => { throw new SafeError(message); };
const hash = value => crypto.createHash('sha256').update(value).digest('hex');
const clone = value => JSON.parse(JSON.stringify(value));
const samePath = (a, b) => path.resolve(a).toLowerCase() === path.resolve(b).toLowerCase();

function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(',')}]`;
  if (value && typeof value === 'object') {
    return `{${Object.keys(value).sort().map(key => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(',')}}`;
  }
  return JSON.stringify(value);
}

function parseArgs(argv) {
  const args = [...argv];
  const command = args.shift() || 'help';
  const options = {
    command,
    profileDir: path.join(process.env.APPDATA || '', 'io.github.clash-verge-rev.clash-verge-rev'),
    core: 'C:/ProgramData/clash-verge-service/cores/verge-mihomo.exe',
    python: process.env.GWIFI_PYTHON || 'python',
  };
  if (command === 'mode') options.mode = args.shift();
  while (args.length) {
    const key = args.shift();
    if (!['--profile-dir', '--python', '--core'].includes(key) || !args.length) {
      fail('参数无效。运行 node scripts/gwifi-system.cjs help 查看用法。');
    }
    options[{ '--profile-dir': 'profileDir', '--python': 'python', '--core': 'core' }[key]] = args.shift();
  }
  if (!['prepare', 'install', 'status', 'mode', 'uninstall', 'help', '--help', '-h'].includes(command)) {
    fail('未知命令。运行 node scripts/gwifi-system.cjs help 查看用法。');
  }
  if (command === 'mode' && !Object.hasOwn(MODES, options.mode)) fail('mode 必须是 dual、wired、wifi 或 off。');
  return options;
}

function readFile(file, message) {
  try { return fs.readFileSync(file); } catch { fail(message); }
}

function readYaml(file, python) {
  // A path argument, never source interpolation: no YAML or credentials enter a shell.
  const code = 'import json,sys,yaml; f=open(sys.argv[1],encoding="utf-8-sig"); d=yaml.safe_load(f); f.close(); print(json.dumps(d,ensure_ascii=True))';
  try {
    const out = execFileSync(python, ['-c', code, file], {
      windowsHide: true, timeout: 30000, maxBuffer: 32 * 1024 * 1024,
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    const data = JSON.parse(out.toString('utf8'));
    if (!data || typeof data !== 'object' || Array.isArray(data)) throw new Error();
    return data;
  } catch { fail('无法解析本地 YAML；请确认 Python 已安装 PyYAML。配置内容和解析器输出已隐藏。'); }
}

function discoverCore() {
  // The service overrides the YAML pipe and configuration path on its command line.
  // Return only the three allowlisted arguments; never expose the command line itself.
  const ps = `$ErrorActionPreference='Stop'
[Console]::OutputEncoding=[System.Text.UTF8Encoding]::new($false)
$items=@(Get-CimInstance Win32_Process -Filter "Name='verge-mihomo.exe'" | ForEach-Object {
  $argsMap=@{}
  foreach ($m in [regex]::Matches($_.CommandLine, '(?i)(?:^|\\s)(-f|-d|-ext-ctl-pipe)(?:\\s+|=)(?:"([^"]*)"|([^\\s]+))')) {
    $v=$m.Groups[2].Value
    if (-not $v) { $v=$m.Groups[3].Value }
    $argsMap[$m.Groups[1].Value.ToLowerInvariant()]=$v
  }
  [pscustomobject]@{config=$argsMap['-f'];directory=$argsMap['-d'];pipe=$argsMap['-ext-ctl-pipe']}
})
ConvertTo-Json -InputObject $items -Compress`;
  try {
    const out = execFileSync('powershell.exe', ['-NoProfile', '-NonInteractive', '-Command', ps], {
      windowsHide: true, timeout: 15000, maxBuffer: 65536, stdio: ['ignore', 'pipe', 'pipe'],
    });
    const items = JSON.parse(out.toString('utf8').replace(/^\uFEFF/, ''));
    if (!Array.isArray(items) || items.length !== 1 || !items[0].config) throw new Error();
    return items[0];
  } catch { fail('无法确定唯一的运行中 Mihomo 服务及其配置文件；请先启动 Clash Verge。未更改任何网络设置。'); }
}

function readState(stateFile) {
  if (!fs.existsSync(stateFile)) return null;
  try {
    const state = JSON.parse(fs.readFileSync(stateFile, 'utf8'));
    if (state.version !== 1) throw new Error();
    return state;
  } catch { fail('安装状态文件无法读取；保留现有脚本和备份，请手动检查 gwifi-backup/state.json。'); }
}

function loadContext(options) {
  const profileDir = path.resolve(options.profileDir);
  const scriptFile = path.join(profileDir, 'profiles', 'Script.js');
  const profilesFile = path.join(profileDir, 'profiles.yaml');
  const backupRoot = path.join(profileDir, 'gwifi-backup');
  const stateFile = path.join(backupRoot, 'state.json');
  const core = discoverCore();
  const runtimeFile = path.resolve(core.config);
  const runtimeBytes = readFile(runtimeFile, '无法读取运行中的 Mihomo 配置文件。');
  const base = readYaml(runtimeFile, options.python);
  const profiles = readYaml(profilesFile, options.python);
  if (typeof profiles.current !== 'string' || !profiles.current) fail('无法确定当前订阅配置；未修改设置。');
  const pipe = core.pipe || base['external-controller-pipe'];
  if (typeof pipe !== 'string' || !pipe.startsWith('\\\\.\\pipe\\')) fail('没有可用的 Mihomo 本地命名管道；未修改设置。');
  return {
    ...options, profileDir, scriptFile, profilesFile, backupRoot, stateFile,
    runtimeFile, runtimeBytes, base, pipe, profile: profiles.current,
    scriptBytes: readFile(scriptFile, '无法读取 Clash Verge 全局扩展脚本。'),
    state: readState(stateFile),
  };
}

function identityScript(source) {
  const clean = source.replace(/^\uFEFF/, '').replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/[^\r\n]*/g, '').trim();
  return /^function\s+main\s*\(\s*([A-Za-z_$][\w$]*)\s*(?:,\s*[A-Za-z_$][\w$]*\s*)?\)\s*\{\s*return\s+\1\s*;?\s*\}\s*;?$/.test(clean);
}

function verifiedBackup(ctx, kind) {
  const state = ctx.state;
  const file = path.resolve(ctx.backupRoot, state.backupDir, kind === 'script' ? 'Script.js' : 'runtime.yaml');
  if (!file.startsWith(path.resolve(ctx.backupRoot) + path.sep)) fail('备份路径无效。');
  const bytes = readFile(file, '安装备份缺失；未修改当前设置。');
  if (hash(bytes) !== state[kind === 'script' ? 'originalScriptHash' : 'originalRuntimeHash']) fail('安装备份内容已变化；未修改当前设置。');
  return { file, bytes };
}

function checkInstalled(ctx) {
  if (!ctx.state || !ctx.state.active) fail('尚未安装 Gwifi 全局分流。');
  if (ctx.state.profile !== ctx.profile) fail('当前订阅配置与安装时不同；请先切回安装时的配置，再切换或卸载。');
  if (!samePath(ctx.state.profileDir, ctx.profileDir) || !samePath(ctx.state.runtimeFile, ctx.runtimeFile)) {
    fail('配置目录或运行时配置位置已改变；未修改当前设置。');
  }
  if (hash(ctx.scriptBytes) !== ctx.state.installedScriptHash) fail('全局脚本在安装后被编辑过；为保留这些修改，已停止操作。');
}

function originalScript(ctx) {
  const source = ctx.scriptBytes.toString('utf8');
  if (source.startsWith(MARKER)) {
    checkInstalled(ctx);
    return verifiedBackup(ctx, 'script').bytes;
  }
  if (ctx.state && ctx.state.active) fail('安装状态与全局脚本不一致；未覆盖当前脚本。');
  if (!identityScript(source)) fail('现有全局脚本包含自定义逻辑；自动安装只接受空 main，未覆盖该脚本。');
  return ctx.scriptBytes;
}

function makeWrapper(original, overlay) {
  return `${MARKER}\n// Managed by gwifi-in-one; use gwifi-system.cjs uninstall to restore the original.\nfunction main(config, profileName) {\n  const originalMain = (function () {\n${original}\n    return main;\n  })();\n  const overlayMain = (function () {\n${overlay}\n    return main;\n  })();\n  return overlayMain(originalMain(config, profileName), profileName);\n}\n`;
}

function applyScript(source, base) {
  try {
    const sandbox = { config: clone(base) };
    vm.runInNewContext(`${source}\nresult = main(config);`, sandbox, { timeout: 2000 });
    const output = clone(sandbox.result);
    if (!output || !Array.isArray(output['proxy-groups']) || !Array.isArray(output.rules)) throw new Error();
    return output;
  } catch { fail('Gwifi 分流脚本生成配置失败。原配置和脚本均未改动。'); }
}

function configurationForAPI(ctx, config) {
  const output = clone(config);
  // Preserve the actual service pipe even when the runtime YAML contains a stale one.
  output['external-controller-pipe'] = ctx.pipe;
  return output;
}

function writeAtomic(file, contents) {
  const temporary = `${file}.gwifi-${crypto.randomUUID()}.tmp`;
  try {
    fs.writeFileSync(temporary, contents, { flag: 'wx' });
    fs.renameSync(temporary, file);
  } finally {
    try { fs.unlinkSync(temporary); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
}

function saveState(ctx, state) {
  writeAtomic(ctx.stateFile, JSON.stringify(state, null, 2) + '\n');
  ctx.state = state;
}

function assertUnchanged(ctx, expectedScriptHash = hash(ctx.scriptBytes)) {
  if (hash(readFile(ctx.scriptFile, '全局脚本已无法读取。')) !== expectedScriptHash) fail('操作期间全局脚本被修改；已停止，未覆盖用户编辑。');
  const current = readYaml(ctx.profilesFile, ctx.python).current;
  if (current !== ctx.profile) fail('操作期间订阅配置发生切换；已停止操作。');
  if (hash(readFile(ctx.runtimeFile, '运行时配置已无法读取。')) !== hash(ctx.runtimeBytes)) fail('操作期间运行时配置已变化；请重试。');
}

function prepare(options, silent = false) {
  const ctx = loadContext(options);
  const original = originalScript(ctx);
  const overlay = readFile(path.join(__dirname, 'gwifi-mihomo.js'), '缺少 scripts/gwifi-mihomo.js。').toString('utf8');
  const wrapper = makeWrapper(original.toString('utf8'), overlay);
  const candidate = configurationForAPI(ctx, applyScript(wrapper, ctx.base));
  const names = new Set([...(candidate.proxies || []), ...candidate['proxy-groups']].map(item => item.name));
  if (NAMES.some(name => !names.has(name))) fail('分流脚本未生成完整的 Gwifi 出口和分组。');
  const cacheDir = path.join(ROOT, '.cache', 'gwifi-system');
  fs.mkdirSync(cacheDir, { recursive: true });
  const candidateFile = path.join(cacheDir, `candidate-${crypto.randomUUID()}.json`);
  try {
    fs.writeFileSync(candidateFile, JSON.stringify(candidate), { flag: 'wx' });
    try {
      execFileSync(ctx.core, ['-t', '-f', candidateFile, '-d', ctx.profileDir], {
        windowsHide: true, timeout: 60000, maxBuffer: 4 * 1024 * 1024,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
    } catch { fail('Mihomo 配置校验未通过；未安装。为防止泄露订阅信息，内核原始日志已隐藏。'); }
  } finally {
    try { fs.unlinkSync(candidateFile); } catch (error) { if (error.code !== 'ENOENT') throw error; }
  }
  assertUnchanged(ctx);
  const plan = {
    version: 1, preparedAt: new Date().toISOString(), profileDir: ctx.profileDir,
    runtimeFile: ctx.runtimeFile, profile: ctx.profile, originalScriptHash: hash(original),
    observedScriptHash: hash(ctx.scriptBytes), installedScriptHash: hash(wrapper),
    originalRuntimeHash: hash(ctx.runtimeBytes), groups: NAMES, initialMode: 'wired',
    originalRuleCount: (ctx.base.rules || []).length, candidateRuleCount: candidate.rules.length,
    coreValidationPassed: true,
  };
  writeAtomic(path.join(cacheDir, 'plan.json'), JSON.stringify(plan, null, 2) + '\n');
  if (!silent) {
    console.log('准备完成：现有 Mihomo 内核配置校验通过。');
    console.log('新增有线/无线直连出口、双路轮询组和总开关；安装初始模式为 wired。');
    console.log('此命令未修改 Clash Verge 脚本或运行中的网络配置。');
    console.log(`不含凭据的准备记录：${path.join(cacheDir, 'plan.json')}`);
  }
  return { ...ctx, original, wrapper, candidate, plan };
}

function api(ctx, method, route, body) {
  return new Promise((resolve, reject) => {
    const data = body === undefined ? null : JSON.stringify(body);
    const headers = { Accept: 'application/json', Connection: 'close' };
    if (data !== null) {
      headers['Content-Type'] = 'application/json';
      headers['Content-Length'] = Buffer.byteLength(data);
    }
    if (ctx.base.secret) headers.Authorization = `Bearer ${ctx.base.secret}`;
    const request = http.request({ socketPath: ctx.pipe, path: route, method, headers, agent: false }, response => {
      let size = 0;
      const chunks = [];
      response.on('data', chunk => {
        size += chunk.length;
        if (size > 16 * 1024 * 1024) response.destroy(new Error('size'));
        else chunks.push(chunk);
      });
      response.on('error', () => reject(new SafeError('Mihomo 本地 API 响应中断；原始响应已隐藏。')));
      response.on('end', () => {
        if (response.statusCode < 200 || response.statusCode >= 300) {
          reject(new SafeError(`Mihomo 本地 API 返回 HTTP ${response.statusCode}；原始响应已隐藏。`));
          return;
        }
        try {
          const result = Buffer.concat(chunks).toString('utf8');
          resolve(result ? JSON.parse(result) : null);
        } catch { reject(new SafeError('Mihomo 本地 API 返回了无法解析的响应；内容已隐藏。')); }
      });
    });
    request.setTimeout(30000, () => request.destroy());
    request.on('error', () => reject(new SafeError('无法通过本地命名管道访问 Mihomo API；未输出密钥或响应内容。')));
    if (data !== null) request.write(data);
    request.end();
  });
}

function selectors(proxies) {
  const result = {};
  for (const [name, proxy] of Object.entries(proxies || {})) {
    if (proxy.type === 'Selector' && typeof proxy.now === 'string') result[name] = proxy.now;
  }
  return result;
}

async function select(ctx, name, value) {
  await api(ctx, 'PUT', `/proxies/${encodeURIComponent(name)}`, { name: value });
}

async function restoreSelections(ctx, previous, migrate) {
  const { proxies } = await api(ctx, 'GET', '/proxies');
  const configuredGroups = new Set((ctx.base['proxy-groups'] || []).map(group => group.name));
  for (const [name, oldValue] of Object.entries(previous)) {
    if (NAMES.includes(name) && migrate) continue;
    const current = proxies[name];
    if (!current || current.type !== 'Selector' || !Array.isArray(current.all)) continue;
    const value = migrate && name !== 'GLOBAL' && configuredGroups.has(name) && oldValue === 'DIRECT' && current.all.includes(GROUP) ? GROUP : oldValue;
    if (current.all.includes(value) && current.now !== value) await select(ctx, name, value);
  }
}

async function reload(ctx, config) {
  // Intentionally no ?force=true: unrelated established connections remain intact.
  await api(ctx, 'PUT', '/configs', { payload: JSON.stringify(configurationForAPI(ctx, config)) });
}

async function verifyAPI(ctx) {
  const response = await api(ctx, 'GET', '/proxies');
  if (!response || !response.proxies || NAMES.some(name => !response.proxies[name])) fail('Mihomo API 未显示完整的 Gwifi 出口和分组。');
  const rules = await api(ctx, 'GET', '/rules');
  if (!rules || !Array.isArray(rules.rules) || !rules.rules.some(rule => rule.proxy === GROUP)) fail('Mihomo API 未显示 Gwifi 下载分流规则。');
  return response.proxies;
}

async function install(options) {
  const ctx = prepare(options, true);
  if (ctx.state && ctx.state.active) {
    await verifyAPI(ctx);
    console.log('Gwifi 全局分流已安装，未重复修改。');
    return statusWithContext(ctx);
  }
  const previous = selectors((await api(ctx, 'GET', '/proxies')).proxies);
  assertUnchanged(ctx);
  const backupDir = `${new Date().toISOString().replace(/[:.]/g, '-')}-${crypto.randomUUID().slice(0, 8)}`;
  const fullBackupDir = path.join(ctx.backupRoot, backupDir);
  fs.mkdirSync(fullBackupDir, { recursive: true });
  fs.writeFileSync(path.join(fullBackupDir, 'Script.js'), ctx.original, { flag: 'wx' });
  fs.writeFileSync(path.join(fullBackupDir, 'runtime.yaml'), ctx.runtimeBytes, { flag: 'wx' });
  const state = {
    ...ctx.plan, active: false, backupDir, originalSelections: previous,
    createdAt: new Date().toISOString(), status: 'prepared',
  };
  saveState(ctx, state);
  let scriptWritten = false;
  let reloadAttempted = false;
  try {
    assertUnchanged(ctx);
    writeAtomic(ctx.scriptFile, ctx.wrapper);
    scriptWritten = true;
    reloadAttempted = true;
    await reload(ctx, ctx.candidate);
    await select(ctx, GROUP, MODES.wired);
    await restoreSelections(ctx, previous, true);
    const proxies = await verifyAPI(ctx);
    if (proxies[GROUP].now !== MODES.wired) fail('初始有线模式未生效。');
    saveState(ctx, { ...state, active: true, status: 'installed', installedAt: new Date().toISOString() });
  } catch (error) {
    const issues = [];
    if (scriptWritten) {
      try {
        if (hash(fs.readFileSync(ctx.scriptFile)) !== hash(ctx.wrapper)) fail('全局脚本出现并发编辑');
        writeAtomic(ctx.scriptFile, ctx.original);
      } catch { issues.push('全局脚本未自动恢复（可能存在并发编辑）'); }
    }
    if (reloadAttempted) {
      try { await reload(ctx, ctx.base); await restoreSelections(ctx, previous, false); }
      catch { issues.push('内核运行配置未能自动恢复'); }
    }
    try { saveState(ctx, { ...state, status: issues.length ? 'rollback-incomplete' : 'rolled-back', rollbackIssues: issues }); }
    catch { issues.push('状态文件未能更新'); }
    if (issues.length) fail(`安装失败；${issues.join('；')}。原始备份保留在 ${fullBackupDir}。没有停止内核。`);
    fail(`安装失败，已恢复原脚本及原运行配置。备份保留在 ${fullBackupDir}。${error instanceof SafeError ? ' ' + error.message : ''}`);
  }
  console.log('已安装，当前为 wired（仅有线）模式；既有代理分组选择已保留，原 DIRECT 选择已迁移至 Gwifi 总开关。');
  console.log(`恢复备份：${fullBackupDir}`);
  console.log('全局 Script.js 已持久化；本次 API 热加载只更新内核。Clash Verge 界面若显示旧状态，需重新激活当前配置来同步内存。');
  await statusWithContext(ctx);
}

async function statusWithContext(ctx) {
  const response = await api(ctx, 'GET', '/proxies');
  console.log(`持久化安装状态：${ctx.state && ctx.state.active ? '已安装' : '未安装'}`);
  for (const name of NAMES) {
    const proxy = response && response.proxies && response.proxies[name];
    if (!proxy) { console.log(`${name}: 不存在`); continue; }
    const fields = [];
    if (typeof proxy.now === 'string') fields.push(`selected=${ALLOWED_SELECTIONS.has(proxy.now) ? proxy.now : '(已隐藏)'}`);
    if (typeof proxy.alive === 'boolean') fields.push(`alive=${proxy.alive}`);
    const history = Array.isArray(proxy.history) ? proxy.history : [];
    const last = history[history.length - 1];
    if (last && typeof last.delay === 'number') fields.push(`lastDelay=${last.delay}ms`);
    console.log(`${name}: ${fields.join(', ') || '尚无健康检查结果'}`);
  }
}

async function mode(options) {
  const ctx = loadContext(options);
  checkInstalled(ctx);
  await verifyAPI(ctx);
  assertUnchanged(ctx);
  await select(ctx, GROUP, MODES[options.mode]);
  const proxies = (await api(ctx, 'GET', '/proxies')).proxies;
  if (!proxies[GROUP] || proxies[GROUP].now !== MODES[options.mode]) fail('模式切换尚未得到内核确认。');
  console.log(`GWIFI-DOWNLOAD: selected=${MODES[options.mode]}`);
  if (options.mode === 'dual') console.log('双路模式按连接分流；单条 TCP/QUIC 连接不会被拆到两条链路。');
}

async function uninstall(options) {
  const ctx = loadContext(options);
  checkInstalled(ctx);
  const original = verifiedBackup(ctx, 'script').bytes;
  const runtime = verifiedBackup(ctx, 'runtime');
  const originalConfig = readYaml(runtime.file, ctx.python);
  const expectedInstalledConfig = applyScript(ctx.scriptBytes.toString('utf8'), originalConfig);
  const currentKey = canonicalJSON(configurationForAPI(ctx, ctx.base));
  const originalKey = canonicalJSON(configurationForAPI(ctx, originalConfig));
  const installedKey = canonicalJSON(configurationForAPI(ctx, expectedInstalledConfig));
  if (currentKey !== originalKey && currentKey !== installedKey) {
    fail(`当前运行配置已发生订阅更新或其他改动，自动卸载会覆盖这些更新，因此已停止。请先运行 mode off，再将备份 ${path.join(ctx.backupRoot, ctx.state.backupDir, 'Script.js')} 恢复到全局 Script.js，并在 Clash Verge 重新激活当前配置。`);
  }
  const before = selectors((await api(ctx, 'GET', '/proxies')).proxies);
  const installedConfig = applyScript(ctx.scriptBytes.toString('utf8'), ctx.base);
  let changed = false;
  try {
    assertUnchanged(ctx);
    writeAtomic(ctx.scriptFile, original);
    changed = true;
    await reload(ctx, originalConfig);
    // Preserve node changes made after installation. Only the added selector
    // has no equivalent in the original configuration, so map it to DIRECT.
    const restored = {};
    for (const [name, value] of Object.entries(before)) {
      if (!NAMES.includes(name)) restored[name] = value === GROUP ? 'DIRECT' : value;
    }
    await restoreSelections(ctx, restored, false);
    const proxies = (await api(ctx, 'GET', '/proxies')).proxies;
    if (NAMES.some(name => proxies[name])) fail('卸载后内核仍存在 Gwifi 分组。');
    saveState(ctx, { ...ctx.state, active: false, status: 'uninstalled', uninstalledAt: new Date().toISOString() });
  } catch (error) {
    if (!changed) throw error;
    const issues = [];
    try {
      if (hash(fs.readFileSync(ctx.scriptFile)) !== hash(original)) fail('全局脚本出现并发编辑');
      writeAtomic(ctx.scriptFile, ctx.scriptBytes);
    } catch { issues.push('全局脚本存在并发编辑或无法恢复'); }
    try { await reload(ctx, installedConfig); await restoreSelections(ctx, before, false); }
    catch { issues.push('无法恢复卸载前的内核配置'); }
    if (issues.length) fail(`卸载未完成；${issues.join('；')}。备份保留在 ${ctx.backupRoot}。没有停止内核。`);
    fail(`卸载失败，已恢复卸载前的分流配置。${error instanceof SafeError ? ' ' + error.message : ''}`);
  }
  console.log('已恢复安装前的全局脚本与内核运行配置；保留当前有效的代理选择，Gwifi 选择恢复 DIRECT。备份保留。');
  console.log('API 恢复只更新内核；请在 Clash Verge 重新激活当前配置，以同步界面内存。');
}

async function main(argv = process.argv.slice(2)) {
  const options = parseArgs(argv);
  switch (options.command) {
    case 'prepare': return prepare(options);
    case 'install': return install(options);
    case 'status': return statusWithContext(loadContext(options));
    case 'mode': return mode(options);
    case 'uninstall': return uninstall(options);
    default:
      console.log('node scripts/gwifi-system.cjs prepare|install|status|uninstall [--profile-dir 路径]');
      console.log('node scripts/gwifi-system.cjs mode dual|wired|wifi|off [--profile-dir 路径]');
      console.log('可选：--python Python可执行文件、--core Mihomo可执行文件。prepare 仅生成摘要并校验，不安装。');
  }
}

module.exports = { parseArgs, identityScript, makeWrapper, applyScript, selectors, configurationForAPI, canonicalJSON, main };
if (require.main === module) {
  main().catch(error => {
    console.error(`错误：${error instanceof SafeError ? error.message : '操作未完成；为保护本地配置，底层错误详情已隐藏。'}`);
    process.exitCode = 1;
  });
}
