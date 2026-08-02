(() => {
  const state = { library: null, admin: null, csrf: "", deferredInstall: null, online: navigator.onLine };
  const $ = selector => document.querySelector(selector);

  function fileURL(file) {
    return `/content/${encodeURIComponent(file.id)}/${encodeURIComponent(file.name)}?v=${encodeURIComponent(file.sha256)}`;
  }

  function formatBytes(bytes) {
    if (bytes < 1024) return `${bytes} B`;
    if (bytes < 1024 ** 2) return `${(bytes / 1024).toFixed(1)} KB`;
    return `${(bytes / 1024 ** 2).toFixed(1)} MB`;
  }

  function formatDate(value) {
    return value ? new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value)) : "Never";
  }

  function toast(message) {
    const element = $("#toast");
    element.textContent = message;
    element.hidden = false;
    clearTimeout(toast.timer);
    toast.timer = setTimeout(() => { element.hidden = true; }, 4500);
  }

  async function fetchJSON(url, options = {}) {
    const headers = { ...(options.headers || {}) };
    if (state.csrf && options.method && options.method !== "GET") headers["X-CSRF-Token"] = state.csrf;
    if (options.body && !(options.body instanceof FormData)) headers["Content-Type"] = "application/json";
    const response = await fetch(url, { credentials: "same-origin", ...options, headers });
    const body = await response.json().catch(() => ({}));
    if (!response.ok && response.status !== 207) throw new Error(body.error || "The request could not be completed.");
    return body;
  }

  function renderFiles() {
    const grid = $("#file-grid");
    const files = state.library?.files || [];
    grid.replaceChildren();
    $("#empty-state").hidden = files.length !== 0;
    $("#file-summary").textContent = files.length === 1 ? "1 file" : `${files.length} files`;
    for (const file of files) {
      const card = document.createElement("article");
      card.className = "file-card";
      const link = document.createElement("a");
      link.href = fileURL(file);
      const icon = document.createElement("div");
      icon.className = "file-icon";
      icon.textContent = file.name.toLowerCase().endsWith(".html") ? "HTML" : "FILE";
      const name = document.createElement("h3");
      name.className = "file-name";
      name.textContent = file.name;
      const meta = document.createElement("p");
      meta.className = "file-meta";
      meta.textContent = `${formatBytes(file.size)} · Updated ${formatDate(file.updatedAt)}`;
      link.append(icon, name, meta);
      const actions = document.createElement("div");
      actions.className = "file-actions";
      const rename = document.createElement("button");
      rename.type = "button";
      rename.className = "secondary";
      rename.dataset.write = "";
      rename.textContent = "Rename";
      rename.addEventListener("click", () => renameFile(file));
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "secondary";
      remove.dataset.write = "";
      remove.textContent = "Delete";
      remove.addEventListener("click", () => deleteFile(file));
      actions.append(rename, remove);
      card.append(link, actions);
      grid.append(card);
    }
    updateConnectivity();
  }

  async function loadLibrary() {
    state.library = await fetchJSON("/api/library");
    state.csrf = state.library.csrfToken;
    $("#site-name").value = state.library.siteName;
    renderFiles();
    await synchronizeOffline();
  }

  async function loadAdmin() {
    state.admin = await fetchJSON("/api/admin");
    state.csrf = state.admin.csrfToken;
    renderOwners();
  }

  async function uploadFiles(files) {
    if (!state.online || !files.length) return;
    const data = new FormData();
    for (const file of files) data.append("files", file, file.name);
    toast(`Uploading ${files.length === 1 ? files[0].name : `${files.length} files`}…`);
    const result = await fetchJSON("/api/files", { method: "POST", body: data });
    const failures = result.results.filter(item => item.error);
    const replacements = result.results.filter(item => item.replaced).length;
    await loadLibrary();
    if (failures.length) toast(`${failures.length} upload${failures.length === 1 ? "" : "s"} failed: ${failures[0].error}`);
    else toast(`${result.results.length} file${result.results.length === 1 ? "" : "s"} uploaded${replacements ? `, ${replacements} replaced` : ""}.`);
  }

  async function renameFile(file) {
    const name = prompt("New filename", file.name);
    if (!name || name === file.name) return;
    try {
      await fetchJSON(`/api/files/${encodeURIComponent(file.id)}/rename`, { method: "POST", body: JSON.stringify({ name }) });
      await loadLibrary();
      toast("File renamed.");
    } catch (error) { toast(error.message); }
  }

  async function deleteFile(file) {
    if (!confirm(`Delete ${file.name}?`)) return;
    try {
      await fetchJSON(`/api/files/${encodeURIComponent(file.id)}`, { method: "DELETE" });
      await loadLibrary();
      toast("File deleted.");
    } catch (error) { toast(error.message); }
  }

  function renderOwners() {
    const list = $("#owner-list");
    list.replaceChildren();
    for (const owner of state.admin?.owners || []) {
      const card = document.createElement("article");
      card.className = "owner-card";
      const heading = document.createElement("div");
      heading.className = "owner-title";
      const title = document.createElement("strong");
      title.textContent = `${owner.personName} · ${owner.passkeyName}`;
      heading.append(title);
      if (owner.current) {
        const current = document.createElement("span");
        current.className = "current-badge";
        current.textContent = "This passkey";
        heading.append(current);
      }
      const metadata = document.createElement("div");
      metadata.className = "owner-meta";
      const rows = [
        ["Registered", formatDate(owner.createdAt)], ["Last used", formatDate(owner.lastUsedAt)],
        ["AAGUID", owner.aaguid || "Unknown"], ["Attachment", owner.attachment || "Unknown"],
        ["Transports", owner.transports?.join(", ") || "Unknown"], ["Synced", owner.backupEligible ? (owner.backupState ? "Yes" : "Eligible") : "No"],
        ["Sign count", String(owner.signCount)], ["Credential ID", owner.credentialId],
        ["Registered from", owner.userAgent || "Unknown"],
      ];
      for (const [label, value] of rows) {
        const row = document.createElement("span");
        row.textContent = `${label}: ${value}`;
        metadata.append(row);
      }
      const actions = document.createElement("div");
      actions.className = "owner-actions";
      const rename = document.createElement("button");
      rename.type = "button"; rename.className = "secondary"; rename.dataset.write = ""; rename.textContent = "Rename passkey";
      rename.addEventListener("click", () => renamePasskey(owner));
      actions.append(rename);
      if (!owner.current) {
        const remove = document.createElement("button");
        remove.type = "button"; remove.className = "danger"; remove.dataset.write = ""; remove.textContent = "Delete owner";
        remove.addEventListener("click", () => deleteOwner(owner));
        actions.append(remove);
      }
      card.append(heading, metadata, actions);
      list.append(card);
    }
    updateConnectivity();
  }

  async function renamePasskey(owner) {
    const name = prompt("Passkey name", owner.passkeyName);
    if (!name || name === owner.passkeyName) return;
    try {
      await fetchJSON(`/api/owners/${encodeURIComponent(owner.id)}/rename`, { method: "POST", body: JSON.stringify({ name }) });
      await loadAdmin();
      toast("Passkey renamed.");
    } catch (error) { toast(error.message); }
  }

  async function deleteOwner(owner) {
    if (!confirm(`Delete ${owner.personName} · ${owner.passkeyName}? Their sessions will stop immediately.`)) return;
    try {
      await fetchJSON(`/api/owners/${encodeURIComponent(owner.id)}`, { method: "DELETE" });
      await loadAdmin();
      toast("Owner deleted.");
    } catch (error) { toast(error.message); }
  }

  async function synchronizeOffline() {
    if (!("serviceWorker" in navigator) || !state.online) return;
    const status = $("#offline-status");
    status.textContent = "Preparing offline copy…";
    status.className = "offline-status";
    try {
      const registration = await navigator.serviceWorker.register("/sw.js", { scope: "/" });
      await navigator.serviceWorker.ready;
      const worker = registration.active || registration.waiting || registration.installing;
      const channel = new MessageChannel();
      const result = await new Promise((resolve, reject) => {
        const timeout = setTimeout(() => reject(new Error("Offline synchronization timed out.")), 60000);
        channel.port1.onmessage = event => { clearTimeout(timeout); resolve(event.data); };
        worker.postMessage({ type: "SYNC_PRIVATE", urls: state.library.files.map(fileURL) }, [channel.port2]);
      });
      if (result.ok) {
        status.textContent = "Ready offline";
        status.className = "offline-status ready";
      } else throw new Error(result.error || "Some files could not be cached.");
    } catch (error) {
      status.textContent = "Offline copy incomplete";
      status.className = "offline-status incomplete";
      status.title = error.message;
    }
  }

  function updateConnectivity() {
    const online = state.online;
    const indicator = $("#connectivity");
    indicator.textContent = online ? "Online" : "Offline · read only";
    indicator.classList.toggle("offline", !online);
    for (const control of document.querySelectorAll("[data-write]")) control.disabled = !online;
  }

  async function refreshConnectivity() {
    try {
      const response = await fetch("/healthz", { cache: "no-store", credentials: "same-origin" });
      state.online = response.ok;
    } catch {
      state.online = false;
    }
    updateConnectivity();
    if (!state.online && state.library) {
      const status = $("#offline-status");
      status.textContent = "Using offline copy";
      status.className = "offline-status ready";
    }
    return state.online;
  }

  $("#choose-files").addEventListener("click", () => $("#file-input").click());
  $("#file-input").addEventListener("change", event => uploadFiles([...event.target.files]).catch(error => toast(error.message)));
  const dropZone = $("#drop-zone");
  for (const type of ["dragenter", "dragover"]) dropZone.addEventListener(type, event => { event.preventDefault(); dropZone.classList.add("dragging"); });
  for (const type of ["dragleave", "drop"]) dropZone.addEventListener(type, event => { event.preventDefault(); dropZone.classList.remove("dragging"); });
  dropZone.addEventListener("drop", event => uploadFiles([...event.dataTransfer.files]).catch(error => toast(error.message)));

  $("#settings-button").addEventListener("click", async () => {
    $("#settings-dialog").showModal();
    if (state.online) {
      try { await loadAdmin(); } catch (error) { toast(error.message); }
    }
  });
  $("#site-form").addEventListener("submit", async event => {
    event.preventDefault();
    try {
      await fetchJSON("/api/site", { method: "POST", body: JSON.stringify({ name: $("#site-name").value }) });
      location.reload();
    } catch (error) { toast(error.message); }
  });
  $("#invite-button").addEventListener("click", async () => {
    try {
      const invite = await fetchJSON("/api/invites", { method: "POST", body: "{}" });
      const result = $("#invite-result");
      result.replaceChildren(); result.hidden = false;
      const text = document.createElement("div");
      text.textContent = `Expires ${formatDate(invite.expiresAt)} · ${invite.url}`;
      const copy = document.createElement("button");
      copy.type = "button"; copy.className = "secondary"; copy.textContent = "Copy invite link";
      copy.addEventListener("click", async () => { await navigator.clipboard.writeText(invite.url); toast("Invite link copied."); });
      result.append(text, copy);
    } catch (error) { toast(error.message); }
  });
  $("#logout-button").addEventListener("click", async () => {
    if ("serviceWorker" in navigator) {
      const registration = await navigator.serviceWorker.getRegistration("/").catch(() => null);
      if (registration?.active) {
        const channel = new MessageChannel();
        const cleared = new Promise(resolve => {
          const timeout = setTimeout(resolve, 3000);
          channel.port1.onmessage = () => { clearTimeout(timeout); resolve(); };
        });
        registration.active.postMessage({ type: "CLEAR_PRIVATE" }, [channel.port2]);
        await cleared;
      }
    }
    try { await fetchJSON("/auth/logout", { method: "POST", body: "{}" }); } finally { location.assign("/"); }
  });

  addEventListener("online", () => { refreshConnectivity().then(online => { if (online) loadLibrary().catch(error => toast(error.message)); }); });
  addEventListener("offline", () => { state.online = false; updateConnectivity(); });
  addEventListener("beforeinstallprompt", event => { event.preventDefault(); state.deferredInstall = event; $("#install-button").hidden = false; });
  $("#install-button").addEventListener("click", async () => { await state.deferredInstall?.prompt(); state.deferredInstall = null; $("#install-button").hidden = true; });

  updateConnectivity();
  refreshConnectivity().then(online => Promise.all([loadLibrary(), online ? loadAdmin() : Promise.resolve()])).catch(error => toast(error.message));
})();
