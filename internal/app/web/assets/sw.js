const CACHE_VERSION = "__CACHE_VERSION__";
const SHELL_CACHE = `capsule-shell-${CACHE_VERSION}`;
const PRIVATE_CACHE = "capsule-private-v1";
const SHELL_URLS = ["/app", "/assets/styles.css", "/assets/app.js", "/assets/icon.svg", "/assets/icon-192.png", "/assets/icon-512.png", "/manifest.webmanifest"];
const REVALIDATE_TIMEOUT = 4000;
const SYNC_SHELL_TIMEOUT = 30000;
const SYNC_CONTENT_TIMEOUT = 120000;

function fetchWithTimeout(input, init, timeoutMs) {
  return fetch(input, timeoutMs ? { ...init, signal: AbortSignal.timeout(timeoutMs) } : init);
}

self.addEventListener("install", event => {
  event.waitUntil(precacheShell().catch(() => {}).then(() => self.skipWaiting()));
});

self.addEventListener("activate", event => {
  event.waitUntil((async () => {
    for (const name of await caches.keys()) {
      if (name.startsWith("capsule-shell-") && name !== SHELL_CACHE) await caches.delete(name);
    }
    await self.clients.claim();
  })());
});

let syncQueue = Promise.resolve();

self.addEventListener("message", event => {
  if (event.data?.type === "SYNC_PRIVATE") {
    const run = syncQueue.then(() => syncPrivate(event.data.urls || []));
    syncQueue = run.catch(() => {});
    event.waitUntil(run.then(
      () => event.ports[0]?.postMessage({ ok: true }),
      error => event.ports[0]?.postMessage({ ok: false, error: error.message }),
    ));
  }
  if (event.data?.type === "CLEAR_PRIVATE") {
    event.waitUntil((async () => {
      for (const name of await caches.keys()) {
        if (name.startsWith("capsule-")) await caches.delete(name);
      }
      event.ports[0]?.postMessage({ ok: true });
    })());
  }
});

function shellResponseUsable(url, response) {
  return response.ok && !(url === "/app" && new URL(response.url).pathname !== "/app");
}

async function precacheShell() {
  const shell = await caches.open(SHELL_CACHE);
  for (const url of SHELL_URLS) {
    if (await shell.match(url)) continue;
    const response = await fetchWithTimeout(url, { credentials: "same-origin" }, SYNC_SHELL_TIMEOUT);
    if (shellResponseUsable(url, response)) await shell.put(url, response);
  }
}

async function syncPrivate(contentURLs) {
  const failures = [];
  const shell = await caches.open(SHELL_CACHE);
  for (const url of SHELL_URLS) {
    if (await shell.match(url)) continue;
    try {
      const response = await fetchWithTimeout(url, { credentials: "same-origin" }, SYNC_SHELL_TIMEOUT);
      if (!shellResponseUsable(url, response)) throw new Error();
      await shell.put(url, response);
    } catch {
      failures.push(url);
    }
  }
  const privateCache = await caches.open(PRIVATE_CACHE);
  if (!(await privateCache.match("/api/library"))) {
    try {
      const library = await fetchWithTimeout("/api/library", { credentials: "same-origin" }, SYNC_SHELL_TIMEOUT);
      if (!library.ok) throw new Error();
      await privateCache.put("/api/library", library);
    } catch {
      failures.push("the file index");
    }
  }
  const allowed = new Set(contentURLs.map(url => new URL(url, self.location.origin).href));
  for (const url of contentURLs) {
    if (await privateCache.match(url)) continue;
    try {
      const response = await fetchWithTimeout(url, { credentials: "same-origin" }, SYNC_CONTENT_TIMEOUT);
      if (!response.ok) throw new Error();
      await privateCache.put(url, response);
    } catch {
      failures.push(url);
    }
  }
  for (const request of await privateCache.keys()) {
    if (new URL(request.url).pathname.startsWith("/content/") && !allowed.has(request.url)) await privateCache.delete(request);
  }
  if (failures.length) throw new Error(`Could not cache ${failures.length} item${failures.length === 1 ? "" : "s"}.`);
}

self.addEventListener("fetch", event => {
  if (event.request.method !== "GET") return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname.startsWith("/content/")) {
    event.respondWith(networkFirstContent(event.request, PRIVATE_CACHE));
    return;
  }
  if (url.pathname === "/api/library") {
    event.respondWith(networkFirst(event.request, PRIVATE_CACHE));
    return;
  }
  if (url.pathname === "/app") {
    event.respondWith(networkFirst(event.request, SHELL_CACHE));
    return;
  }
  if (SHELL_URLS.includes(url.pathname)) {
    event.respondWith(cacheFirst(event.request, SHELL_CACHE));
    return;
  }
});

async function cacheFirst(request, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(request);
  if (cached) return cached;
  const response = await fetch(request);
  const url = new URL(response.url);
  if (response.ok && !(url.pathname === "/" && new URL(request.url).pathname === "/app")) await cache.put(request, response.clone());
  return response;
}

async function networkFirstContent(request, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(request);
  if (cached) {
    try {
      const url = new URL(request.url);
      const etag = cached.headers.get("ETag") || (url.searchParams.get("v") ? `"${url.searchParams.get("v")}"` : "");
      const headers = new Headers(request.headers);
      if (etag) headers.set("If-None-Match", etag);
      const req = new Request(request, { headers });
      const revalidate = (async () => {
        const response = await fetch(req);
        if (response.status === 304) return cached;
        if (response.ok) await cache.put(request, response.clone());
        return response;
      })();
      const winner = await Promise.race([
        revalidate.catch(() => null),
        new Promise(resolve => setTimeout(resolve, REVALIDATE_TIMEOUT, null)),
      ]);
      return winner || cached;
    } catch {
      return cached;
    }
  }
  const response = await fetch(request);
  if (response.ok) await cache.put(request, response.clone());
  return response;
}

async function networkFirst(request, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(request);
  const network = (async () => {
    const response = await fetch(request);
    const url = new URL(response.url);
    if (response.ok && !(url.pathname === "/" && new URL(request.url).pathname === "/app")) await cache.put(request, response.clone());
    return response;
  })();
  if (!cached) return network;
  const winner = await Promise.race([
    network.catch(() => null),
    new Promise(resolve => setTimeout(resolve, REVALIDATE_TIMEOUT, null)),
  ]);
  return winner || cached;
}
