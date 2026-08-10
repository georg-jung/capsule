(() => {
  const state = { library: null, admin: null, csrf: "", deferredInstall: null, online: navigator.onLine, query: "", sort: "name", persistenceRequested: false };
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

  function toast(message, options = {}) {
    const element = $("#toast");
    element.textContent = message;
    element.hidden = false;
    clearTimeout(toast.timer);
    if (!options.sticky) toast.timer = setTimeout(() => { element.hidden = true; }, 4500);
  }

  async function fetchJSON(url, options = {}) {
    const { timeoutMs = 20000, ...request } = options;
    const headers = { ...(request.headers || {}) };
    if (state.csrf && request.method && request.method !== "GET") headers["X-CSRF-Token"] = state.csrf;
    if (request.body && !(request.body instanceof FormData)) headers["Content-Type"] = "application/json";
    let response;
    try {
      response = await fetch(url, {
        credentials: "same-origin",
        ...request,
        headers,
        ...(timeoutMs ? { signal: AbortSignal.timeout(timeoutMs) } : {}),
      });
    } catch (error) {
      throw new Error(error.name === "TimeoutError" ? "The request timed out. Check your connection." : "The request failed. Check your connection.");
    }
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
    const body = await fetchJSON("/api/library");
    // A response that is merely valid JSON is not enough: any other
    // authenticated payload that reached this URL by accident would carry a
    // CSRF token but no file list, and an empty list makes the offline sync
    // prune every cached file. Require the full library shape instead.
    if (typeof body.csrfToken !== "string" || body.csrfToken === "" || !Array.isArray(body.files)) {
      throw new Error("The library could not be loaded.");
    }
    state.library = body;
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

  function uploadRequest(data, describeProgress) {
    return new Promise((resolve, reject) => {
      const request = new XMLHttpRequest();
      // Stall watchdog instead of a flat timeout: legitimate uploads on slow
      // links may take arbitrarily long, but progress events should keep
      // arriving. Once the body is sent, allow the server a longer window.
      let stallTimer;
      const armStallTimer = ms => {
        clearTimeout(stallTimer);
        stallTimer = setTimeout(() => request.abort(), ms);
      };
      request.open("POST", "/api/files");
      request.responseType = "json";
      if (state.csrf) request.setRequestHeader("X-CSRF-Token", state.csrf);
      request.upload.addEventListener("progress", event => {
        armStallTimer(60000);
        if (event.lengthComputable) describeProgress(Math.round((event.loaded / event.total) * 100));
      });
      request.upload.addEventListener("loadend", () => armStallTimer(300000));
      request.addEventListener("load", () => {
        clearTimeout(stallTimer);
        const body = request.response || {};
        if ((request.status >= 200 && request.status < 300) || request.status === 207) resolve(body);
        else reject(new Error(body.error || "The upload could not be completed."));
      });
      request.addEventListener("error", () => { clearTimeout(stallTimer); reject(new Error("The upload failed. Check your connection.")); });
      request.addEventListener("abort", () => { clearTimeout(stallTimer); reject(new Error("The upload stalled and was stopped. Check your connection and try again.")); });
      armStallTimer(60000);
      request.send(data);
    });
  }

  async function uploadFiles(files) {
    if (!state.online || !files.length) return;
    if (state.uploading) {
      toast("An upload is already in progress.");
      return;
    }
    if ($("#upload-dialog").open) $("#upload-dialog").close();
    const data = new FormData();
    for (const file of files) data.append("files", file, file.name);
    const label = files.length === 1 ? files[0].name : `${files.length} files`;
    state.uploading = true;
    toast(`Uploading ${label}…`, { sticky: true });
    try {
      const result = await uploadRequest(data, percent => toast(`Uploading ${label}… ${percent}%`, { sticky: true }));
      const failures = result.results.filter(item => item.error);
      const replacements = result.results.filter(item => item.replaced).length;
      await loadLibrary();
      $("#file-input").value = "";
      if (failures.length) toast(`${failures.length} upload${failures.length === 1 ? "" : "s"} failed: ${failures[0].error}`);
      else toast(`${result.results.length} file${result.results.length === 1 ? "" : "s"} uploaded${replacements ? `, ${replacements} replaced` : ""}.`);
    } finally {
      state.uploading = false;
    }
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

  function updateOfflineStatus() {
    const status = $("#offline-status");
    if (!status) return;
    if (!state.online) {
      if (state.library) {
        status.textContent = "Using offline copy";
        status.className = "offline-status ready";
        status.removeAttribute("title");
      } else {
        status.textContent = "Offline copy incomplete";
        status.className = "offline-status incomplete";
      }
    }
  }

  // Best-effort: ask the browser not to evict our offline caches under
  // storage pressure. Requested after every synchronization attempt, not only
  // after a reported success, because a sync that outlives this page's
  // timeout still finishes in the worker and leaves a cache worth keeping.
  // Failures here must never affect the visible status text, only the tooltip
  // of the status element passed in, if any.
  async function ensurePersistentStorage(status) {
    try {
      if (!navigator.storage?.persisted) return;
      let persisted = await navigator.storage.persisted();
      if (!persisted && navigator.storage.persist && !state.persistenceRequested) {
        state.persistenceRequested = true;
        persisted = await navigator.storage.persist();
      }
      if (!persisted && status) status.title = "Your browser may delete the offline copy if disk space runs low.";
    } catch {
      // Ignore: persistence support is inconsistent across browsers.
    }
  }

  function isIOSDevice() {
    return /iP(hone|ad|od)/.test(navigator.userAgent) || (navigator.userAgent.includes("Mac") && navigator.maxTouchPoints > 1);
  }

  function isStandaloneDisplay() {
    return matchMedia("(display-mode: standalone)").matches || navigator.standalone === true;
  }

  function updateIOSInstallHint() {
    const hint = $("#ios-install-hint");
    if (!hint) return;
    hint.hidden = !(isIOSDevice() && !isStandaloneDisplay());
  }

  async function synchronizeOffline() {
    if (!("serviceWorker" in navigator)) return;
    if (!state.online) {
      updateOfflineStatus();
      return;
    }
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
        await ensurePersistentStorage(status);
      } else throw new Error(result.error || "Some files could not be cached.");
    } catch (error) {
      await ensurePersistentStorage(null);
      const online = await refreshConnectivity();
      if (!online && state.library) {
        updateOfflineStatus();
      } else {
        status.textContent = "Offline copy incomplete";
        status.className = "offline-status incomplete";
        status.title = error.message;
      }
    }
  }

  function markBusy(control) {
    control.dataset.busy = "";
    control.disabled = true;
  }

  function clearBusy(control) {
    delete control.dataset.busy;
    control.disabled = !state.online;
  }

  function updateConnectivity() {
    const online = state.online;
    const indicator = $("#connectivity");
    indicator.textContent = online ? "Online" : "Offline · read only";
    indicator.classList.toggle("offline", !online);
    for (const control of document.querySelectorAll("[data-write]")) control.disabled = !online || control.dataset.busy !== undefined;
    updateOfflineStatus();
  }

  function refreshConnectivity() {
    if (state.connectivityProbe) return state.connectivityProbe;
    state.connectivityProbe = (async () => {
      try {
        const response = await fetch("/healthz", { cache: "no-store", credentials: "same-origin", signal: AbortSignal.timeout(5000) });
        state.online = response.ok;
      } catch {
        state.online = false;
      }
      updateConnectivity();
      return state.online;
    })().finally(() => { state.connectivityProbe = null; });
    return state.connectivityProbe;
  }

  function probeConnectivity() {
    if (document.visibilityState !== "visible") return;
    const wasOnline = state.online;
    refreshConnectivity().then(online => {
      if (online && !wasOnline) loadLibrary().catch(error => toast(error.message));
    });
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
    const button = event.currentTarget.querySelector("button[type=submit]") || event.currentTarget.querySelector("button");
    if (button?.disabled) return;
    if (button) markBusy(button);
    try {
      await fetchJSON("/api/site", { method: "POST", body: JSON.stringify({ name: $("#site-name").value }) });
      location.reload();
    } catch (error) {
      toast(error.message);
      if (button) clearBusy(button);
    }
  });
  $("#invite-button").addEventListener("click", async event => {
    const button = event.currentTarget;
    if (button.disabled) return;
    markBusy(button);
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
    } catch (error) {
      toast(error.message);
    } finally {
      clearBusy(button);
    }
  });
  $("#logout-button").addEventListener("click", async event => {
    const button = event.currentTarget;
    if (button.disabled) return;
    markBusy(button);
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
    try { await fetchJSON("/auth/logout", { method: "POST", body: "{}", timeoutMs: 10000 }); } finally { location.assign("/"); }
  });

  addEventListener("online", () => { refreshConnectivity().then(online => { if (online) loadLibrary().catch(error => toast(error.message)); }); });
  addEventListener("offline", () => { state.online = false; updateConnectivity(); });
  addEventListener("visibilitychange", () => probeConnectivity());
  setInterval(probeConnectivity, 30000);
  addEventListener("beforeinstallprompt", event => { event.preventDefault(); state.deferredInstall = event; $("#install-button").hidden = false; });
  $("#install-button").addEventListener("click", async () => { await state.deferredInstall?.prompt(); state.deferredInstall = null; $("#install-button").hidden = true; });

  updateConnectivity();
  updateIOSInstallHint();
  refreshConnectivity().then(() => loadLibrary()).catch(error => toast(error.message));
})();
