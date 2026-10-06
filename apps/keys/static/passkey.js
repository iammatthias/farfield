// Passkey ceremonies for the keys console — the login button and the "Add
// passkey" form. navigator.credentials speaks ArrayBuffers; the server speaks
// base64url JSON; this file is the conversion between them and nothing else.
(() => {
  "use strict";
  const dec = (s) => {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    const bin = atob(s + "===".slice((s.length + 3) % 4));
    return Uint8Array.from(bin, (c) => c.charCodeAt(0)).buffer;
  };
  const enc = (buf) => {
    let bin = "";
    for (const b of new Uint8Array(buf)) bin += String.fromCharCode(b);
    return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };
  // The error line is created on first use: an empty .error still draws its rule.
  const errBox = document.querySelector("[data-passkey-error]");
  const show = (msg) => {
    if (!errBox) return;
    errBox.innerHTML = "";
    const p = document.createElement("p");
    p.className = "error";
    p.textContent = msg;
    errBox.append(p);
  };

  // A 303 to the login page (a stale session) is followed by fetch; take the
  // browser there instead of parsing HTML as JSON.
  async function post(url, body) {
    const r = await fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: typeof body === "string" ? body : JSON.stringify(body || {}),
    });
    if (r.redirected) {
      location.href = r.url;
      return null;
    }
    const j = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(j.error || "Failed");
    return j;
  }

  const supported = !!window.PublicKeyCredential;

  // ── login ──
  const loginBtn = document.querySelector("[data-passkey-login]");
  if (loginBtn) {
    if (!supported) loginBtn.closest("#passkey").style.display = "none";
    loginBtn.addEventListener("click", async () => {
      loginBtn.disabled = true;
      try {
        const next = document.getElementById("passkey").dataset.next || "";
        const opts = await post("/passkey/login/begin", { next });
        if (!opts) return;
        const pk = opts.publicKey;
        pk.challenge = dec(pk.challenge);
        for (const c of pk.allowCredentials || []) c.id = dec(c.id);
        const cred = await navigator.credentials.get({ publicKey: pk });
        const res = await post("/passkey/login/finish", {
          id: cred.id,
          rawId: enc(cred.rawId),
          type: cred.type,
          response: {
            clientDataJSON: enc(cred.response.clientDataJSON),
            authenticatorData: enc(cred.response.authenticatorData),
            signature: enc(cred.response.signature),
            userHandle: cred.response.userHandle ? enc(cred.response.userHandle) : null,
          },
          clientExtensionResults: cred.getClientExtensionResults(),
        });
        if (res) location.href = res.next || "/";
      } catch (e) {
        show(e.name === "NotAllowedError" ? "Cancelled." : e.message);
      } finally {
        loginBtn.disabled = false;
      }
    });
  }

  // ── register ──
  const regForm = document.querySelector("[data-passkey-register]");
  if (regForm) {
    if (!supported) regForm.style.display = "none";
    regForm.addEventListener("submit", async (ev) => {
      ev.preventDefault();
      const btn = regForm.querySelector("button");
      btn.disabled = true;
      try {
        const opts = await post("/passkey/register/begin", { name: regForm.querySelector("#pk-name").value });
        if (!opts) return;
        const pk = opts.publicKey;
        pk.challenge = dec(pk.challenge);
        pk.user.id = dec(pk.user.id);
        for (const c of pk.excludeCredentials || []) c.id = dec(c.id);
        const cred = await navigator.credentials.create({ publicKey: pk });
        const res = await post("/passkey/register/finish", {
          id: cred.id,
          rawId: enc(cred.rawId),
          type: cred.type,
          response: {
            clientDataJSON: enc(cred.response.clientDataJSON),
            attestationObject: enc(cred.response.attestationObject),
            transports: cred.response.getTransports ? cred.response.getTransports() : [],
          },
          clientExtensionResults: cred.getClientExtensionResults(),
        });
        if (res) location.reload();
      } catch (e) {
        show(e.name === "InvalidStateError" ? "Already added." :
             e.name === "NotAllowedError" ? "Cancelled." : e.message);
      } finally {
        btn.disabled = false;
      }
    });
  }
})();
