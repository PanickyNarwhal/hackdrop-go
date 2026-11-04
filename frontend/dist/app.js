let currentState = {
  computerName: 'HackDrop-PC',
  visibilityMode: 'Everyone',
  autoAccept: false
};
let selectedFiles = [];
let activePromptId = null;
let isSavingSettings = false;

// Sidebar tab navigation
const tabTitles = {
  airdrop: 'AirDrop',
  transfers: 'Transfers',
  settings: 'Settings'
};

document.querySelectorAll('.nav-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    const tab = btn.dataset.tab;
    document.querySelectorAll('.nav-btn').forEach(b => b.classList.toggle('active', b === btn));
    document.querySelectorAll('.tab-view').forEach(v => v.classList.toggle('active', v.id === `tab-${tab}`));
    document.getElementById('page-title').textContent = tabTitles[tab] || 'AirDrop';
  });
});

// Toggle Direct IP input bar
document.getElementById('btn-toggle-direct-ip').addEventListener('click', () => {
  const bar = document.getElementById('direct-ip-bar');
  bar.classList.toggle('hidden');
  if (!bar.classList.contains('hidden')) {
    document.getElementById('peer-ip-input').focus();
  }
});

// Toggle Diagnostics in Settings
document.getElementById('btn-toggle-diagnostics').addEventListener('click', (e) => {
  const panel = document.getElementById('diagnostics-panel');
  const hidden = panel.classList.toggle('hidden');
  e.currentTarget.textContent = hidden
    ? 'Show Network & Radio Diagnostics'
    : 'Hide Network & Radio Diagnostics';
});

// Everyone / Off Segmented Controls (works on both Radar card and Settings tab)
document.querySelectorAll('[data-vis-group] .seg-btn').forEach(btn => {
  btn.addEventListener('click', async () => {
    const vis = btn.getAttribute('data-vis');
    currentState.visibilityMode = vis;
    applyVisibilityUI(vis);
    showToast(vis === 'Off' ? 'AirDrop receiving turned Off' : 'Discoverable to Everyone');

    await updateSettings({
      computerName: currentState.computerName || '',
      visibilityMode: vis,
      autoAccept: !!currentState.autoAccept
    });
  });
});

function applyVisibilityUI(vis) {
  const isOff = vis === 'Off';

  // Highlight active button in all Everyone/Off segmented groups
  document.querySelectorAll('[data-vis-group] .seg-btn').forEach(b => {
    b.classList.toggle('active', b.getAttribute('data-vis') === vis);
  });

  // Update radar rings & status dot
  const radarVisual = document.getElementById('radar-visual');
  if (radarVisual) {
    radarVisual.classList.toggle('off', isOff);
  }

  const radarDot = document.getElementById('radar-status-dot');
  if (radarDot) {
    radarDot.classList.toggle('green', !isOff);
  }

  const radarText = document.getElementById('radar-status-text');
  if (radarText) {
    radarText.textContent = isOff ? 'Receiving Off' : 'Discoverable to Everyone';
  }

  // Sidebar HTTPS dot stays green while server is running, or dims if Off
  const httpsDot = document.getElementById('https-status-dot');
  if (httpsDot) {
    httpsDot.classList.toggle('green', !isOff);
  }
}

// Auto-Accept switch
document.getElementById('auto-accept-btn').addEventListener('click', async () => {
  const nextVal = !currentState.autoAccept;
  currentState.autoAccept = nextVal;
  const autoBtn = document.getElementById('auto-accept-btn');
  autoBtn.classList.toggle('active', nextVal);
  autoBtn.setAttribute('aria-checked', String(nextVal));

  showToast(nextVal ? 'Auto-Accept enabled' : 'Auto-Accept disabled');
  await updateSettings({
    computerName: currentState.computerName || '',
    visibilityMode: currentState.visibilityMode || 'Everyone',
    autoAccept: nextVal
  });
});

// Save Device Name in Settings
document.getElementById('btn-save-settings').addEventListener('click', async () => {
  const name = document.getElementById('settings-computer-name').value.trim();
  if (!name) return;
  currentState.computerName = name;
  await updateSettings({
    computerName: name,
    visibilityMode: currentState.visibilityMode || 'Everyone',
    autoAccept: !!currentState.autoAccept
  });
  showToast('Saved device name');
});

async function updateSettings(payload) {
  isSavingSettings = true;
  try {
    const res = await fetch('/api/settings', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    });
    if (res.ok) {
      currentState = await res.json();
      renderState(currentState);
    }
  } finally {
    isSavingSettings = false;
  }
}

// Wake Nearby (BLE) & Scan Network
document.getElementById('btn-ble-pulse').addEventListener('click', async () => {
  const res = await fetch('/api/wakeup', { method: 'POST' });
  await res.json();
  showToast('Sent BLE wakeup broadcast');
  await fetchState();
});

document.getElementById('btn-subnet-scan').addEventListener('click', async () => {
  showToast('Scanning local network for AirDrop devices...');
  const res = await fetch('/api/scan-subnet', { method: 'POST' });
  const data = await res.json();
  showToast(`Network scan complete (${data.found || 0} found)`);
  await fetchState();
});

// File Selection & Drag-and-Drop
const fileInput = document.getElementById('file-input');
const dropzone = document.getElementById('dropzone');

fileInput.addEventListener('change', (e) => {
  selectedFiles = Array.from(e.target.files);
  renderSelectedFiles();
});

dropzone.addEventListener('dragover', (e) => {
  e.preventDefault();
});

dropzone.addEventListener('drop', (e) => {
  e.preventDefault();
  if (e.dataTransfer && e.dataTransfer.files.length > 0) {
    selectedFiles = Array.from(e.dataTransfer.files);
    renderSelectedFiles();
  }
});

function renderSelectedFiles() {
  const badge = document.getElementById('selected-count-badge');
  const list = document.getElementById('selected-files-list');
  badge.textContent = selectedFiles.length ? `${selectedFiles.length} file(s) ready` : 'No files selected';
  list.innerHTML = selectedFiles.map(f => `
    <div class="file-chip">
      <span>${escapeHTML(f.name)}</span>
      <span class="mono-text muted-text">${(f.size / 1024).toFixed(1)} KB</span>
    </div>
  `).join('');
}

// Direct IP Connect
document.getElementById('btn-confirm-add-peer').addEventListener('click', async () => {
  const ipAddr = document.getElementById('peer-ip-input').value.trim();
  const displayName = document.getElementById('peer-name-input').value.trim();
  if (!ipAddr) {
    showToast('Enter a device IP address first');
    return;
  }
  showToast(`Connecting to ${ipAddr}...`);
  const res = await fetch('/api/peer/add', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ displayName, modelName: '', ipAddr, port: 8770 })
  });
  const peer = await res.json();
  document.getElementById('direct-ip-bar').classList.add('hidden');
  showToast(`Added ${peer.displayName} (${ipAddr}:${peer.port})`);
  await fetchState();
});

// Send Files to Peer
async function sendToPeer(peerId, peerName) {
  if (selectedFiles.length === 0) {
    showToast('Select or drop files in the Send Files box first');
    return;
  }
  showToast(`Sending ${selectedFiles.length} file(s) to ${peerName}...`);
  const formData = new FormData();
  formData.append('peerId', peerId);
  selectedFiles.forEach(f => formData.append('files', f));

  const res = await fetch('/api/upload-temp', {
    method: 'POST',
    body: formData
  });
  if (res.ok) {
    showToast(`Delivered to ${peerName}`);
  } else {
    const errText = await res.text();
    showToast(`Failed: ${errText}`);
  }
  await fetchState();
}

// Incoming /Ask Modal
document.getElementById('btn-accept-ask').addEventListener('click', () => respondAsk(true));
document.getElementById('btn-decline-ask').addEventListener('click', () => respondAsk(false));

async function respondAsk(accept) {
  if (!activePromptId) return;
  const id = activePromptId;
  activePromptId = null;
  document.getElementById('ask-modal').classList.add('hidden');
  await fetch('/api/prompt', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ transferId: id, accept })
  });
  showToast(accept ? 'Accepted transfer' : 'Declined transfer');
  setTimeout(fetchState, 400);
}

async function fetchState() {
  if (isSavingSettings) return;
  try {
    const res = await fetch('/api/state');
    currentState = await res.json();
    renderState(currentState);
  } catch (e) {
    console.error(e);
  }
}

function renderState(st) {
  const isOff = st.visibilityMode === 'Off';
  document.getElementById('sidebar-port').textContent = isOff
    ? `HTTPS :${st.httpsPort} (Off)`
    : `HTTPS :${st.httpsPort} Active`;
  document.getElementById('sidebar-ip').textContent = st.localIpv4 || '127.0.0.1';

  document.getElementById('receive-node-name').textContent = st.computerName;
  document.getElementById('receive-ip-badge').textContent = `${st.localIpv4 || '127.0.0.1'}:${st.httpsPort} · ${st.modelName}`;

  document.getElementById('transfers-download-dir').textContent = st.downloadDir || '';
  document.getElementById('settings-download-dir').textContent = st.downloadDir || '';

  const settingsNameInput = document.getElementById('settings-computer-name');
  if (document.activeElement !== settingsNameInput) {
    settingsNameInput.value = st.computerName || '';
  }

  applyVisibilityUI(st.visibilityMode || 'Everyone');

  const autoBtn = document.getElementById('auto-accept-btn');
  autoBtn.classList.toggle('active', !!st.autoAccept);
  autoBtn.setAttribute('aria-checked', String(!!st.autoAccept));

  // Pending /Ask prompt
  const pendingEvent = (st.recentEvents || []).find(ev => ev.type === 'ask' && ev.prompt && ev.prompt.status === 'pending');
  if (pendingEvent && activePromptId !== pendingEvent.transferId) {
    activePromptId = pendingEvent.transferId;
    const p = pendingEvent.prompt;
    document.getElementById('ask-modal-desc').innerHTML =
      `<strong>${escapeHTML(p.senderComputerName)}</strong> would like to share ${p.files.length} file(s).`;
    document.getElementById('ask-modal-files').innerHTML = p.files.map(f =>
      `<div>${escapeHTML(f.fileName)}</div>`
    ).join('');
    document.getElementById('ask-modal').classList.remove('hidden');
  }

  // Render Nearby Devices
  const peers = st.discoveredPeers || [];
  document.getElementById('peers-count-label').textContent = `${peers.length} found`;
  const peersGrid = document.getElementById('peers-grid');

  if (peers.length === 0) {
    peersGrid.innerHTML = `<div class="card empty-state" style="grid-column: 1 / -1;">No nearby devices found yet. Click Scan Network or Add IP above.</div>`;
  } else {
    peersGrid.innerHTML = peers.map(p => {
      const initial = (p.displayName || 'D').trim().charAt(0).toUpperCase();
      return `
        <div class="peer-card">
          <div class="peer-card-left">
            <div class="peer-avatar">${escapeHTML(initial)}</div>
            <div style="min-width:0;">
              <div class="peer-name">${escapeHTML(p.displayName)}</div>
              <div class="peer-meta">${escapeHTML(p.ipv4Addr || p.ipv6Addr)}:${p.port}</div>
            </div>
          </div>
          <button type="button" class="mac-btn mac-btn-primary" onclick="sendToPeer('${escapeHTML(p.instanceId)}', '${escapeHTML(p.displayName)}')">
            Send
          </button>
        </div>
      `;
    }).join('');
  }

  // Render Transfers
  const evList = document.getElementById('events-list');
  if (st.recentEvents && st.recentEvents.length > 0) {
    evList.innerHTML = st.recentEvents.map(ev => {
      const timeStr = new Date(ev.timestamp).toLocaleTimeString();
      if (ev.type === 'complete') {
        const files = (ev.savedFiles || []).join(', ');
        return `
          <div class="mac-row">
            <div>
              <div class="row-title">${escapeHTML(ev.peerName)}</div>
              <div class="row-sub mono-text">${escapeHTML(files)}</div>
            </div>
            <span class="mono-text muted-text">${timeStr}</span>
          </div>
        `;
      }
      return `
        <div class="mac-row">
          <div>
            <div class="row-title">${escapeHTML(ev.peerName)}</div>
            <div class="row-sub">${escapeHTML(ev.type.toUpperCase())}</div>
          </div>
          <span class="mono-text muted-text">${timeStr}</span>
        </div>
      `;
    }).join('');
  }

  // Render Diagnostics (Transports + Packet Log)
  const tGrid = document.getElementById('transports-grid');
  if (tGrid) {
    tGrid.innerHTML = (st.transports || []).map(t => `
      <div class="transport-card">
        <div class="transport-title">${escapeHTML(t.mode)}</div>
        <div class="transport-kv">${escapeHTML(t.interfaceName)}</div>
      </div>
    `).join('');
  }

  const pktBody = document.getElementById('packet-log-body');
  if (pktBody) {
    pktBody.innerHTML = (st.recentPackets || []).slice(0, 20).map(pkt => `
      <tr>
        <td class="mono-text">${escapeHTML(pkt.timestamp)}</td>
        <td>${escapeHTML(pkt.layer)}</td>
        <td class="mono-text">${escapeHTML(pkt.direction)}</td>
        <td>${escapeHTML(pkt.summary)}</td>
      </tr>
    `).join('');
  }
}

function showToast(msg) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.classList.remove('hidden');
  clearTimeout(window.__toastTimer);
  window.__toastTimer = setTimeout(() => t.classList.add('hidden'), 2800);
}

function escapeHTML(str) {
  return String(str || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

fetchState();
setInterval(fetchState, 2000);
