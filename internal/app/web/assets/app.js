(() => {
  const state = { library: null, admin: null, csrf: "", deferredInstall: null, online: navigator.onLine, query: "", sort: "name" };
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

  function normalizeSearch(value) {
    return value.normalize("NFKC").toLocaleLowerCase().replace(/[\s._-]+/g, " ").trim();
  }

  function libraryFiles() {
    return Array.isArray(state.library?.files) ? state.library.files : [];
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
    const list = $("#file-list");
    const allFiles = libraryFiles();
    const query = normalizeSearch(state.query);
    const files = allFiles
      .filter(file => !query || normalizeSearch(file.name).includes(query))
      .sort((left, right) => {
        if (state.sort === "updated") {
          const newestFirst = new Date(right.updatedAt).getTime() - new Date(left.updatedAt).getTime();
          if (newestFirst) return newestFirst;
        }
        return left.name.localeCompare(right.name, undefined, { numeric: true, sensitivity: "base" });
      });
    list.replaceChildren();
    $("#empty-state").hidden = allFiles.length !== 0;
    $("#search-empty-state").hidden = allFiles.length === 0 || files.length !== 0;
    $("#file-table-wrap").hidden = files.length === 0;
    $("#file-search").disabled = allFiles.length === 0;
    $("#file-sort").disabled = allFiles.length < 2;
    $("#file-summary").textContent = query
      ? `${files.length} of ${allFiles.length} ${allFiles.length === 1 ? "file" : "files"}`
      : (allFiles.length === 1 ? "1 file" : `${allFiles.length} files`);
    for (const file of files) {
      const row = document.createElement("tr");
      row.className = "file-row";
      const nameCell = document.createElement("td");
      const link = document.createElement("a");
      link.className = "file-link";
      link.href = fileURL(file);
      link.textContent = file.name;
      nameCell.append(link);
      const updated = document.createElement("td");
      updated.className = "file-updated";
      const time = document.createElement("time");
      time.dateTime = file.updatedAt;
      time.textContent = formatDate(file.updatedAt);
      updated.append(time);
      const actionsCell = document.createElement("td");
      actionsCell.className = "file-actions-cell";
      const actions = document.createElement("details");
      actions.className = "row-actions";
      const actionsSummary = document.createElement("summary");
      actionsSummary.setAttribute("aria-label", `Actions for ${file.name}`);
      actionsSummary.textContent = "⋯";
      const menu = document.createElement("div");
      menu.className = "file-menu";
      const metadata = document.createElement("p");
      metadata.className = "file-menu-meta";
      metadata.textContent = `${formatBytes(file.size)} · Updated ${formatDate(file.updatedAt)}`;
      const buttons = document.createElement("div");
      buttons.className = "file-menu-actions";
      const rename = document.createElement("button");
      rename.type = "button";
      rename.className = "secondary";
      rename.dataset.write = "";
      rename.textContent = "Rename";
      rename.addEventListener("click", () => { actions.open = false; renameFile(file); });
      const remove = document.createElement("button");
      remove.type = "button";
      remove.className = "danger";
      remove.dataset.write = "";
      remove.textContent = "Delete";
      remove.addEventListener("click", () => { actions.open = false; deleteFile(file); });
      buttons.append(rename, remove);
      menu.append(metadata, buttons);
      actions.append(actionsSummary, menu);
      actions.addEventListener("toggle", () => {
        if (!actions.open) return;
        for (const other of document.querySelectorAll(".row-actions[open]")) if (other !== actions) other.open = false;
      });
      actionsCell.append(actions);
      row.append(nameCell, updated, actionsCell);
      list.append(row);
    }
    updateConnectivity();
  }

  async function loadLibrary() {
    state.library = await fetchJSON("/api/library");
    if (!Array.isArray(state.library.files)) state.library.files = [];
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
    if ($("#upload-dialog").open) $("#upload-dialog").close();
    const data = new FormData();
    for (const file of files) data.append("files", file, file.name);
    toast(`Uploading ${files.length === 1 ? files[0].name : `${files.length} files`}…`);
    const result = await fetchJSON("/api/files", { method: "POST", body: data });
    const failures = result.results.filter(item => item.error);
    const replacements = result.results.filter(item => item.replaced).length;
    await loadLibrary();
    $("#file-input").value = "";
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
      const identity = document.createElement("div");
      identity.className = "owner-identity";
      const title = document.createElement("strong");
      title.textContent = owner.personName;
      const passkey = document.createElement("span");
      passkey.className = "owner-passkey";
      passkey.textContent = owner.passkeyName;
      identity.append(title, passkey);
      heading.append(identity);
      if (owner.current) {
        const current = document.createElement("span");
        current.className = "current-badge";
        current.textContent = "This passkey";
        heading.append(current);
      }
      const registered = document.createElement("p");
      registered.className = "owner-registered";
      registered.textContent = `Registered ${formatDate(owner.createdAt)}`;
      const details = document.createElement("details");
      details.className = "owner-details";
      const detailsSummary = document.createElement("summary");
      detailsSummary.textContent = "Passkey details";
      const metadata = document.createElement("div");
      metadata.className = "owner-meta";
      const rows = [
        ["Last used", formatDate(owner.lastUsedAt)],
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
      details.append(detailsSummary, metadata);
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
      card.append(heading, registered, details, actions);
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
    const files = libraryFiles();
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
        worker.postMessage({ type: "SYNC_PRIVATE", urls: files.map(fileURL) }, [channel.port2]);
      });
      if (result.ok) {
        status.textContent = files.length ? "Ready offline" : "No files to cache";
        status.className = files.length ? "offline-status ready" : "offline-status";
        status.removeAttribute("title");
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

  function openUploadDialog() {
    if (state.online) $("#upload-dialog").showModal();
  }

  $("#add-files-button").addEventListener("click", openUploadDialog);
  $("#choose-files").addEventListener("click", () => $("#file-input").click());
  $("#file-input").addEventListener("change", event => uploadFiles([...event.target.files]).catch(error => toast(error.message)));
  const dropZone = $("#drop-zone");
  for (const type of ["dragenter", "dragover"]) dropZone.addEventListener(type, event => { event.preventDefault(); dropZone.classList.add("dragging"); });
  for (const type of ["dragleave", "drop"]) dropZone.addEventListener(type, event => { event.preventDefault(); dropZone.classList.remove("dragging"); });

  let dragDepth = 0;
  function isFileDrag(event) {
    return [...(event.dataTransfer?.types || [])].includes("Files");
  }
  addEventListener("dragenter", event => {
    if (!state.online || !isFileDrag(event)) return;
    event.preventDefault();
    dragDepth++;
    $("#page-drop-overlay").hidden = false;
  });
  addEventListener("dragover", event => {
    if (!state.online || !isFileDrag(event)) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = "copy";
  });
  addEventListener("dragleave", event => {
    if (!isFileDrag(event)) return;
    dragDepth = Math.max(0, dragDepth - 1);
    if (dragDepth === 0) $("#page-drop-overlay").hidden = true;
  });
  addEventListener("drop", event => {
    if (!state.online || !isFileDrag(event)) return;
    event.preventDefault();
    dragDepth = 0;
    $("#page-drop-overlay").hidden = true;
    uploadFiles([...(event.dataTransfer?.files || [])]).catch(error => toast(error.message));
  });

  const searchContainer = $("#search-container");
  const searchInput = $("#file-search");
  const searchToggle = $("#search-toggle");

  function expandSearch(event) {
    if (event && event.type !== "click") event.preventDefault();
    searchContainer.classList.add("expanded");
    requestAnimationFrame(() => {
      searchInput.focus();
    });
  }

  if (searchToggle) {
    searchToggle.addEventListener("pointerdown", expandSearch);
    searchToggle.addEventListener("click", expandSearch);
  }

  searchInput.addEventListener("blur", () => {
    if (!searchInput.value) searchContainer.classList.remove("expanded");
  });
  searchInput.addEventListener("input", event => {
    clearTimeout(state.searchTimer);
    state.searchTimer = setTimeout(() => { state.query = event.target.value; renderFiles(); }, 160);
  });
  $("#file-sort").addEventListener("change", event => { state.sort = event.target.value; renderFiles(); });

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
  refreshConnectivity().then(() => loadLibrary()).catch(error => toast(error.message));
})();
