'use strict';

// Base64url <-> ArrayBuffer for the WebAuthn API
function b64uToBuf(s) {
  s = s.replace(/-/g, '+').replace(/_/g, '/');
  const bin = atob(s + '='.repeat((4 - (s.length % 4)) % 4));
  const b = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) b[i] = bin.charCodeAt(i);
  return b.buffer;
}
function bufToB64u(buf) {
  let s = '';
  for (const x of new Uint8Array(buf)) s += String.fromCharCode(x);
  return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

async function wanFetch(url, body, headers) {
  const res = await fetch(url, {
    method: 'POST',
    // X-Lang: the server replies with error messages in the page's language
    headers: { 'Content-Type': 'application/json', 'X-Lang': LANG, ...headers },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (!res.ok) {
    let data = {};
    try { data = await res.json(); } catch {}
    const err = new Error(data.error || t('error.status', { status: res.status }));
    err.reauth = !!data.reauth; // server wants a fresh passkey sign-in first
    throw err;
  }
  return res;
}

async function passkeyLogin() {
  const begin = await wanFetch('/api/auth/login/begin');
  const opt = (await begin.json()).publicKey;
  opt.challenge = b64uToBuf(opt.challenge);
  if (opt.allowCredentials) {
    opt.allowCredentials = opt.allowCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
  }
  const cred = await navigator.credentials.get({ publicKey: opt });
  await wanFetch('/api/auth/login/finish', {
    id: cred.id,
    rawId: bufToB64u(cred.rawId),
    type: cred.type,
    response: {
      authenticatorData: bufToB64u(cred.response.authenticatorData),
      clientDataJSON: bufToB64u(cred.response.clientDataJSON),
      signature: bufToB64u(cred.response.signature),
      userHandle: cred.response.userHandle ? bufToB64u(cred.response.userHandle) : null,
    },
  });
}

// headers: only setup needs any (X-Setup-Code).
async function passkeyRegister(beginUrl, finishUrl, name, headers) {
  const begin = await wanFetch(beginUrl, undefined, headers);
  const opt = (await begin.json()).publicKey;
  opt.challenge = b64uToBuf(opt.challenge);
  opt.user.id = b64uToBuf(opt.user.id);
  if (opt.excludeCredentials) {
    opt.excludeCredentials = opt.excludeCredentials.map((c) => ({ ...c, id: b64uToBuf(c.id) }));
  }
  const cred = await navigator.credentials.create({ publicKey: opt });
  await wanFetch(finishUrl + '?name=' + encodeURIComponent(name || ''), {
    id: cred.id,
    rawId: bufToB64u(cred.rawId),
    type: cred.type,
    response: {
      attestationObject: bufToB64u(cred.response.attestationObject),
      clientDataJSON: bufToB64u(cred.response.clientDataJSON),
    },
  }, headers);
}
