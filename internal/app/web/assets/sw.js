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
  const shellResponses = [];
  for (const url of SHELL_URLS) {
    const response = await fetch(url, { credentials: "same-origin", cache: "no-cache" });
    if (!response.ok || (url === "/app" && new URL(response.url).pathname !== "/app")) throw new Error(`Could not cache ${url}`);
    shellResponses.push([url, response]);
  }
  for (const [url, response] of shellResponses) await shell.put(url, response);
  const privateCache = await caches.open(PRIVATE_CACHE);
  const library = await fetch("/api/library", { credentials: "same-origin", cache: "no-cache" });
  if (!library.ok) throw new Error("Could not cache the file index");
  const allowed = new Set(contentURLs.map(url => new URL(url, self.location.origin).href));
  for (const url of contentURLs) {
    const response = await fetch(url, { credentials: "same-origin", cache: "no-cache" });
    if (!response.ok) throw new Error(`Could not cache ${url}`);
    await privateCache.put(url, response);
  }
  await privateCache.put("/api/library", library);
  for (const request of await privateCache.keys()) {
    if (new URL(request.url).pathname.startsWith("/content/") && !allowed.has(request.url)) await privateCache.delete(request);
  }
}

self.addEventListener("fetch", event => {
  if (event.request.method !== "GET") return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname.startsWith("/content/")) {
    event.respondWith(networkFirst(event.request, PRIVATE_CACHE));
    return;
  }
  if (url.pathname === "/api/library") {
    event.respondWith(networkFirst(event.request, PRIVATE_CACHE));
    return;
  }
  if (url.pathname === "/app" || SHELL_URLS.includes(url.pathname)) {
    event.respondWith(networkFirst(event.request, SHELL_CACHE));
  }
});

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
