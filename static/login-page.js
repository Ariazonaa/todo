'use strict';
// A separate file instead of an inline script: the CSP only allows script-src 'self'.
document.getElementById('login-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  // Block a double tap: a second begin would replace the ceremony cookie,
  // and the first sign-in attempt would then fail with "no sign-in in progress".
  const btn = e.target.querySelector('button[type=submit]');
  if (btn.disabled) return;
  btn.disabled = true;
  const err = document.getElementById('err');
  err.hidden = true;
  try {
    await passkeyLogin();
    location.href = '/';
  } catch (ex) {
    err.textContent = ex.name === 'NotAllowedError'
      ? t('auth.cancelled')
      : (ex.message || t('error.login_failed'));
    err.hidden = false;
  } finally {
    btn.disabled = false;
  }
});
