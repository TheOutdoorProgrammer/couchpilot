let sessions = [];
let config = {};
let expandedId = null;
let confirmCallback = null;
let eventSource = null;

document.addEventListener('DOMContentLoaded', init);

async function init() {
  await fetchConfig();
  connectSSE();
  bindEvents();
}

// --- API ---

async function api(path, opts = {}) {
  const res = await fetch('/api' + path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  if (!res.ok && res.status !== 204) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  if (res.status === 204) return null;
  return res.json();
}

async function fetchConfig() {
  config = await api('/config');
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

async function saveConfig(data) {
  config = await api('/config', { method: 'PUT', body: JSON.stringify(data) });
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
    if (isExpanded && !isDead) {
      const openLink = s.url
        ? `<a href="${esc(s.url)}" target="_blank" rel="noopener" class="btn-open">Open in Claude</a>`
        : `<span class="btn-open" style="opacity:0.4;pointer-events:none">Waiting for URL...</span>`;
      actions = `<div class="session-actions">
        ${openLink}
        <button class="btn-kill" data-kill="${s.id}">Kill</button>
      </div>`;
    } else if (isExpanded && isDead) {
      actions = `<div class="session-actions">
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

function renderDirChips() {
  const container = document.getElementById('dir-chips');
  const input = document.getElementById('session-dir');
  container.innerHTML = (config.favoriteDirs || []).map(d =>
    `<span class="chip" data-dir="${esc(d)}">${esc(d)}</span>`
  ).join('');
}

function renderFavDirs() {
  const container = document.getElementById('fav-dirs');
  container.innerHTML = (config.favoriteDirs || []).map(d =>
    `<div class="fav-item">
      <span>${esc(d)}</span>
      <button class="fav-remove" data-remove-fav="${esc(d)}">&times;</button>
    </div>`
  ).join('');
}

// --- Events ---

function bindEvents() {
  document.getElementById('new-btn').addEventListener('click', () => {
    openNewModal();
  });

  document.getElementById('settings-btn').addEventListener('click', () => {
    renderFavDirs();
    document.getElementById('default-skip').checked = config.defaultSkipPermissions;
    openModal('settings-modal');
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

    const dismissBtn = e.target.closest('[data-dismiss-session]');
    if (dismissBtn) {
      e.stopPropagation();
      dismissSession(dismissBtn.dataset.dismissSession);
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

    try {
      await createSession({
        name: form.name.value.trim(),
        dir: form.dir.value.trim() || config.defaultDir,
        skipPerms: form.skipPerms.checked,
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

  document.getElementById('dir-chips').addEventListener('click', (e) => {
    const chip = e.target.closest('.chip');
    if (chip) {
      document.getElementById('session-dir').value = chip.dataset.dir;
      document.querySelectorAll('#dir-chips .chip').forEach(c => c.classList.remove('selected'));
      chip.classList.add('selected');
    }
  });

  document.getElementById('save-settings').addEventListener('click', async () => {
    try {
      await saveConfig({
        favoriteDirs: config.favoriteDirs,
        defaultSkipPermissions: document.getElementById('default-skip').checked,
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
    if (!config.favoriteDirs) config.favoriteDirs = [];
    if (!config.favoriteDirs.includes(val)) {
      config.favoriteDirs.push(val);
      renderFavDirs();
    }
    input.value = '';
  });

  document.getElementById('fav-dirs').addEventListener('click', (e) => {
    const btn = e.target.closest('[data-remove-fav]');
    if (btn) {
      config.favoriteDirs = config.favoriteDirs.filter(d => d !== btn.dataset.removeFav);
      renderFavDirs();
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
  document.getElementById('skip-perms').checked = config.defaultSkipPermissions;
  document.getElementById('session-dir').value = '';
  renderDirChips();
  openModal('new-modal');
}

function openModal(id) {
  document.getElementById(id).classList.add('active');
}

function closeModal(id) {
  document.getElementById(id).classList.remove('active');
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
