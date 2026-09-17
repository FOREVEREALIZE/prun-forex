// Discord sends us back here with #access_token=... (implicit grant). The token
// goes to our server once, which uses it to identify the user and discards it.
(async () => {
  const hash = new URLSearchParams(location.hash.slice(1));
  const query = new URLSearchParams(location.search);
  history.replaceState(null, '', location.pathname);

  const expected = sessionStorage.getItem('discord_oauth_state');
  sessionStorage.removeItem('discord_oauth_state');

  const fail = (msg) => {
    document.getElementById('auth-msg').textContent = 'Sign-in failed';
    const err = document.getElementById('auth-err');
    err.textContent = msg;
    err.hidden = false;
    document.getElementById('auth-back').hidden = false;
  };

  const error = hash.get('error') || query.get('error');
  if (error) {
    return fail(hash.get('error_description') || query.get('error_description') || 'Discord sign-in was cancelled.');
  }
  const token = hash.get('access_token');
  if (!token || !expected || (hash.get('state') || query.get('state')) !== expected) {
    return fail('This sign-in link expired or didn\'t come from this browser. Please try again.');
  }
  try {
    const res = await fetch('/auth/session', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-Requested-With': 'fetch' },
      body: JSON.stringify({ access_token: token }),
    });
    if (!res.ok) throw new Error((await res.text()).trim() || res.statusText);
    location.replace('/');
  } catch (e) {
    fail(String(e.message || e));
  }
})();
