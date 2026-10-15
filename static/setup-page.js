'use strict';
// A separate file instead of an inline script: the CSP only allows script-src 'self'.
document.getElementById('setup-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  // Block a double tap: a second begin would replace the ceremony cookie.
  const btn = e.target.querySelector('button[type=submit]');
  if (btn.disabled) return;
  btn.disabled = true;
  const err = document.getElementById('err');
  err.hidden = true;
  try {
    await passkeyRegister('/api/setup/begin', '/api/setup/finish',
      document.getElementById('device-name').value.trim(),
      { 'X-Setup-Code': document.getElementById('setup-code').value.trim() });
    location.href = '/';
  } catch (ex) {
    err.textContent = ex.name === 'NotAllowedError'
      ? t('auth.cancelled')
      : (ex.message || t('setup.failed'));
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
});

// First-time setup (code from the server log) or an account with a code from the admin
i18nReady.then(async () => {
  try {
    const res = await fetch('/api/setup/mode');
    if (res.ok && !(await res.json()).first) {
      // Via data-i18n: a catalog arriving late re-translates the page and
      // would otherwise bring back the first-time-setup text.
      const hint = document.querySelector('.hint');
      hint.dataset.i18n = 'setup.hint_code';
      translateInto(hint, 'setup.hint_code');
    }
  } catch {}
});
