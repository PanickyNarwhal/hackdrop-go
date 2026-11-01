let currentState = null;
let selectedFiles = [];
let activePromptId = null;

// Tab switching
document.querySelectorAll('.nav-btn').forEach(btn => {
  btn.addEventListener('click', () => {
    document.querySelectorAll('.nav-btn').forEach(b => b.classList.remove('active'));
    document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
    btn.classList.add('active');
    const tabId = 'tab-' + btn.dataset.tab;
    document.getElementById(tabId).classList.add('active');
  });
});

// Visibility selector (Everyone / Off)
document.querySelectorAll('#visibility-segmented .seg-btn').forEach(btn => {
  btn.addEventListener('click', async () => {
    const vis = btn.dataset.vis;
    await updateSettings({
      computerName: currentState?.computerName || '',
      visibilityMode: vis,
      autoAccept: currentState?.autoAccept || false
    });
    showToast(`Visibility set to "${vis}"`);
  });
});

// Auto-Accept toggle
document.getElementById('auto-accept-btn').addEventListener('click', async () => {
  const nextVal = !(currentState?.autoAccept);
  await updateSettings({
    computerName: currentState?.computerName || '',
    visibilityMode: currentState?.visibilityMode || 'Everyone',
    autoAccept: nextVal
  });
  showToast(nextVal ? 'Quick Save (Auto-Accept) Enabled' : 'Quick Save Disabled (Ask Every Time)');
});

// BLE Pulse & Subnet Scan buttons
document.getElementById('btn-ble-pulse').addEventListener('click', triggerBLEPulse);
document.getElementById('btn-subnet-scan').addEventListener('click', triggerSubnetScan);
document.getElementById('btn-scan-refresh').addEventListener('click', triggerSubnetScan);

async function triggerBLEPulse() {
  const res = await fetch('/api/wakeup', { method: 'POST' });
  const data = await res.json();
  showToast(`Broadcasted BLE 0x4c00 Frame (${data.bleFrameHex.slice(0, 16)}...)`);
  await fetchState();
}

async function triggerSubnetScan() {
  showToast('Scanning local Wi-Fi subnet on ports 8770 & 8771...');
  const res = await fetch('/api/scan-subnet', { method: 'POST' });
  const data = await res.json();
  showToast(`Subnet scan finished (${data.found || 0} active host(s) responded to /Discover)`);
  await fetchState();
}

// File input handling
const fileInput = document.getElementById('file-input');
fileInput.addEventListener('change', (e) => {
  selectedFiles = Array.from(e.target.files);
  renderSelectedFiles();
});

function renderSelectedFiles() {
  const badge = document.getElementById('selected-count-badge');
  const list = document.getElementById('selected-files-list');
  badge.textContent = `${selectedFiles.length} file(s)`;
  list.innerHTML = selectedFiles.map(f => `
    <div class="file-chip">
      <span>${escapeHTML(f.name)}</span>
      <span class="muted">${(f.size / 1024).toFixed(1)} KB</span>
    </div>
  `).join('');
}

// Direct IP Connect
document.getElementById('btn-confirm-add-peer').addEventListener('click', async () => {
  const ipAddr = document.getElementById('peer-ip-input').value.trim();
  const displayName = document.getElementById('peer-name-input').value.trim();
  if (!ipAddr) {
    showToast('Enter an IPv4 or fe80:: IPv6 address first');
    return;
  }
  showToast(`Probing https://${ipAddr}:8770/Discover ...`);
  const res = await fetch('/api/peer/add', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ displayName, modelName: '', ipAddr, port: 8770 })
  });
  const peer = await res.json();
  showToast(`Added ${peer.displayName} (${ipAddr}:${peer.port})`);
  await fetchState();
});

// Send files to peer
async function sendToPeer(peerId, peerName) {
  if (selectedFiles.length === 0) {
    showToast('Select at least one file in Step 1 first');
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
    showToast(`Transfer to ${peerName} completed`);
  } else {
    const errText = await res.text();
    showToast(`Transfer failed: ${errText}`);
  }
  await fetchState();
}

// Incoming /Ask Modal Accept / Decline
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
  showToast(accept ? 'Accepted incoming transfer — extracting CPIO stream...' : 'Declined transfer request');
  setTimeout(fetchState, 500);
}

// Save settings
document.getElementById('btn-save-settings').addEventListener('click', async () => {
  const name = document.getElementById('settings-computer-name').value.trim();
  await updateSettings({
    computerName: name,
    visibilityMode: currentState?.visibilityMode || 'Everyone',
    autoAccept: currentState?.autoAccept || false
  });
  showToast('Saved node settings');
});

async function updateSettings(payload) {
  const res = await fetch('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  currentState = await res.json();
  renderState(currentState);
}

async function fetchState() {
  try {
    const res = await fetch('/api/state');
    currentState = await res.json();
    renderState(currentState);
  } catch (e) {
    console.error(e);
  }
}

function renderState(st) {
  document.getElementById('sidebar-os').textContent = st.os;
  document.getElementById('sidebar-port').textContent = `HTTPS :${st.httpsPort} Active`;
  document.getElementById('sidebar-device-id').textContent = `ID: ${st.deviceId}`;

  document.getElementById('receive-node-name').textContent = st.computerName;
  document.getElementById('receive-model-badge').textContent = st.modelName;
  document.getElementById('receive-ip-badge').textContent = `${st.localIpv4 || '127.0.0.1'}:${st.httpsPort}`;
  document.getElementById('receive-id-badge').textContent = `#${st.deviceId}`;

  const maskedDir = (st.downloadDir || '').replace(/([\\/])Users\1[^\\/]+/gi, '$1Users$1BlackMesa');
  document.getElementById('download-dir-label').textContent = maskedDir;
  document.getElementById('ble-frame-badge').textContent = `BLE 0x4c00: ${st.bleFrameHex}`;

  const settingsNameInput = document.getElementById('settings-computer-name');
  if (document.activeElement !== settingsNameInput) {
    settingsNameInput.value = st.computerName;
  }
  document.getElementById('settings-download-dir').value = maskedDir;

  // Visibility segmented buttons
  document.querySelectorAll('#visibility-segmented .seg-btn').forEach(btn => {
    btn.classList.toggle('active', btn.dataset.vis === st.visibilityMode);
  });

  // Auto-accept button
  const autoBtn = document.getElementById('auto-accept-btn');
  autoBtn.classList.toggle('active', st.autoAccept);
  autoBtn.textContent = st.autoAccept ? 'On (Auto-Save to Downloads)' : 'Off (Ask Every Time)';

  // Check for pending /Ask prompt
  const pendingEvent = (st.recentEvents || []).find(ev => ev.type === 'ask' && ev.prompt && ev.prompt.status === 'pending');
  if (pendingEvent && activePromptId !== pendingEvent.transferId) {
    activePromptId = pendingEvent.transferId;
    const p = pendingEvent.prompt;
    document.getElementById('ask-modal-desc').innerHTML =
      `<strong>${escapeHTML(p.senderComputerName)}</strong> (${escapeHTML(p.senderModelName)}) wants to send ${p.files.length} file(s):`;
    document.getElementById('ask-modal-files').innerHTML = p.files.map(f =>
      `<div><strong>${escapeHTML(f.fileName)}</strong> <span class="muted">(${escapeHTML(f.fileType)})</span></div>`
    ).join('');
    document.getElementById('ask-modal').classList.remove('hidden');
  }

  // Render recent transfer events
  const evList = document.getElementById('events-list');
  if (st.recentEvents && st.recentEvents.length > 0) {
    evList.innerHTML = st.recentEvents.map(ev => {
      if (ev.type === 'complete') {
        const files = (ev.savedFiles || []).join(', ').replace(/([\\/])Users\1[^\\/]+/gi, '$1Users$1BlackMesa');
        return `<div class="event-row">
          <span><span class="event-tag">SAVED</span><strong>Transfer Complete</strong> (${escapeHTML(ev.peerName)}) — <code>${escapeHTML(files)}</code></span>
          <span class="muted">${new Date(ev.timestamp).toLocaleTimeString()}</span>
        </div>`;
      }
      if (ev.type === 'discover') {
        return `<div class="event-row">
          <span><span class="event-tag">DISCOVER</span><strong>/Discover Handshake</strong> from ${escapeHTML(ev.peerName)}</span>
          <span class="muted">${new Date(ev.timestamp).toLocaleTimeString()}</span>
        </div>`;
      }
      if (ev.type === 'ask') {
        return `<div class="event-row">
          <span><span class="event-tag">ASK</span><strong>/Ask Prompt</strong> from ${escapeHTML(ev.peerName)}</span>
          <span class="muted">${new Date(ev.timestamp).toLocaleTimeString()}</span>
        </div>`;
      }
      return `<div class="event-row">
        <span><span class="event-tag">${escapeHTML(ev.type)}</span>Peer ${escapeHTML(ev.peerName)}</span>
        <span class="muted">${new Date(ev.timestamp).toLocaleTimeString()}</span>
      </div>`;
    }).join('');
  }

  // Render peers grid
  const peersGrid = document.getElementById('peers-grid');
  const peers = st.discoveredPeers || [];
  if (peers.length === 0) {
    peersGrid.innerHTML = `<div class="empty-hint">No peers listed yet. Type your MacBook or PC IP above and click <strong>Connect IP</strong>, or click <strong>Scan Subnet &amp; Pulse</strong>.</div>`;
  } else {
    peersGrid.innerHTML = peers.map(p => `
      <div class="peer-card">
        <div class="peer-top">
          <div class="peer-avatar">NODE</div>
          <div>
            <div class="peer-title">${escapeHTML(p.displayName)}</div>
            <div class="peer-meta">${escapeHTML(p.modelName)} • ${escapeHTML(p.ipv4Addr || p.ipv6Addr)}:${p.port}</div>
          </div>
        </div>
        <div class="badge badge-accent">${escapeHTML(p.transport)}</div>
        <button class="btn btn-primary btn-sm" onclick="sendToPeer('${escapeHTML(p.instanceId)}', '${escapeHTML(p.displayName)}')">
          Send Selected Files
        </button>
      </div>
    `).join('');
  }

  // Render P2P Transports
  const tGrid = document.getElementById('transports-grid');
  tGrid.innerHTML = (st.transports || []).map(t => `
    <div class="transport-card">
      <div class="transport-title">${escapeHTML(t.mode)}</div>
      <div class="transport-kv">Backend: ${escapeHTML(t.driverBackend)}</div>
      <div class="transport-kv">Interface: ${escapeHTML(t.interfaceName)} (${escapeHTML(t.ipv6LinkLocal)})</div>
      ${t.currentChannel ? `<div class="transport-kv">Social Channel: Ch ${t.currentChannel}</div>` : ''}
      <div class="transport-detail">${escapeHTML(t.detail)}</div>
    </div>
  `).join('');

  // Render Packet Inspector
  const pktBody = document.getElementById('packet-log-body');
  pktBody.innerHTML = (st.recentPackets || []).map(pkt => `
    <tr>
      <td>${escapeHTML(pkt.timestamp)}</td>
      <td><span class="badge">${escapeHTML(pkt.layer)}</span></td>
      <td><strong>${escapeHTML(pkt.direction)}</strong></td>
      <td>${escapeHTML(pkt.summary)}</td>
      <td class="hex-cell">${escapeHTML(pkt.hexBytes)}</td>
    </tr>
  `).join('');
}

function showToast(msg) {
  const t = document.getElementById('toast');
  t.textContent = msg;
  t.classList.remove('hidden');
  clearTimeout(window.__toastTimer);
  window.__toastTimer = setTimeout(() => t.classList.add('hidden'), 3200);
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
