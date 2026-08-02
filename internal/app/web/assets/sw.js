const SHELL_CACHE = "capsule-shell-v1";
const PRIVATE_CACHE = "capsule-private-v1";
const SHELL_URLS = ["/app", "/assets/styles.css", "/assets/app.js", "/assets/icon.svg", "/assets/icon-192.png", "/assets/icon-512.png", "/manifest.webmanifest"];

self.addEventListener("install", event => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", event => {
  event.waitUntil(self.clients.claim());
});

self.addEventListener("message", event => {
  if (event.data?.type === "SYNC_PRIVATE") {
    event.waitUntil(syncPrivate(event.data.urls || []).then(
      () => event.ports[0]?.postMessage({ ok: true }),
      error => event.ports[0]?.postMessage({ ok: false, error: error.message }),
    ));
  }
  if (event.data?.type === "CLEAR_PRIVATE") {
    event.waitUntil(Promise.all([caches.delete(SHELL_CACHE), caches.delete(PRIVATE_CACHE)]).then(() => event.ports[0]?.postMessage({ ok: true })));
  }
});

async function syncPrivate(contentURLs) {
  const shell = await caches.open(SHELL_CACHE);
  for (const url of SHELL_URLS) {
    if (await shell.match(url)) continue;
    const response = await fetch(url, { credentials: "same-origin" });
    if (!response.ok || (url === "/app" && new URL(response.url).pathname !== "/app")) throw new Error(`Could not cache ${url}`);
    await shell.put(url, response);
  }
  const privateCache = await caches.open(PRIVATE_CACHE);
  if (!(await privateCache.match("/api/library"))) {
    const library = await fetch("/api/library", { credentials: "same-origin" });
    if (!library.ok) throw new Error("Could not cache the file index");
    await privateCache.put("/api/library", library);
  }
  const allowed = new Set(contentURLs.map(url => new URL(url, self.location.origin).href));
  for (const url of contentURLs) {
    if (await privateCache.match(url)) continue;
    const response = await fetch(url, { credentials: "same-origin" });
    if (!response.ok) throw new Error(`Could not cache ${url}`);
    await privateCache.put(url, response);
  }
  for (const request of await privateCache.keys()) {
    if (new URL(request.url).pathname.startsWith("/content/") && !allowed.has(request.url)) await privateCache.delete(request);
  }
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
      const response = await fetch(req);
      if (response.status === 304) return cached;
      if (response.ok) {
        await cache.put(request, response.clone());
        return response;
      }
      return response;
    } catch {
      return cached;
    }
  }
  try {
    const response = await fetch(request);
    if (response.ok) await cache.put(request, response.clone());
    return response;
  } catch (error) {
    throw error;
  }
}

async function networkFirst(request, cacheName) {
  const cache = await caches.open(cacheName);
  try {
    const response = await fetch(request);
    const url = new URL(response.url);
    if (response.ok && !(url.pathname === "/" && new URL(request.url).pathname === "/app")) await cache.put(request, response.clone());
    return response;
  } catch (error) {
    const cached = await cache.match(request);
    if (cached) return cached;
    throw error;
  }
}
