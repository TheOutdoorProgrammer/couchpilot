let sessions = [];
let config = {};
let cachedModels = [];
let expandedId = null;
let confirmCallback = null;
let eventSource = null;
let loginRunning = false;
let authStatus = {};
let appStarted = false;
let settingsDraft = {};
let appVersion = '';

document.addEventListener('DOMContentLoaded', init);

async function init() {
  bindAuthEvents();
  fetchVersion();
  try {
    authStatus = await api('/auth/status');
  } catch {
    authStatus = { enabled: false, authed: true };
  }
  if (!authStatus.authed) {
    showAuthGate();
    return;
  }
  await startApp();
}

async function startApp() {
  hideAuthGate();
  if (appStarted) return;
  appStarted = true;
  await fetchConfig();
  await fetchModels();
  connectSSE();
  bindEvents();
}

// --- API ---

async function api(path, opts = {}) {
  const res = await fetch('/api' + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (res.status === 401) {
    handleUnauthorized();
    throw new Error('Session expired — please log in');
  }
  if (!res.ok && res.status !== 204) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  if (res.status === 204) return null;
  return res.json();
}

// --- Version ---

async function fetchVersion() {
  try {
    const v = await api('/version');
    appVersion = (v && v.version) ? v.version : '';
  } catch {
    appVersion = '';
  }
  renderVersionLabels();
}

function renderVersionLabels() {
  let label = '';
  if (appVersion) {
    label = appVersion === 'dev'
      ? 'couchpilot · dev build'
      : 'couchpilot · v' + appVersion.replace(/^v/, '');
  }
  for (const id of ['auth-version', 'settings-version']) {
    const el = document.getElementById(id);
    if (el) el.textContent = label;
  }
}

// --- Auth ---

// authReq talks to the auth endpoints without triggering the global 401
// redirect (a wrong password on the login screen is expected, not a session
// expiry).
async function authReq(path, body, method = 'POST') {
  const res = await fetch('/api' + path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body || {}),
  });
  if (!res.ok && res.status !== 204) {
    const b = await res.json().catch(() => ({}));
    throw new Error(b.error || res.statusText);
  }
  return res.status === 204 ? null : res.json();
}

function showAuthGate() {
  const gate = document.getElementById('auth-gate');
  const needsSetup = !!authStatus.needsSetup;
  document.getElementById('auth-login-form').hidden = needsSetup;
  document.getElementById('auth-setup-form').hidden = !needsSetup;
  gate.hidden = false;
  renderVersionLabels();
  document.body.classList.add('modal-open');
  setTimeout(() => {
    const id = needsSetup ? 'auth-setup-password' : 'auth-login-password';
    document.getElementById(id).focus();
  }, 60);
}

function hideAuthGate() {
  document.getElementById('auth-gate').hidden = true;
  if (!document.querySelector('.modal.active')) {
    document.body.classList.remove('modal-open');
  }
}

function handleUnauthorized() {
  authStatus = { enabled: true, authed: false, needsSetup: false };
  showAuthGate();
}

async function afterAuthSuccess() {
  try {
    authStatus = await api('/auth/status');
  } catch {
    authStatus = { enabled: true, authed: true };
  }
  const wasStarted = appStarted;
  await startApp();
  // On a re-auth (cookie expired while the app was open) startApp short-circuits,
  // so refresh the SSE stream that died on 401. First-time start already connects.
  if (wasStarted) connectSSE();
}

function bindAuthEvents() {
  document.getElementById('auth-login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const err = document.getElementById('auth-login-error');
    err.hidden = true;
    const input = document.getElementById('auth-login-password');
    try {
      await authReq('/auth/login', { password: input.value });
      input.value = '';
      await afterAuthSuccess();
    } catch (ex) {
      err.textContent = ex.message || 'Login failed';
      err.hidden = false;
    }
  });

  document.getElementById('auth-setup-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const err = document.getElementById('auth-setup-error');
    err.hidden = true;
    const pw = document.getElementById('auth-setup-password').value;
    const confirm = document.getElementById('auth-setup-confirm').value;
    if (pw.length < 6) { err.textContent = 'Password must be at least 6 characters'; err.hidden = false; return; }
    if (pw !== confirm) { err.textContent = 'Passwords do not match'; err.hidden = false; return; }
    try {
      await authReq('/auth/setup', { password: pw });
      await afterAuthSuccess();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    }
  });

  document.getElementById('auth-setup-disable').addEventListener('click', async () => {
    const err = document.getElementById('auth-setup-error');
    err.hidden = true;
    try {
      await authReq('/auth/setup', { disable: true });
      await afterAuthSuccess();
    } catch (ex) {
      err.textContent = ex.message;
      err.hidden = false;
    }
  });
}

async function fetchConfig() {
  config = await api('/config');
}

async function fetchProjects() {
  return api('/projects');
}

async function fetchBranches(path) {
  return api('/branches?path=' + encodeURIComponent(path));
}

async function createSession(data) {
  return api('/sessions', { method: 'POST', body: JSON.stringify(data) });
}

async function killSession(id) {
  return api('/sessions/' + id, { method: 'DELETE' });
}

async function dismissSession(id) {
  return api('/sessions/' + id + '/dismiss', { method: 'POST' });
}

async function resumeSession(id) {
  return api('/sessions/' + id + '/resume', { method: 'POST' });
}

async function restartChannels() {
  return api('/channels/restart', { method: 'POST' });
}

async function fetchModels() {
  try {
    cachedModels = await api('/models') || [];
  } catch {
    cachedModels = [];
  }
  return cachedModels;
}

let activeModelPickerTarget = null;

function getModelEntries() {
  const aliases = [
    { id: 'opus', name: 'opus', family: 'aliases' },
    { id: 'sonnet', name: 'sonnet', family: 'aliases' },
    { id: 'haiku', name: 'haiku', family: 'aliases' },
  ];
  return [...cachedModels, ...aliases];
}

function renderModelPicker(query) {
  const list = document.getElementById('model-picker-list');
  const q = (query || '').toLowerCase().trim();
  const all = getModelEntries();

  const filtered = q
    ? all.filter(m => m.id.toLowerCase().includes(q) || m.name.toLowerCase().includes(q) || (m.family || '').toLowerCase().includes(q))
    : all;

  const familyOrder = ['claude-opus', 'claude-sonnet', 'claude-haiku', 'claude-fable'];
  const groups = new Map();
  for (const m of filtered) {
    const fam = m.family || 'other';
    if (!groups.has(fam)) groups.set(fam, []);
    groups.get(fam).push(m);
  }

  const ordered = familyOrder.filter(f => groups.has(f));
  for (const fam of groups.keys()) {
    if (!ordered.includes(fam)) ordered.push(fam);
  }

  const parts = [];
  const emptyLabel = activeModelPickerTarget === 'session-model' ? 'Default' : 'None';
  parts.push(`<button type="button" class="picker-item" data-model-id=""><div class="picker-item-main"><span class="picker-item-name">${emptyLabel}</span></div></button>`);

  for (const fam of ordered) {
    const label = fam === 'aliases' ? 'Aliases' : fam.replace('claude-', '').replace(/^./, c => c.toUpperCase());
    parts.push(`<div class="picker-group-label">${esc(label)}</div>`);
    for (const m of groups.get(fam)) {
      const displayName = m.family === 'aliases' ? m.name : m.name.replace('Claude ', '');
      parts.push(`<button type="button" class="picker-item" data-model-id="${esc(m.id)}">
        <div class="picker-item-main">
          <span class="picker-item-name">${esc(displayName)}</span>
          ${m.family !== 'aliases' ? `<span class="picker-item-path">${esc(m.id)}</span>` : ''}
        </div>
      </button>`);
    }
  }

  if (filtered.length === 0 && q) {
    parts.push('<div class="picker-empty">No matches</div>');
  }

  parts.push('<button type="button" class="picker-item picker-item-custom" data-model-id="__custom__"><div class="picker-item-main"><span class="picker-item-name">Custom ID…</span></div></button>');

  list.innerHTML = parts.join('');
}

function openModelPicker(target) {
  activeModelPickerTarget = target;
  const search = document.getElementById('model-picker-search');
  search.value = '';
  renderModelPicker('');
  openModal('model-picker-modal');

  const currentId = document.getElementById(target + '-hidden').value;
  if (currentId && currentId !== '__custom__') {
    requestAnimationFrame(() => {
      const el = document.querySelector(`#model-picker-list .picker-item[data-model-id="${CSS.escape(currentId)}"]`);
      if (el) el.scrollIntoView({ block: 'center' });
    });
  }
}

function closeModelPicker() {
  closeModal('model-picker-modal');
  activeModelPickerTarget = null;
}

function selectModel(target, modelId) {
  const valueEl = document.getElementById(target + '-value');
  const hidden = document.getElementById(target + '-hidden');
  const customInput = document.getElementById(target + '-custom');
  const pickerEl = document.getElementById(target + '-picker');

  if (modelId === '__custom__') {
    valueEl.textContent = 'Custom…';
    valueEl.classList.add('placeholder');
    hidden.value = '__custom__';
    customInput.style.display = '';
    customInput.focus();
    if (pickerEl) pickerEl.classList.remove('open');
    return;
  }

  customInput.style.display = 'none';
  customInput.value = '';

  if (!modelId) {
    const defaultLabel = target === 'session-model' ? 'Default' : 'None';
    valueEl.textContent = defaultLabel;
    valueEl.classList.add('placeholder');
    hidden.value = '';
    if (pickerEl) pickerEl.classList.remove('open');
    return;
  }

  const entry = getModelEntries().find(m => m.id === modelId);
  const displayName = entry ? (entry.family === 'aliases' ? entry.name : entry.name.replace('Claude ', '')) : modelId;
  valueEl.textContent = displayName;
  valueEl.classList.remove('placeholder');
  hidden.value = modelId;
  if (pickerEl) pickerEl.classList.remove('open');
}

function getModelValue(target) {
  const hidden = document.getElementById(target + '-hidden');
  if (hidden.value === '__custom__') {
    return document.getElementById(target + '-custom').value;
  }
  return hidden.value;
}

function setModelFromValue(target, value) {
  if (!value) {
    selectModel(target, '');
    return;
  }
  const entry = getModelEntries().find(m => m.id === value);
  if (entry) {
    selectModel(target, value);
  } else {
    selectModel(target, '__custom__');
    document.getElementById(target + '-custom').value = value;
  }
}

async function saveConfig(data) {
  config = await api('/config', { method: 'PUT', body: JSON.stringify(data) });
}

async function fetchLoginState() {
  return api('/login');
}

async function startLogin(opts) {
  return api('/login', { method: 'POST', body: JSON.stringify(opts || {}) });
}

async function sendLoginInput(data) {
  return api('/login/input', { method: 'POST', body: JSON.stringify({ data }) });
}

async function stopLogin() {
  return api('/login', { method: 'DELETE' });
}

// --- SSE ---

function connectSSE() {
  if (eventSource) eventSource.close();
  eventSource = new EventSource('/api/events');

  eventSource.addEventListener('init', (e) => {
    sessions = JSON.parse(e.data);
    render();
  });

  eventSource.addEventListener('session_created', (e) => {
    const s = JSON.parse(e.data);
    const idx = sessions.findIndex(x => x.id === s.id);
    if (idx >= 0) sessions[idx] = s;
    else sessions.unshift(s);
    render();
  });

  eventSource.addEventListener('session_updated', (e) => {
    const s = JSON.parse(e.data);
    const idx = sessions.findIndex(x => x.id === s.id);
    if (idx >= 0) sessions[idx] = s;
    else sessions.unshift(s);
    render();
  });

  eventSource.addEventListener('session_died', (e) => {
    const s = JSON.parse(e.data);
    const idx = sessions.findIndex(x => x.id === s.id);
    if (idx >= 0) sessions[idx] = s;
    render();
  });

  eventSource.addEventListener('session_dismissed', (e) => {
    const d = JSON.parse(e.data);
    sessions = sessions.filter(s => s.id !== d.id);
    render();
  });

  eventSource.addEventListener('login_started', (e) => {
    const state = JSON.parse(e.data);
    setLoginState(state, { replace: true });
  });

  eventSource.addEventListener('login_output', (e) => {
    const d = JSON.parse(e.data);
    appendLoginOutput(d.data || '');
  });

  eventSource.addEventListener('login_ended', (e) => {
    const state = JSON.parse(e.data);
    loginRunning = false;
    updateLoginButtons();
    const code = state.exitCode == null ? '?' : state.exitCode;
    appendLoginOutput(`\n[claude login exited with code ${code}]\n`);
  });


  eventSource.onerror = () => {
    eventSource.close();
    setTimeout(connectSSE, 3000);
  };
}

// --- Rendering ---

function render() {
  renderSessions();
}

function renderSessions() {
  const container = document.getElementById('sessions');
  const empty = document.getElementById('empty');

  if (sessions.length === 0) {
    container.innerHTML = '';
    empty.style.display = '';
    return;
  }

  empty.style.display = 'none';

  container.innerHTML = sessions.map(s => {
    const isExpanded = expandedId === s.id;
    const isDead = s.status === 'dead';
    const statusClass = s.status;
    const name = s.name || 'Untitled Session';
    const dir = shortenDir(s.dir);
    const time = isDead && s.diedAt
      ? 'Ended ' + timeAgo(s.diedAt)
      : timeAgo(s.createdAt);

    let actions = '';
    if (isExpanded && s.isChannels) {
      const openLink = s.url
        ? `<a href="${esc(s.url)}" target="_blank" rel="noopener" class="btn-open">Open in Claude</a>`
        : `<span class="btn-open" style="opacity:0.4;pointer-events:none">${isDead ? 'Restarting...' : 'Waiting for URL...'}</span>`;
      actions = `<div class="session-actions">
        ${openLink}
        <button class="btn-restart" data-restart-channels>Restart</button>
      </div>`;
    } else if (isExpanded && !isDead) {
      const openLink = s.url
        ? `<a href="${esc(s.url)}" target="_blank" rel="noopener" class="btn-open">Open in Claude</a>`
        : `<span class="btn-open" style="opacity:0.4;pointer-events:none">Waiting for URL...</span>`;
      actions = `<div class="session-actions">
        ${openLink}
        <button class="btn-kill" data-kill="${s.id}">Kill</button>
      </div>`;
    } else if (isExpanded && isDead) {
      const resumeBtn = s.sessionUuid
        ? `<button class="btn-resume" data-resume="${s.id}">Resume</button>`
        : '';
      actions = `<div class="session-actions">
        ${resumeBtn}
        <button class="btn-dismiss" data-dismiss-session="${s.id}">Dismiss</button>
      </div>`;
    }

    return `<div class="session-card ${isDead ? 'dead-card' : ''} ${isExpanded ? 'expanded' : ''}" data-id="${s.id}">
      <div class="session-header">
        <span class="status-dot ${statusClass}"></span>
        <span class="session-name">${esc(name)}</span>
      </div>
      <div class="session-meta">
        <span>${esc(dir)}</span>
        <span class="sep">&middot;</span>
        <span>${time}</span>
      </div>
      ${actions}
    </div>`;
  }).join('');
}

let dirPickerProjects = [];

async function renderDirSelect() {
  try {
    dirPickerProjects = await fetchProjects();
  } catch (err) {
    dirPickerProjects = (config.favoriteDirs || []).map(p => ({ path: p, exists: true, group: 'Favorites' }));
  }
  renderDirPicker('');
  if (dirPickerProjects.length > 0) {
    setDirSelection(dirPickerProjects[0]);
  } else {
    setDirSelection(null);
  }
}

function renderDirPicker(query) {
  const list = document.getElementById('dir-picker-list');
  const q = (query || '').toLowerCase().trim();

  const filtered = q
    ? dirPickerProjects.filter(p =>
        p.path.toLowerCase().includes(q) ||
        (p.expanded || '').toLowerCase().includes(q) ||
        (p.branch || '').toLowerCase().includes(q))
    : dirPickerProjects;

  const groups = new Map();
  for (const p of filtered) {
    const g = p.group || 'Other';
    if (!groups.has(g)) groups.set(g, []);
    groups.get(g).push(p);
  }

  const parts = [];
  for (const [groupName, items] of groups) {
    parts.push(`<div class="picker-group-label">${esc(groupName)}</div>`);
    for (const p of items) {
      parts.push(renderPickerItem(p));
    }
  }
  if (filtered.length === 0) {
    parts.push('<div class="picker-empty">No matches</div>');
  }
  parts.push('<button type="button" class="picker-item picker-item-custom" data-path="__custom__"><div class="picker-item-main"><span class="picker-item-name">Custom path…</span></div></button>');

  list.innerHTML = parts.join('');
}

function renderPickerItem(p) {
  const baseName = p.path.split('/').filter(Boolean).pop() || p.path;
  const isFav = p.group === 'Favorites';
  const name = isFav ? p.path : baseName;
  const pathRow = isFav ? '' : `<span class="picker-item-path">${esc(p.path)}</span>`;
  let meta = '';
  if (!p.exists) {
    meta = '<span class="meta-missing">missing</span>';
  } else if (p.isGitRepo) {
    const branchChip = `<span class="branch-chip">${esc(p.branch || 'detached')}</span>`;
    const dirtyChip = p.dirty ? `<span class="dirty-chip" title="${p.dirtyCount} uncommitted file(s)">${p.dirtyCount}</span>` : '';
    meta = branchChip + renderAheadBehind(p) + dirtyChip;
  }
  return `<button type="button" class="picker-item" data-path="${esc(p.path)}">
    <div class="picker-item-main">
      <span class="picker-item-name">${esc(name)}</span>
      ${pathRow}
    </div>
    <div class="picker-item-meta">${meta}</div>
  </button>`;
}

function renderAheadBehind(p) {
  if (!p.upstream) return '';
  const parts = [];
  if (p.ahead) parts.push(`<span class="ab-chip ab-ahead" title="${p.ahead} ahead of ${esc(p.upstream)}">↑${p.ahead}</span>`);
  if (p.behind) parts.push(`<span class="ab-chip ab-behind" title="${p.behind} behind ${esc(p.upstream)}">↓${p.behind}</span>`);
  return parts.join('');
}

function setDirSelection(project) {
  const valueEl = document.getElementById('dir-picker-value');
  const hidden = document.getElementById('session-dir-value');
  const customInput = document.getElementById('session-dir');
  if (!project) {
    valueEl.textContent = 'Pick a project…';
    valueEl.classList.add('placeholder');
    hidden.value = '';
    customInput.style.display = 'none';
    customInput.value = '';
    refreshBranchSelect('');
    return;
  }
  if (project.path === '__custom__') {
    valueEl.textContent = 'Custom path…';
    valueEl.classList.add('placeholder');
    hidden.value = '__custom__';
    customInput.style.display = '';
    customInput.focus();
    refreshBranchSelect('');
    return;
  }
  const baseName = project.path.split('/').filter(Boolean).pop() || project.path;
  valueEl.classList.remove('placeholder');
  valueEl.innerHTML = `<span class="picker-value-name">${esc(baseName)}</span>` +
    (project.branch ? `<span class="branch-chip">${esc(project.branch)}</span>` : '') +
    renderAheadBehind(project) +
    (project.dirty ? `<span class="dirty-chip">${project.dirtyCount}</span>` : '');
  hidden.value = project.path;
  customInput.style.display = 'none';
  customInput.value = '';
  refreshBranchSelect(project.path);
}

function openDirPicker() {
  document.getElementById('dir-picker').classList.add('open');
  const search = document.getElementById('dir-picker-search');
  search.value = '';
  renderDirPicker('');
  openModal('dir-picker-modal');
  requestAnimationFrame(() => {
    const selectedPath = document.getElementById('session-dir-value').value;
    if (selectedPath && selectedPath !== '__custom__') {
      const el = document.querySelector(`#dir-picker-list .picker-item[data-path="${CSS.escape(selectedPath)}"]`);
      if (el) el.scrollIntoView({ block: 'center' });
    }
  });
}

function closeDirPicker() {
  document.getElementById('dir-picker').classList.remove('open');
  closeModal('dir-picker-modal');
}

let currentBranches = [];

async function refreshBranchSelect(path) {
  const select = document.getElementById('session-branch');
  const from = document.getElementById('session-branch-from');
  const branchField = select.closest('.field');
  const newWrap = document.getElementById('session-branch-new');

  select.innerHTML = '<option value="">(current branch)</option>';
  from.innerHTML = '<option value="">(current)</option>';
  newWrap.style.display = 'none';
  currentBranches = [];

  const known = dirPickerProjects.find(p => p.path === path);
  if (known && !known.isGitRepo) {
    branchField.style.display = 'none';
    return;
  }
  branchField.style.display = '';

  if (!path || path === '__custom__') {
    select.innerHTML += '<option value="__new__">+ New branch…</option>';
    return;
  }

  try {
    currentBranches = await fetchBranches(path);
  } catch (err) {
    currentBranches = [];
  }

  if (currentBranches.length === 0 && !known) {
    branchField.style.display = 'none';
    return;
  }

  for (const b of currentBranches) {
    select.innerHTML += `<option value="${esc(b)}">${esc(b)}</option>`;
    from.innerHTML += `<option value="${esc(b)}">${esc(b)}</option>`;
  }
  select.innerHTML += '<option value="__new__">+ New branch…</option>';
  const preferred = ['main', 'master', 'develop'].find(b => currentBranches.includes(b));
  if (preferred) from.value = preferred;
}

function renderFavDirs() {
  const container = document.getElementById('fav-dirs');
  container.innerHTML = (settingsDraft.favoriteDirs || []).map(d =>
    `<div class="fav-item">
      <span>${esc(d)}</span>
      <button class="fav-remove" data-remove-fav="${esc(d)}">&times;</button>
    </div>`
  ).join('');
}

function renderProjectRoots() {
  const container = document.getElementById('project-roots');
  container.innerHTML = (settingsDraft.projectRoots || []).map(d =>
    `<div class="fav-item">
      <span>${esc(d)}</span>
      <button class="fav-remove" data-remove-root="${esc(d)}">&times;</button>
    </div>`
  ).join('');
}

function renderPluginDirs() {
  const container = document.getElementById('plugin-dirs');
  container.innerHTML = (settingsDraft.pluginDirs || []).map(d =>
    `<div class="fav-item">
      <span>${esc(d)}</span>
      <button class="fav-remove" data-remove-plugin-dir="${esc(d)}">&times;</button>
    </div>`
  ).join('');
}

function updatePermWarn() {
  const v = document.getElementById('default-perm-mode').value;
  document.getElementById('default-perm-warn').hidden = v !== 'bypassPermissions';
}

function openSettings() {
  // Work on a clone so Cancel discards edits and list changes never look saved
  // until Save actually persists them.
  settingsDraft = {
    favoriteDirs: [...(config.favoriteDirs || [])],
    projectRoots: [...(config.projectRoots || [])],
    pluginDirs: [...(config.pluginDirs || [])],
  };
  renderFavDirs();
  renderProjectRoots();
  renderPluginDirs();
  document.getElementById('default-perm-mode').value = config.defaultPermissionMode || 'bypassPermissions';
  updatePermWarn();
  setModelFromValue('default-model', config.defaultModel || '');
  document.getElementById('default-effort').value = config.defaultEffort || '';
  document.getElementById('channels-enabled').checked = !!config.channelsEnabled;
  document.getElementById('channels-config').style.display = config.channelsEnabled ? '' : 'none';
  document.getElementById('default-channels').value = config.defaultChannels || '';
  renderSecurity();
  renderVersionLabels();
  openModal('settings-modal');
}

function renderSecurity() {
  const enabled = !!authStatus.enabled;
  document.getElementById('auth-enabled').checked = enabled;
  document.getElementById('auth-pw-label').textContent =
    authStatus.hasPassword ? 'Change password' : 'Set a password';
  document.getElementById('auth-new-password').value = '';
  document.getElementById('logout-btn').style.display = enabled ? '' : 'none';
}

// --- Events ---

function bindEvents() {
  document.getElementById('new-btn').addEventListener('click', () => {
    openNewModal();
  });

  document.getElementById('settings-btn').addEventListener('click', openSettings);

  document.getElementById('default-perm-mode').addEventListener('change', updatePermWarn);

  document.getElementById('auth-enabled').addEventListener('change', async (e) => {
    const wantOn = e.target.checked;
    if (wantOn && !authStatus.hasPassword) {
      e.target.checked = false;
      toast('Set a password first, then enable', true);
      document.getElementById('auth-new-password').focus();
      return;
    }
    try {
      authStatus = await authReq('/auth/config', { enabled: wantOn }, 'PUT');
      renderSecurity();
      toast(wantOn ? 'Password protection on' : 'Password protection off');
    } catch (err) {
      e.target.checked = !wantOn;
      toast(err.message, true);
    }
  });

  document.getElementById('auth-save-password').addEventListener('click', async () => {
    const input = document.getElementById('auth-new-password');
    const pw = input.value;
    if (pw.length < 6) { toast('Password must be at least 6 characters', true); return; }
    try {
      const body = { newPassword: pw };
      if (!authStatus.enabled) body.enabled = true; // setting a password from off also turns it on
      authStatus = await authReq('/auth/config', body, 'PUT');
      input.value = '';
      renderSecurity();
      toast('Password updated');
    } catch (err) {
      toast(err.message, true);
    }
  });

  document.getElementById('logout-btn').addEventListener('click', async () => {
    try {
      await authReq('/auth/logout', {});
      closeModal('settings-modal');
      handleUnauthorized();
    } catch (err) {
      toast(err.message, true);
    }
  });

  document.getElementById('sessions').addEventListener('click', (e) => {
    const killBtn = e.target.closest('[data-kill]');
    if (killBtn) {
      e.stopPropagation();
      const id = killBtn.dataset.kill;
      confirmAction('Kill this session?', async () => {
        try {
          await killSession(id);
        } catch (err) {
          toast(err.message, true);
        }
      });
      return;
    }

    const restartBtn = e.target.closest('[data-restart-channels]');
    if (restartBtn) {
      e.stopPropagation();
      restartChannels().then(() => toast('Channels restarting...')).catch(err => toast(err.message, true));
      return;
    }

    const dismissBtn = e.target.closest('[data-dismiss-session]');
    if (dismissBtn) {
      e.stopPropagation();
      dismissSession(dismissBtn.dataset.dismissSession);
      return;
    }

    const resumeBtn = e.target.closest('[data-resume]');
    if (resumeBtn) {
      e.stopPropagation();
      resumeSession(resumeBtn.dataset.resume)
        .then(() => toast('Resuming session...'))
        .catch(err => toast(err.message, true));
      return;
    }

    const card = e.target.closest('.session-card');
    if (card) {
      const id = card.dataset.id;
      expandedId = expandedId === id ? null : id;
      render();
    }
  });

  document.getElementById('new-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const form = e.target;
    const btn = document.getElementById('launch-btn');
    btn.disabled = true;
    btn.textContent = 'Launching...';

    const dirValue = document.getElementById('session-dir-value').value;
    const dir = dirValue === '__custom__'
      ? form.dir.value.trim()
      : dirValue;

    const branchSelect = document.getElementById('session-branch');
    let branch = '';
    let createBranch = false;
    let branchFrom = '';
    if (branchSelect.value === '__new__') {
      branch = document.getElementById('session-branch-custom').value.trim();
      createBranch = !!branch;
      branchFrom = document.getElementById('session-branch-from').value;
    } else if (branchSelect.value) {
      branch = branchSelect.value;
    }

    try {
      await createSession({
        name: form.name.value.trim(),
        dir: dir || config.defaultDir,
        permissionMode: form.permissionMode.value,
        model: getModelValue('session-model'),
        effort: form.effort.value,
        branch,
        createBranch,
        branchFrom,
      });
      closeModal('new-modal');
      form.reset();
      toast('Session launched');
    } catch (err) {
      toast(err.message, true);
    } finally {
      btn.disabled = false;
      btn.textContent = 'Launch';
    }
  });

  document.getElementById('dir-picker-trigger').addEventListener('click', () => {
    openDirPicker();
  });

  let searchDebounce;
  document.getElementById('dir-picker-search').addEventListener('input', (e) => {
    const val = e.target.value;
    clearTimeout(searchDebounce);
    searchDebounce = setTimeout(() => renderDirPicker(val), 60);
  });

  document.getElementById('dir-picker-list').addEventListener('click', (e) => {
    const item = e.target.closest('.picker-item');
    if (!item) return;
    const path = item.dataset.path;
    if (path === '__custom__') {
      setDirSelection({ path: '__custom__' });
    } else {
      const project = dirPickerProjects.find(p => p.path === path);
      if (project) setDirSelection(project);
    }
    closeDirPicker();
  });

  document.getElementById('dir-picker-search').addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      closeDirPicker();
    }
  });

  document.getElementById('session-branch').addEventListener('change', (e) => {
    const wrap = document.getElementById('session-branch-new');
    const custom = document.getElementById('session-branch-custom');
    if (e.target.value === '__new__') {
      wrap.style.display = '';
      custom.focus();
    } else {
      wrap.style.display = 'none';
      custom.value = '';
    }
  });

  document.getElementById('save-settings').addEventListener('click', async () => {
    try {
      await saveConfig({
        favoriteDirs: settingsDraft.favoriteDirs,
        projectRoots: settingsDraft.projectRoots,
        defaultPermissionMode: document.getElementById('default-perm-mode').value,
        defaultModel: getModelValue('default-model'),
        defaultEffort: document.getElementById('default-effort').value,
        channelsEnabled: document.getElementById('channels-enabled').checked,
        defaultChannels: document.getElementById('default-channels').value.trim(),
        pluginDirs: settingsDraft.pluginDirs,
      });
      closeModal('settings-modal');
      toast('Settings saved');
    } catch (err) {
      toast(err.message, true);
    }
  });

  document.getElementById('add-fav-btn').addEventListener('click', () => {
    const input = document.getElementById('new-fav-dir');
    const val = input.value.trim();
    if (!val) return;
    if (!settingsDraft.favoriteDirs) settingsDraft.favoriteDirs = [];
    if (!settingsDraft.favoriteDirs.includes(val)) {
      settingsDraft.favoriteDirs.push(val);
      renderFavDirs();
    }
    input.value = '';
  });

  document.getElementById('add-project-root-btn').addEventListener('click', () => {
    const input = document.getElementById('new-project-root');
    const val = input.value.trim();
    if (!val) return;
    if (!settingsDraft.projectRoots) settingsDraft.projectRoots = [];
    if (!settingsDraft.projectRoots.includes(val)) {
      settingsDraft.projectRoots.push(val);
      renderProjectRoots();
    }
    input.value = '';
  });

  document.getElementById('fav-dirs').addEventListener('click', (e) => {
    const btn = e.target.closest('[data-remove-fav]');
    if (btn) {
      settingsDraft.favoriteDirs = (settingsDraft.favoriteDirs || []).filter(d => d !== btn.dataset.removeFav);
      renderFavDirs();
    }
  });

  document.getElementById('project-roots').addEventListener('click', (e) => {
    const btn = e.target.closest('[data-remove-root]');
    if (btn) {
      settingsDraft.projectRoots = (settingsDraft.projectRoots || []).filter(d => d !== btn.dataset.removeRoot);
      renderProjectRoots();
    }
  });

  document.getElementById('add-plugin-dir-btn').addEventListener('click', () => {
    const input = document.getElementById('new-plugin-dir');
    const val = input.value.trim();
    if (!val) return;
    if (!settingsDraft.pluginDirs) settingsDraft.pluginDirs = [];
    if (!settingsDraft.pluginDirs.includes(val)) {
      settingsDraft.pluginDirs.push(val);
      renderPluginDirs();
    }
    input.value = '';
  });

  document.getElementById('plugin-dirs').addEventListener('click', (e) => {
    const btn = e.target.closest('[data-remove-plugin-dir]');
    if (btn) {
      settingsDraft.pluginDirs = (settingsDraft.pluginDirs || []).filter(d => d !== btn.dataset.removePluginDir);
      renderPluginDirs();
    }
  });

  document.getElementById('channels-enabled').addEventListener('change', (e) => {
    document.getElementById('channels-config').style.display = e.target.checked ? '' : 'none';
  });

  document.getElementById('session-model-trigger').addEventListener('click', () => {
    openModelPicker('session-model');
  });

  document.getElementById('default-model-trigger').addEventListener('click', () => {
    openModelPicker('default-model');
  });

  document.getElementById('model-picker-search').addEventListener('input', (e) => {
    renderModelPicker(e.target.value);
  });

  document.getElementById('model-picker-list').addEventListener('click', (e) => {
    const item = e.target.closest('.picker-item');
    if (!item || !activeModelPickerTarget) return;
    const modelId = item.dataset.modelId;
    selectModel(activeModelPickerTarget, modelId);
    closeModelPicker();
  });

  document.getElementById('open-login-btn').addEventListener('click', openLoginModal);

  document.getElementById('login-start-btn').addEventListener('click', async () => {
    try {
      const opts = {
        method: document.getElementById('login-method').value,
        email: document.getElementById('login-email').value.trim(),
      };
      const state = await startLogin(opts);
      setLoginState(state, { replace: true });
    } catch (err) {
      toast(err.message, true);
    }
  });

  document.getElementById('login-stop-btn').addEventListener('click', async () => {
    try {
      await stopLogin();
    } catch (err) {
      toast(err.message, true);
    }
  });

  document.querySelectorAll('#login-modal .key-btn').forEach(btn => {
    btn.addEventListener('click', () => sendLoginKey(btn.dataset.key));
  });

  document.getElementById('login-send').addEventListener('click', submitLoginInput);
  document.getElementById('login-input').addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      submitLoginInput();
    }
  });

  document.getElementById('confirm-yes').addEventListener('click', () => {
    closeModal('confirm-modal');
    if (confirmCallback) confirmCallback();
    confirmCallback = null;
  });

  document.querySelectorAll('[data-dismiss]').forEach(btn => {
    btn.addEventListener('click', () => closeModal(btn.dataset.dismiss));
  });

  document.querySelectorAll('.modal-backdrop').forEach(backdrop => {
    backdrop.addEventListener('click', () => {
      const modal = backdrop.closest('.modal');
      if (modal) closeModal(modal.id);
    });
  });
}

// --- Modals ---

function openNewModal() {
  const form = document.getElementById('new-form');
  form.reset();
  document.getElementById('perm-mode').value = config.defaultPermissionMode || 'bypassPermissions';
  setModelFromValue('session-model', config.defaultModel || '');
  document.getElementById('session-effort').value = config.defaultEffort || '';
  document.getElementById('session-dir').value = '';
  document.getElementById('session-dir').style.display = 'none';
  document.getElementById('session-branch-custom').value = '';
  document.getElementById('session-branch-new').style.display = 'none';
  renderDirSelect();
  openModal('new-modal');
}

function openModal(id) {
  document.querySelectorAll('.modal.active:not(.behind)').forEach(m => m.classList.add('behind'));
  document.getElementById(id).classList.add('active');
  document.body.classList.add('modal-open');
}

function closeModal(id) {
  document.getElementById(id).classList.remove('active');
  const behind = document.querySelectorAll('.modal.behind');
  if (behind.length > 0) {
    behind[behind.length - 1].classList.remove('behind');
  }
  if (!document.querySelector('.modal.active')) {
    document.body.classList.remove('modal-open');
  }
}

async function openLoginModal() {
  closeModal('settings-modal');
  document.getElementById('login-output').textContent = '';
  openModal('login-modal');
  try {
    const state = await fetchLoginState();
    setLoginState(state, { replace: true });
  } catch (err) {
    toast(err.message, true);
  }
}

function setLoginState(state, { replace = false } = {}) {
  loginRunning = !!state.running;
  if (replace) {
    const out = document.getElementById('login-output');
    out.textContent = stripANSI(state.output || '');
    out.scrollTop = out.scrollHeight;
    renderLoginLinks();
  }
  updateLoginButtons();
}

function updateLoginButtons() {
  const start = document.getElementById('login-start-btn');
  const stop = document.getElementById('login-stop-btn');
  if (!start || !stop) return;
  if (loginRunning) {
    start.style.display = 'none';
    stop.style.display = '';
  } else {
    start.style.display = '';
    stop.style.display = 'none';
  }
}

function appendLoginOutput(chunk) {
  const out = document.getElementById('login-output');
  if (!out) return;
  const nearBottom = out.scrollHeight - out.scrollTop - out.clientHeight < 40;
  out.textContent += stripANSI(chunk);
  if (nearBottom) out.scrollTop = out.scrollHeight;
  renderLoginLinks();
}

const URL_RE = /https?:\/\/[^\s'")]+/g;
const ANSI_RE = /\x1b\[[0-9;?]*[a-zA-Z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[=>]|[\x00-\x08\x0b\x0c\x0e-\x1f]/g;

function stripANSI(s) {
  return (s || '').replace(ANSI_RE, '');
}

function renderLoginLinks() {
  const out = document.getElementById('login-output');
  if (!out) return;
  const text = out.textContent;
  const matches = [...text.matchAll(URL_RE)];
  const last = matches[matches.length - 1];
  const hint = document.getElementById('login-hint');
  const existing = document.getElementById('login-url-link');
  if (existing) existing.remove();
  if (last) {
    const a = document.createElement('a');
    a.id = 'login-url-link';
    a.href = last[0];
    a.target = '_blank';
    a.rel = 'noopener';
    a.className = 'login-url';
    a.textContent = 'Open authentication URL';
    hint.after(a);
  }
}

async function sendLoginKey(key) {
  const map = {
    up: '\x1b[A',
    down: '\x1b[B',
    enter: '\r',
    esc: '\x1b',
    'ctrl-c': '\x03',
  };
  const data = map[key];
  if (!data) return;
  try {
    await sendLoginInput(data);
  } catch (err) {
    toast(err.message, true);
  }
}

async function submitLoginInput() {
  const input = document.getElementById('login-input');
  const val = input.value;
  if (!val) return;
  try {
    await sendLoginInput(val + '\r');
    input.value = '';
  } catch (err) {
    toast(err.message, true);
  }
}

function confirmAction(msg, cb) {
  document.getElementById('confirm-msg').textContent = msg;
  confirmCallback = cb;
  openModal('confirm-modal');
}

// --- Toast ---

let toastTimeout;

function toast(msg, isError = false) {
  let el = document.querySelector('.toast');
  if (!el) {
    el = document.createElement('div');
    el.className = 'toast';
    document.body.appendChild(el);
  }
  el.textContent = msg;
  el.classList.toggle('error', isError);
  el.classList.add('show');
  clearTimeout(toastTimeout);
  toastTimeout = setTimeout(() => el.classList.remove('show'), 3000);
}

// --- Helpers ---

function timeAgo(dateStr) {
  const diff = (Date.now() - new Date(dateStr).getTime()) / 1000;
  if (diff < 5) return 'just now';
  if (diff < 60) return Math.floor(diff) + 's ago';
  if (diff < 3600) return Math.floor(diff / 60) + 'm ago';
  if (diff < 86400) return Math.floor(diff / 3600) + 'h ago';
  return Math.floor(diff / 86400) + 'd ago';
}

function shortenDir(dir) {
  const home = '/Users/' + (dir.split('/')[2] || '');
  if (dir.startsWith(home)) return '~' + dir.slice(home.length);
  return dir;
}


function esc(s) {
  if (!s) return '';
  const d = document.createElement('div');
  d.textContent = s;
  return d.innerHTML;
}

// Update time-ago every 30s
setInterval(() => {
  if (sessions.length > 0) render();
}, 30000);
