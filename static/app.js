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
let reviews = [];        // review metadata, newest first (no diff)
let activeReview = null; // full review (with diff segments) shown in the review screen
let gapState = {};       // segment index -> {up, down}: context lines revealed from each end
const GAP_CHUNK = 20;    // lines revealed per expand tap
let composerCtx = null;  // line/range target for the open comment composer; null = global
let selection = null;    // in-flight line-range selection: {startKey, endKey, commentId}

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
  await fetchReviewsList();
  connectSSE();
  bindEvents();
  bindReviewEvents();
  initPush();

  // Deep link from a push notification: /?review=<id>
  const wanted = new URLSearchParams(location.search).get('review');
  if (wanted) {
    history.replaceState(null, '', '/');
    openReviewScreen(wanted);
  }
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

async function changeSessionModel(id, modelId) {
  return api('/sessions/' + id + '/model', { method: 'PUT', body: JSON.stringify({ model: modelId }) });
}

function modelDisplayName(modelId) {
  if (!modelId) return 'default';
  const entry = getModelEntries().find(m => m.id === modelId);
  if (entry) return entry.family === 'aliases' ? entry.name : entry.name.replace('Claude ', '');
  return modelId;
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
let pendingModelSessionId = null;

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
  if (!pendingModelSessionId) {
    const emptyLabel = activeModelPickerTarget === 'session-model' ? 'Default' : 'None';
    parts.push(`<button type="button" class="picker-item" data-model-id=""><div class="picker-item-main"><span class="picker-item-name">${emptyLabel}</span></div></button>`);
  }

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

  const hiddenEl = document.getElementById(target + '-hidden');
  const currentId = hiddenEl ? hiddenEl.value : '';
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
  pendingModelSessionId = null;
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
    // init fires on every (re)connect. Reviews created while this client was
    // disconnected (phone locked, PWA suspended) aren't replayed by SSE, so
    // refetch them or they stay invisible until a full page reload.
    fetchReviewsList().then(render);
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

  eventSource.addEventListener('review_created', (e) => {
    const v = JSON.parse(e.data);
    upsertReview(v);
    const sess = sessions.find(s => s.id === v.sessionId);
    toast(`Review requested: ${baseName(v.filePath)}${sess ? ' · ' + sess.name : ''}`);
    render();
    syncReviewScreen(v);
  });

  eventSource.addEventListener('review_updated', (e) => {
    upsertReview(JSON.parse(e.data));
    render();
  });

  eventSource.addEventListener('review_decided', (e) => {
    const v = JSON.parse(e.data);
    upsertReview(v);
    render();
    syncReviewScreen(v);
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
  renderReviewBell();
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

    const pendingCount = reviews.filter(r => r.sessionId === s.id && r.status === 'pending').length;
    const reviewBadge = pendingCount > 0
      ? `<button class="review-badge" data-open-reviews="${s.id}" aria-label="Pending reviews">${pendingCount}</button>`
      : '';
    const reviewChip = s.reviewMode ? '<span class="review-mode-chip" title="Code review on">review</span>' : '';
    const reviewToggleBtn = isDead ? '' :
      `<button class="btn-review-toggle ${s.reviewMode ? 'on' : ''}" data-review-toggle="${s.id}">Review: ${s.reviewMode ? 'On' : 'Off'}</button>`;

    let actions = '';
    if (isExpanded && s.isChannels) {
      const openLink = s.url
        ? `<a href="${esc(s.url)}" target="_blank" rel="noopener" class="btn-open">Open in Claude</a>`
        : `<span class="btn-open" style="opacity:0.4;pointer-events:none">${isDead ? 'Restarting...' : 'Waiting for URL...'}</span>`;
      actions = `<div class="session-actions">
        ${openLink}
        ${reviewToggleBtn}
        <button class="btn-restart" data-restart-channels>Restart</button>
      </div>`;
    } else if (isExpanded && !isDead) {
      const openLink = s.url
        ? `<a href="${esc(s.url)}" target="_blank" rel="noopener" class="btn-open">Open in Claude</a>`
        : `<span class="btn-open" style="opacity:0.4;pointer-events:none">Waiting for URL...</span>`;
      actions = `<div class="session-actions">
        ${openLink}
        ${reviewToggleBtn}
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

    const modelRow = isExpanded && !isDead
      ? `<div class="session-model-row">
          <span class="model-label">Model</span>
          <button class="btn-model-change" data-change-model="${s.id}">${esc(modelDisplayName(s.model))}</button>
        </div>`
      : '';

    return `<div class="session-card ${isDead ? 'dead-card' : ''} ${isExpanded ? 'expanded' : ''}" data-id="${s.id}">
      <div class="session-header">
        <span class="status-dot ${statusClass}"></span>
        <span class="session-name">${esc(name)}</span>
        ${reviewChip}
        ${reviewBadge}
      </div>
      <div class="session-meta">
        <span>${esc(dir)}</span>
        <span class="sep">&middot;</span>
        <span>${time}</span>
      </div>
      ${modelRow}
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
  renderPushState();
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

  document.getElementById('push-enabled').addEventListener('change', async (e) => {
    const wantOn = e.target.checked;
    try {
      if (wantOn) {
        await enablePush();
        toast('Push notifications on');
      } else {
        await disablePush();
        toast('Push notifications off');
      }
    } catch (err) {
      e.target.checked = !wantOn;
      toast(err.message, true);
    }
    renderPushState();
  });

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
    const modelBtn = e.target.closest('[data-change-model]');
    if (modelBtn) {
      e.stopPropagation();
      pendingModelSessionId = modelBtn.dataset.changeModel;
      openModelPicker('live-model');
      return;
    }

    const reviewToggle = e.target.closest('[data-review-toggle]');
    if (reviewToggle) {
      e.stopPropagation();
      const id = reviewToggle.dataset.reviewToggle;
      const sess = sessions.find(s => s.id === id);
      const next = !(sess && sess.reviewMode);
      api('/sessions/' + id + '/review-mode', { method: 'POST', body: JSON.stringify({ enabled: next }) })
        .then(() => toast(next ? 'Code review on — file changes now wait for you' : 'Code review off'))
        .catch(err => toast(err.message, true));
      return;
    }

    const openReviews = e.target.closest('[data-open-reviews]');
    if (openReviews) {
      e.stopPropagation();
      openNextPendingReview(openReviews.dataset.openReviews);
      return;
    }

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
        reviewMode: document.getElementById('session-review').checked,
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
    const pickedId = item.dataset.modelId;

    if (pendingModelSessionId) {
      const sid = pendingModelSessionId;
      closeModelPicker();
      let actualId = pickedId;
      if (actualId) {
        changeSessionModel(sid, actualId)
          .then(() => toast('Model changed'))
          .catch(err => toast(err.message, true));
      }
      return;
    }

    selectModel(activeModelPickerTarget, pickedId);
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
  document.getElementById('session-review').checked = false;
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

// --- Code review ---

async function fetchReviewsList() {
  try {
    reviews = await api('/reviews') || [];
  } catch {
    reviews = [];
  }
}

function upsertReview(v) {
  const idx = reviews.findIndex(r => r.id === v.id);
  if (idx >= 0) reviews[idx] = v;
  else reviews.unshift(v);
}

function pendingReviews(sessionId) {
  return reviews
    .filter(r => r.status === 'pending' && (!sessionId || r.sessionId === sessionId))
    .sort((a, b) => a.seq - b.seq);
}

function baseName(p) {
  return (p || '').split('/').filter(Boolean).pop() || p;
}

function sessionName(id) {
  const s = sessions.find(x => x.id === id);
  return s ? s.name : '';
}

function renderReviewBell() {
  const bell = document.getElementById('review-bell');
  const count = pendingReviews().length;
  bell.hidden = count === 0;
  document.getElementById('review-bell-count').textContent = count;
}

function openNextPendingReview(sessionId) {
  const next = pendingReviews(sessionId)[0];
  if (next) openReviewScreen(next.id);
  else toast('No pending reviews');
}

// syncReviewScreen keeps an open review screen honest when SSE reports the
// displayed review changed (e.g. decided from another device) or new reviews
// arrive for the same session (queue counter). Updates in place — a refetch
// here would race the close-after-decide flow and reopen a closed screen.
function syncReviewScreen(v) {
  if (!activeReview || document.getElementById('review-screen').hidden) return;
  if (v.id === activeReview.id) {
    if (v.status !== activeReview.status) {
      activeReview.status = v.status;
      if (v.comments) activeReview.comments = v.comments;
      rerenderReviewBodyPreservingScroll();
      renderReviewFoot();
    }
    renderReviewNav();
  } else {
    renderReviewNav();
  }
}

async function openReviewScreen(id) {
  let full;
  try {
    full = await api('/reviews/' + id);
  } catch (err) {
    toast(err.message, true);
    return;
  }
  activeReview = full;
  gapState = {}; // expansions are per-open; don't carry across reviews
  selection = null; // never carry a line selection across reviews
  closeComposer();
  renderReviewScreen();
  const screen = document.getElementById('review-screen');
  screen.hidden = false;
  document.body.classList.add('modal-open');
  screen.querySelector('.review-body').scrollTop = 0;
}

function closeReviewScreen() {
  document.getElementById('review-screen').hidden = true;
  activeReview = null;
  selection = null;
  closeComposer();
  if (!document.querySelector('.modal.active')) {
    document.body.classList.remove('modal-open');
  }
}

function renderReviewScreen() {
  const r = activeReview;
  if (!r) return;

  document.getElementById('review-file').textContent = baseName(r.filePath);
  const verb = r.toolName === 'Write' ? (r.newFile ? 'create' : 'overwrite') : 'edit';
  document.getElementById('review-sub').innerHTML =
    `${esc(sessionName(r.sessionId) || r.sessionId)} · ${verb} · ` +
    `<span class="stat-add">+${r.adds}</span> <span class="stat-del">−${r.dels}</span>`;

  renderReviewNav();
  renderReviewBody();
  renderReviewFoot();
}

function renderReviewNav() {
  const nav = document.getElementById('review-nav');
  const r = activeReview;
  if (!r) { nav.hidden = true; return; }
  const queue = pendingReviews(r.sessionId);
  const idx = queue.findIndex(q => q.id === r.id);
  if (idx < 0 || queue.length < 2) {
    nav.hidden = true;
    return;
  }
  nav.hidden = false;
  document.getElementById('review-pos').textContent = `${idx + 1}/${queue.length}`;
  document.getElementById('review-prev').disabled = idx === 0;
  document.getElementById('review-next').disabled = idx === queue.length - 1;
}

function reviewLang(path) {
  const ext = (path.split('.').pop() || '').toLowerCase();
  const map = {
    go: 'go', js: 'javascript', mjs: 'javascript', cjs: 'javascript', jsx: 'javascript',
    ts: 'typescript', tsx: 'typescript', py: 'python', rb: 'ruby', rs: 'rust',
    c: 'c', h: 'c', cpp: 'cpp', cc: 'cpp', hpp: 'cpp', java: 'java', kt: 'kotlin',
    swift: 'swift', sh: 'bash', bash: 'bash', zsh: 'bash', yaml: 'yaml', yml: 'yaml',
    json: 'json', html: 'xml', xml: 'xml', css: 'css', scss: 'scss', md: 'markdown',
    sql: 'sql', tf: 'ini', toml: 'ini', ini: 'ini', php: 'php', lua: 'lua',
    dockerfile: 'dockerfile',
  };
  const lang = map[ext] || (baseName(path).toLowerCase() === 'dockerfile' ? 'dockerfile' : '');
  return (lang && window.hljs && hljs.getLanguage(lang)) ? lang : '';
}

function highlightLine(text, lang) {
  if (text === '') return '&nbsp;';
  if (lang && window.hljs) {
    try {
      return hljs.highlight(text, { language: lang, ignoreIllegals: true }).value;
    } catch { /* fall through */ }
  }
  return esc(text);
}

// commentKey anchors comments to diff rows: "g" for global, otherwise
// side:line of the commented row.
function commentKey(c) {
  return c.line ? `${c.side || 'new'}:${c.line}` : 'g';
}

function rowKey(row) {
  return row.t === 'del' ? `old:${row.o}` : `new:${row.n}`;
}

// --- line-range selection (select-then-say) ---
//
// A comment can span a contiguous run of rows. The reviewer builds that span by
// tapping: the first tap on a fresh row starts a one-row selection, a tap on the
// row just past either end grows it, and a tap on an endpoint shrinks it back.
// The span only becomes a comment on "Comment" (new) or is re-saved on "Save"
// (editing an existing comment's range). Adjacency and membership are defined
// over render order, not line numbers — line numbers aren't monotonic once adds
// and dels interleave.

function orderedRows() {
  const out = [];
  for (const seg of (activeReview && activeReview.segments) || []) {
    for (const row of seg.rows) out.push(row);
  }
  return out;
}

function rowIndex() {
  const m = new Map();
  orderedRows().forEach((row, i) => m.set(rowKey(row), i));
  return m;
}

function rowLineNo(row) { return row.t === 'del' ? row.o : row.n; }
function rowSideOf(row) { return row.t === 'del' ? 'old' : 'new'; }

// spanKeys returns the ordered row keys covered by [startKey..endKey], or [] if
// either endpoint isn't a currently-rendered row.
function spanKeys(startKey, endKey) {
  const rows = orderedRows();
  const idx = rowIndex();
  let a = idx.get(startKey), b = idx.get(endKey);
  if (a == null || b == null) return [];
  if (a > b) [a, b] = [b, a];
  const keys = [];
  for (let i = a; i <= b; i++) keys.push(rowKey(rows[i]));
  return keys;
}

// commentSpanStartKey derives a comment's start row key. The anchor line/side is
// the END; single-line comments have no start fields, so start == end.
function commentSpanStartKey(c) {
  if (!c.line) return null;
  return c.startLine ? `${c.startSide || 'new'}:${c.startLine}` : commentKey(c);
}

function commentAtKey(k) {
  for (const c of (activeReview && activeReview.comments) || []) {
    if (!c.line) continue;
    if (spanKeys(commentSpanStartKey(c), commentKey(c)).includes(k)) return c;
  }
  return null;
}

function handleRowTap(k) {
  if (!activeReview || activeReview.status !== 'pending') return;
  if (!selection) {
    const existing = commentAtKey(k);
    if (existing) {
      selection = { startKey: commentSpanStartKey(existing), endKey: commentKey(existing), commentId: existing.id };
    } else {
      selection = { startKey: k, endKey: k, commentId: null };
    }
  } else {
    adjustSelection(k);
  }
  updateSelectionBar();
  rerenderReviewBodyPreservingScroll();
}

// adjustSelection grows the selection when k is the row just past an end, shrinks
// it when k is an endpoint of a multi-row span, ignores interior taps, and — for
// a not-yet-saved selection — restarts at a fresh row when k is elsewhere.
function adjustSelection(k) {
  const rows = orderedRows();
  const idx = rowIndex();
  const ki = idx.get(k);
  let a = idx.get(selection.startKey), b = idx.get(selection.endKey);
  if (ki == null || a == null || b == null) return;
  if (a > b) [a, b] = [b, a];
  if (ki === a - 1) { selection.startKey = k; return; }                      // grow up
  if (ki === b + 1) { selection.endKey = k; return; }                        // grow down
  if (b > a && ki === a) { selection.startKey = rowKey(rows[a + 1]); return; } // shrink from top
  if (b > a && ki === b) { selection.endKey = rowKey(rows[b - 1]); return; }   // shrink from bottom
  if (ki > a && ki < b) return;                                              // interior: no-op
  if (selection.commentId == null) selection = { startKey: k, endKey: k, commentId: null };
}

// selectionRange resolves the selection into oriented endpoints carrying the line
// number, side and source text each end needs for the API and the label.
function selectionRange() {
  if (!selection) return null;
  const rows = orderedRows();
  const idx = rowIndex();
  let a = idx.get(selection.startKey), b = idx.get(selection.endKey);
  if (a == null || b == null) return null;
  if (a > b) [a, b] = [b, a];
  const start = rows[a], end = rows[b];
  return {
    single: a === b,
    startLine: rowLineNo(start), startSide: rowSideOf(start), startText: start.text,
    line: rowLineNo(end), side: rowSideOf(end), lineText: end.text,
  };
}

function rangeLabel(rng) {
  if (!rng) return '';
  if (rng.single) return `Line ${rng.line}`;
  if (rng.startSide === rng.side) return `Lines ${rng.startLine}–${rng.line}`;
  const s = sd => (sd === 'old' ? '−' : '+');
  return `Lines ${rng.startLine}${s(rng.startSide)}–${rng.line}${s(rng.side)}`;
}

// rangeBody is the API payload for a span; single-line selections omit the start
// fields so they persist exactly like a legacy single-line comment.
function rangeBody(rng) {
  const body = { line: rng.line, side: rng.side, lineText: rng.lineText };
  if (!rng.single) {
    body.startLine = rng.startLine;
    body.startSide = rng.startSide;
    body.startText = rng.startText;
  }
  return body;
}

function updateSelectionBar() {
  const bar = document.getElementById('selection-bar');
  if (!bar) return;
  if (!selection) { bar.hidden = true; return; }
  bar.hidden = false;
  document.getElementById('selection-label').textContent =
    (selection.commentId ? 'Range · ' : '') + rangeLabel(selectionRange());
  document.getElementById('selection-commit').textContent = selection.commentId ? 'Save' : 'Comment';
}

function clearSelection() {
  selection = null;
  updateSelectionBar();
}

function cancelSelection() {
  clearSelection();
  rerenderReviewBodyPreservingScroll();
}

// commitSelection turns the span into a comment: a new selection opens the
// composer for text; editing an existing comment's range PATCHes the span.
async function commitSelection() {
  const rng = selectionRange();
  if (!rng || !activeReview) return;
  if (!selection.commentId) {
    openComposer(rng);
    return;
  }
  try {
    const updated = await api('/reviews/' + activeReview.id + '/comments/' + selection.commentId, {
      method: 'PATCH', body: JSON.stringify(rangeBody(rng)),
    });
    activeReview.comments = (activeReview.comments || []).map(c => (c.id === updated.id ? updated : c));
    clearSelection();
    rerenderReviewBodyPreservingScroll();
  } catch (err) {
    toast(err.message, true);
  }
}

// buildSpanInfo maps each row key to its role in a saved comment's span: present
// ⇒ in-range, with start/end flags on the boundary rows. buildSelInfo does the
// same for the in-flight selection.
function buildSpanInfo(r) {
  const info = new Map();
  for (const c of r.comments || []) {
    if (!c.line) continue;
    tagSpan(info, spanKeys(commentSpanStartKey(c), commentKey(c)));
  }
  return info;
}

function buildSelInfo() {
  const info = new Map();
  if (selection) tagSpan(info, spanKeys(selection.startKey, selection.endKey));
  return info;
}

function tagSpan(info, keys) {
  keys.forEach((k, i) => {
    const e = info.get(k) || {};
    if (i === 0) e.start = true;
    if (i === keys.length - 1) e.end = true;
    info.set(k, e);
  });
}

function renderReviewBody() {
  const r = activeReview;
  const body = document.getElementById('review-body');
  const lang = reviewLang(r.filePath);
  const pending = r.status === 'pending';

  const byKey = new Map();
  for (const c of r.comments || []) {
    const k = commentKey(c);
    if (!byKey.has(k)) byKey.set(k, []);
    byKey.get(k).push(c);
  }
  const hl = { byKey, span: buildSpanInfo(r), sel: buildSelInfo() };

  const parts = [];

  const flags = [];
  if (r.newFile) flags.push('<span class="review-flag flag-new">new file</span>');
  if (r.notebook) flags.push('<span class="review-flag flag-nb">notebook cell — new source shown</span>');
  if (r.noMatch) flags.push('<span class="review-flag flag-warn">⚠️ edit pattern not found in the current file — claude\'s change will fail as-is</span>');
  if (r.tooLarge) flags.push('<span class="review-flag flag-warn">file too large to render a diff — approve or deny blind, or check it on disk</span>');
  if (flags.length) parts.push(`<div class="review-flags">${flags.join('')}</div>`);

  const globals = byKey.get('g') || [];
  if (globals.length) {
    parts.push(`<div class="global-comments">${globals.map(c => commentCard(c, pending)).join('')}</div>`);
  }

  const segments = r.segments || [];
  if (segments.length === 0 && !r.tooLarge) {
    parts.push('<div class="review-empty">No content changes to show.</div>');
  }

  // diff-wrap gives no-wrap mode one shared width (max-content), so row
  // backgrounds stay uniform bands while the body scrolls horizontally.
  if (segments.length) parts.push('<div class="diff-wrap">');
  segments.forEach((seg, si) => {
    if (seg.kind === 'gap') {
      // A gap's position decides which way it can expand: the first segment is
      // the top of the file, the last is the bottom, anything else is interior.
      parts.push(renderGap(seg, si, hl, pending, lang, si === 0, si === segments.length - 1));
      return;
    }
    parts.push('<div class="hunk">');
    for (const row of seg.rows) parts.push(renderRow(row, hl, pending, lang));
    parts.push('</div>');
  });
  if (segments.length) parts.push('</div>');

  body.innerHTML = parts.join('');
  applyWrapPref();
}

// renderRow builds one diff line (plus any attached comments). Shared by hunks
// and the context revealed when a gap is expanded.
function renderRow(row, hl, pending, lang) {
  const k = rowKey(row);
  const cls = ['drow', `t-${row.t}`];
  if (hl.byKey.has(k)) cls.push('commented');
  const span = hl.span.get(k);
  if (span) { cls.push('in-range'); if (span.start) cls.push('range-start'); if (span.end) cls.push('range-end'); }
  const sel = hl.sel.get(k);
  if (sel) { cls.push('selecting'); if (sel.start) cls.push('sel-start'); if (sel.end) cls.push('sel-end'); }
  if (pending) cls.push('tappable');
  let html =
    `<div class="${cls.join(' ')}" data-rk="${k}" data-line="${row.t === 'del' ? row.o : row.n}" data-side="${row.t === 'del' ? 'old' : 'new'}">` +
      `<span class="ln">${row.o || ''}</span>` +
      `<span class="ln">${row.n || ''}</span>` +
      `<span class="dsign">${row.t === 'add' ? '+' : row.t === 'del' ? '−' : ''}</span>` +
      `<code class="dcode">${highlightLine(row.text, lang)}</code>` +
    `</div>`;
  if (hl.byKey.has(k)) {
    html += `<div class="line-comments">${hl.byKey.get(k).map(c => commentCard(c, pending)).join('')}</div>`;
  }
  return html;
}

// renderGap renders a collapsed run of unchanged context: any rows already
// revealed from the top, a GitHub-style expander for whatever is still hidden,
// then any rows revealed from the bottom. Revealed lines are ordinary context
// rows, so they stay tappable for comments like any other line.
function renderGap(seg, si, hl, pending, lang, isTop, isBottom) {
  const rows = seg.rows;
  const st = gapState[si] || { top: 0, bottom: 0 };
  const top = Math.min(st.top, rows.length);
  const bottom = Math.min(st.bottom, rows.length - top);
  const hidden = rows.length - top - bottom;

  const out = [];
  for (let i = 0; i < top; i++) out.push(renderRow(rows[i], hl, pending, lang));
  if (hidden > 0) out.push(gapBar(si, hidden, isTop, isBottom));
  for (let i = rows.length - bottom; i < rows.length; i++) out.push(renderRow(rows[i], hl, pending, lang));
  return out.join('');
}

// gapBar is the expand control, modeled on GitHub's. A small gap collapses to a
// single "expand all" button; a larger one shows directional chevrons whose set
// depends on position — the top-of-file gap only expands up (toward line 1), the
// bottom-of-file gap only down (toward the end), and interior gaps both ways.
// The count, tapped, always reveals the whole gap at once.
function gapBar(si, hidden, isTop, isBottom) {
  const plural = hidden === 1 ? 'line' : 'lines';
  if (hidden <= GAP_CHUNK) {
    return `<div class="diff-gap">` +
      `<button class="gap-all" data-gap="${si}" data-dir="all" aria-label="Expand ${hidden} hidden ${plural}">` +
        `<span class="gap-ico">↕</span><span class="gap-text">Expand ${hidden} ${plural}</span>` +
      `</button></div>`;
  }
  const up = isBottom ? '' :
    `<button class="gap-chev" data-gap="${si}" data-dir="up" aria-label="Expand up">⌃</button>`;
  const down = isTop ? '' :
    `<button class="gap-chev" data-gap="${si}" data-dir="down" aria-label="Expand down">⌄</button>`;
  return `<div class="diff-gap">` +
    `<span class="gap-chevs">${up}${down}</span>` +
    `<button class="gap-all" data-gap="${si}" data-dir="all" aria-label="Expand all ${hidden} hidden ${plural}">` +
      `<span class="gap-text">${hidden} hidden ${plural}</span>` +
    `</button></div>`;
}

// expandGap peels more context into view. "down" reveals the top of the gap
// (context flowing down from the hunk above); "up" reveals the bottom (flowing
// up from the hunk below); "all" reveals the whole gap. State is keyed by
// segment index and reapplied on every render, so expansions survive the
// in-place re-renders triggered by comments and SSE updates.
function expandGap(si, dir) {
  const seg = ((activeReview && activeReview.segments) || [])[si];
  if (!seg) return;
  const len = seg.rows.length;
  const st = gapState[si] || { top: 0, bottom: 0 };
  if (dir === 'all') {
    st.top = len;
    st.bottom = 0;
  } else if (dir === 'down') {
    st.top = Math.min(len - st.bottom, st.top + GAP_CHUNK);
  } else if (dir === 'up') {
    st.bottom = Math.min(len - st.top, st.bottom + GAP_CHUNK);
  }
  gapState[si] = st;
  rerenderReviewBodyPreservingScroll();
}

// --- word wrap preference (cookie-persisted) ---

function wrapEnabled() {
  const m = document.cookie.match(/(?:^|;\s*)cp_wrap=(\d)/);
  return m ? m[1] === '1' : true; // wrap on by default
}

function setWrapEnabled(on) {
  document.cookie = 'cp_wrap=' + (on ? '1' : '0') + '; path=/; max-age=31536000; samesite=lax';
  applyWrapPref();
}

function applyWrapPref() {
  const on = wrapEnabled();
  document.getElementById('review-body').classList.toggle('nowrap', !on);
  const btn = document.getElementById('review-wrap');
  btn.setAttribute('aria-pressed', String(on));
  btn.classList.toggle('off', !on);
}

function commentCard(c, pending) {
  const edit = pending ? `<button class="comment-edit" data-edit-comment="${c.id}" aria-label="Edit comment">✎</button>` : '';
  const del = pending ? `<button class="comment-del" data-del-comment="${c.id}" aria-label="Delete comment">&times;</button>` : '';
  let label = '';
  if (!c.line) {
    label = '<span class="comment-scope">overall</span>';
  } else if (c.startLine && (c.startLine !== c.line || c.startSide !== c.side)) {
    label = `<span class="comment-scope">lines ${c.startLine}–${c.line}</span>`;
  }
  return `<div class="comment-card">${label}<p>${esc(c.text)}</p>${edit}${del}</div>`;
}

function renderReviewFoot() {
  const r = activeReview;
  const foot = document.getElementById('review-foot');
  const bar = document.getElementById('review-decided-bar');
  const pending = r.status === 'pending';
  foot.hidden = !pending;
  bar.hidden = pending;

  if (!pending) {
    const labels = {
      approved: '✓ Approved',
      denied: '✗ Denied — claude got your feedback',
      cancelled: 'Cancelled — the session ended',
    };
    bar.textContent = labels[r.status] || r.status;
    bar.className = 'review-decided-bar ' + r.status;
    return;
  }

  const n = (r.comments || []).length;
  const count = document.getElementById('review-comment-count');
  count.hidden = n === 0;
  count.textContent = n;
  const deny = document.getElementById('review-deny');
  deny.disabled = n === 0;
  deny.title = n === 0 ? 'Add at least one comment first' : '';
  document.getElementById('review-approve').textContent = n > 0 ? 'Approve w/ comments' : 'Approve';
}

// --- comment composer ---

function openComposer(ctx) {
  composerCtx = ctx;
  const editing = !!(ctx && ctx.commentId);
  const target = document.getElementById('composer-target');
  if (ctx && ctx.global) {
    target.textContent = editing ? 'Edit overall comment' : 'Overall comment';
  } else if (ctx) {
    const preview = ((ctx.single === false ? ctx.startText : ctx.lineText) || '').trim().slice(0, 80);
    target.innerHTML = `${editing ? 'Edit ' : ''}${rangeLabel(ctx)} <code>${esc(preview)}</code>`;
  } else {
    target.textContent = 'Overall comment';
  }
  document.getElementById('composer-overlay').hidden = false;
  const composer = document.getElementById('composer');
  composer.hidden = false;
  document.getElementById('composer-save').textContent = editing ? 'Save' : 'Comment';
  const text = document.getElementById('composer-text');
  text.value = editing ? ctx.text : '';
  setTimeout(() => text.focus(), 50);
}

// openComposerForEdit loads an existing comment's text into the composer. The
// anchor/range fields ride along only to render the header; saving PUTs the new
// text and leaves the anchor untouched.
function openComposerForEdit(c) {
  openComposer({
    commentId: c.id,
    text: c.text,
    global: !c.line,
    single: !(c.startLine && (c.startLine !== c.line || c.startSide !== c.side)),
    line: c.line, side: c.side, lineText: c.lineText,
    startLine: c.startLine, startSide: c.startSide, startText: c.startText,
  });
}

function closeComposer() {
  document.getElementById('composer').hidden = true;
  document.getElementById('composer-overlay').hidden = true;
  composerCtx = null;
}

async function saveComposerComment() {
  const text = document.getElementById('composer-text').value.trim();
  if (!text || !activeReview) return;
  try {
    if (composerCtx && composerCtx.commentId) {
      // Editing an existing comment's text — PUT leaves its anchor/range alone.
      const updated = await api('/reviews/' + activeReview.id + '/comments/' + composerCtx.commentId, {
        method: 'PUT', body: JSON.stringify({ text }),
      });
      activeReview.comments = (activeReview.comments || []).map(c => (c.id === updated.id ? updated : c));
    } else {
      const body = composerCtx ? { ...rangeBody(composerCtx), text } : { text };
      const c = await api('/reviews/' + activeReview.id + '/comments', { method: 'POST', body: JSON.stringify(body) });
      activeReview.comments = [...(activeReview.comments || []), c];
    }
    closeComposer();
    clearSelection();
    rerenderReviewBodyPreservingScroll();
    renderReviewFoot();
  } catch (err) {
    toast(err.message, true);
  }
}

function rerenderReviewBodyPreservingScroll() {
  const body = document.getElementById('review-body');
  const top = body.scrollTop;
  renderReviewBody();
  body.scrollTop = top;
}

async function decideReview(action) {
  if (!activeReview) return;
  const id = activeReview.id;
  const sessionId = activeReview.sessionId;
  try {
    await api('/reviews/' + id + '/decision', { method: 'POST', body: JSON.stringify({ action }) });
  } catch (err) {
    toast(err.message, true);
    return;
  }
  const idx = reviews.findIndex(r => r.id === id);
  if (idx >= 0) reviews[idx].status = action === 'approve' ? 'approved' : 'denied';
  render();
  toast(action === 'approve' ? 'Approved — claude is writing it' : 'Denied — claude got your comments');

  const next = pendingReviews(sessionId)[0];
  if (next) openReviewScreen(next.id);
  else closeReviewScreen();
}

function bindReviewEvents() {
  document.getElementById('review-bell').addEventListener('click', () => openNextPendingReview());
  document.getElementById('review-back').addEventListener('click', closeReviewScreen);
  document.getElementById('review-wrap').addEventListener('click', () => setWrapEnabled(!wrapEnabled()));
  document.getElementById('review-approve').addEventListener('click', () => decideReview('approve'));
  document.getElementById('review-deny').addEventListener('click', () => decideReview('deny'));
  document.getElementById('review-global-btn').addEventListener('click', () => openComposer(null));
  document.getElementById('composer-cancel').addEventListener('click', closeComposer);
  document.getElementById('composer-overlay').addEventListener('click', closeComposer);
  document.getElementById('composer-save').addEventListener('click', saveComposerComment);
  document.getElementById('selection-commit').addEventListener('click', commitSelection);
  document.getElementById('selection-cancel').addEventListener('click', cancelSelection);

  document.getElementById('review-prev').addEventListener('click', () => stepReview(-1));
  document.getElementById('review-next').addEventListener('click', () => stepReview(1));

  document.getElementById('review-body').addEventListener('click', (e) => {
    const editBtn = e.target.closest('[data-edit-comment]');
    if (editBtn && activeReview) {
      const c = (activeReview.comments || []).find(x => x.id === editBtn.dataset.editComment);
      if (c) openComposerForEdit(c);
      return;
    }
    const del = e.target.closest('[data-del-comment]');
    if (del && activeReview) {
      api('/reviews/' + activeReview.id + '/comments/' + del.dataset.delComment, { method: 'DELETE' })
        .then(() => {
          activeReview.comments = (activeReview.comments || []).filter(c => c.id !== del.dataset.delComment);
          rerenderReviewBodyPreservingScroll();
          renderReviewFoot();
        })
        .catch(err => toast(err.message, true));
      return;
    }
    const gapBtn = e.target.closest('.diff-gap button[data-gap]');
    if (gapBtn) {
      expandGap(parseInt(gapBtn.dataset.gap, 10), gapBtn.dataset.dir);
      return;
    }
    const row = e.target.closest('.drow.tappable');
    if (row) {
      handleRowTap(row.dataset.rk);
    }
  });

  // iOS suspends background PWAs: the EventSource dies silently and its
  // 3s reconnect timer doesn't run until the app is foregrounded again.
  // Refetch reviews immediately on resume so pending work shows up without
  // waiting on the reconnect (which then refreshes sessions via init).
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && appStarted) {
      fetchReviewsList().then(render);
    }
  });

  // Push notification taps land here when the app is already open.
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.addEventListener('message', (e) => {
      if (e.data && e.data.type === 'open-url') {
        const id = new URLSearchParams((e.data.url.split('?')[1] || '')).get('review');
        if (id) openReviewScreen(id);
      }
    });
  }
}

function stepReview(delta) {
  if (!activeReview) return;
  const queue = pendingReviews(activeReview.sessionId);
  const idx = queue.findIndex(q => q.id === activeReview.id);
  const next = queue[idx + delta];
  if (next) openReviewScreen(next.id);
}

// --- web push ---

function pushSupported() {
  return 'serviceWorker' in navigator && 'PushManager' in window && 'Notification' in window;
}

function isIOS() {
  return /iphone|ipad|ipod/i.test(navigator.userAgent);
}

function isStandalone() {
  return window.matchMedia('(display-mode: standalone)').matches || window.navigator.standalone === true;
}

async function initPush() {
  if (!pushSupported()) return;
  try {
    await navigator.serviceWorker.register('/sw.js');
  } catch (err) {
    console.warn('sw register failed', err);
  }
}

async function currentPushSubscription() {
  if (!pushSupported()) return null;
  const reg = await navigator.serviceWorker.getRegistration();
  if (!reg) return null;
  return reg.pushManager.getSubscription();
}

async function renderPushState() {
  const toggle = document.getElementById('push-enabled');
  const hint = document.getElementById('push-hint');
  const defaultHint = 'Get notified when a code review is waiting. Changes apply immediately — not on Save.';

  if (!pushSupported()) {
    toggle.checked = false;
    toggle.disabled = true;
    hint.textContent = isIOS() && !isStandalone()
      ? 'On iPhone: open the share menu → Add to Home Screen, then enable this from the installed app.'
      : 'Push notifications are not supported in this browser.';
    return;
  }
  toggle.disabled = false;
  hint.textContent = defaultHint;
  toggle.checked = !!(await currentPushSubscription());
}

function urlBase64ToUint8Array(base64) {
  const padding = '='.repeat((4 - (base64.length % 4)) % 4);
  const b64 = (base64 + padding).replace(/-/g, '+').replace(/_/g, '/');
  const raw = atob(b64);
  return Uint8Array.from([...raw].map(c => c.charCodeAt(0)));
}

async function enablePush() {
  const perm = await Notification.requestPermission();
  if (perm !== 'granted') throw new Error('Notifications were not allowed');
  const reg = await navigator.serviceWorker.register('/sw.js');
  await navigator.serviceWorker.ready;
  const { key } = await api('/push/key');
  const sub = await reg.pushManager.subscribe({
    userVisibleOnly: true,
    applicationServerKey: urlBase64ToUint8Array(key),
  });
  await api('/push/subscribe', { method: 'POST', body: JSON.stringify(sub.toJSON()) });
}

async function disablePush() {
  const sub = await currentPushSubscription();
  if (!sub) return;
  await api('/push/unsubscribe', { method: 'POST', body: JSON.stringify({ endpoint: sub.endpoint }) });
  await sub.unsubscribe();
}

// Update time-ago every 30s
setInterval(() => {
  if (sessions.length > 0) render();
}, 30000);
