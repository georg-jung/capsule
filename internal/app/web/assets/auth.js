(() => {
  const status = document.querySelector("#auth-status");

  function show(message) {
    if (status) status.textContent = message;
  }

  function decodeBase64URL(value) {
    const normalized = value.replace(/-/g, "+").replace(/_/g, "/");
    const padded = normalized + "=".repeat((4 - normalized.length % 4) % 4);
    const binary = atob(padded);
    return Uint8Array.from(binary, character => character.charCodeAt(0));
  }

  function encodeBase64URL(value) {
    const bytes = new Uint8Array(value);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function prepareOptions(options) {
    const prepared = structuredClone(options);
    const publicKey = prepared.publicKey;
    publicKey.challenge = decodeBase64URL(publicKey.challenge);
    if (publicKey.user?.id) publicKey.user.id = decodeBase64URL(publicKey.user.id);
    for (const descriptor of publicKey.excludeCredentials || []) descriptor.id = decodeBase64URL(descriptor.id);
    for (const descriptor of publicKey.allowCredentials || []) descriptor.id = decodeBase64URL(descriptor.id);
    return prepared;
  }

  function credentialJSON(credential) {
    const response = credential.response;
    const result = {
      id: credential.id,
      rawId: encodeBase64URL(credential.rawId),
      type: credential.type,
      authenticatorAttachment: credential.authenticatorAttachment,
      clientExtensionResults: credential.getClientExtensionResults(),
      response: {
        clientDataJSON: encodeBase64URL(response.clientDataJSON),
      },
    };
    if (response.attestationObject) {
      result.response.attestationObject = encodeBase64URL(response.attestationObject);
      result.response.transports = typeof response.getTransports === "function" ? response.getTransports() : [];
    } else {
      result.response.authenticatorData = encodeBase64URL(response.authenticatorData);
      result.response.signature = encodeBase64URL(response.signature);
      result.response.userHandle = response.userHandle ? encodeBase64URL(response.userHandle) : null;
    }
    return result;
  }

  async function requestJSON(url, options = {}) {
    const headers = { "Content-Type": "application/json", ...(options.headers || {}) };
    let response;
    try {
      response = await fetch(url, {
        credentials: "same-origin",
        ...options,
        headers,
        signal: AbortSignal.timeout(20000),
      });
    } catch (error) {
      throw new Error(error.name === "TimeoutError" ? "The request timed out. Check your connection and try again." : "The request failed. Check your connection and try again.");
    }
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || "The request could not be completed.");
    return body;
  }

  async function createPasskey(beginURL, finishURL, payload) {
    const begin = await requestJSON(beginURL, { method: "POST", body: JSON.stringify(payload) });
    const credential = await navigator.credentials.create(prepareOptions(begin.options));
    await requestJSON(finishURL, {
      method: "POST",
      headers: { "X-Ceremony-ID": begin.ceremonyId },
      body: JSON.stringify(credentialJSON(credential)),
    });
  }

  async function login() {
    show("Waiting for your passkey…");
    const begin = await requestJSON("/auth/login/begin", { method: "POST", body: "{}" });
    const credential = await navigator.credentials.get(prepareOptions(begin.options));
    await requestJSON("/auth/login/finish", {
      method: "POST",
      headers: { "X-Ceremony-ID": begin.ceremonyId },
      body: JSON.stringify(credentialJSON(credential)),
    });
    location.assign("/app");
  }

  const setupForm = document.querySelector("#setup-form");
  setupForm?.addEventListener("submit", async event => {
    event.preventDefault();
    const button = setupForm.querySelector("button");
    button.disabled = true;
    show("Creating your owner passkey…");
    try {
      const values = Object.fromEntries(new FormData(setupForm));
      await createPasskey("/auth/setup/begin", "/auth/setup/finish", values);
      location.assign("/app");
    } catch (error) {
      show(error.message);
      button.disabled = false;
    }
  });

  const loginButton = document.querySelector("#login-button");
  loginButton?.addEventListener("click", async () => {
    loginButton.disabled = true;
    try {
      await login();
    } catch (error) {
      show(error.message);
      loginButton.disabled = false;
    }
  });

  const joinForm = document.querySelector("#join-form");
  if (joinForm) {
    const token = location.hash.slice(1);
    if (!token) {
      show("This invite link is incomplete.");
    } else {
      requestJSON("/auth/invite/exchange", { method: "POST", body: JSON.stringify({ token }) })
        .then(() => {
          history.replaceState(null, "", "/join");
          joinForm.hidden = false;
          show("");
        })
        .catch(error => show(error.message));
    }
    joinForm.addEventListener("submit", async event => {
      event.preventDefault();
      const button = joinForm.querySelector("button");
      button.disabled = true;
      show("Registering your owner passkey…");
      try {
        const values = Object.fromEntries(new FormData(joinForm));
        await createPasskey("/auth/invite/begin", "/auth/invite/finish", values);
        location.assign("/app");
      } catch (error) {
        show(error.message);
        button.disabled = false;
      }
    });
  }

  if (!("credentials" in navigator)) show("This browser does not support passkeys.");
})();
