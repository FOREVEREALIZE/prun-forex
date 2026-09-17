// Sign in: add a random state to Discord's authorize URL and check it on return (auth.js).
document.addEventListener('click', (e) => {
  const login = e.target.closest('a[data-login]');
  if (login) {
    e.preventDefault();
    const bytes = crypto.getRandomValues(new Uint8Array(16));
    const state = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
    sessionStorage.setItem('discord_oauth_state', state);
    const url = new URL(login.href);
    url.searchParams.set('state', state);
    location.href = url.toString();
    return;
  }
  if (e.target.closest('[data-swap]')) {
    const f = e.target.closest('form');
    [f.from.value, f.to.value] = [f.to.value, f.from.value];
    return;
  }
  if (e.target.closest('[data-close]')) closeModal();
});

function closeModal() {
  document.getElementById('modal').replaceChildren();
}

document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') closeModal();
});

// Toasts fade out with a CSS animation, then get removed.
document.addEventListener('animationend', (e) => {
  if (e.target.classList.contains('toast')) e.target.remove();
});

document.addEventListener('orderPlaced', () => {
  const f = document.getElementById('order-form');
  if (f) f.amount.value = '';
});

// Skip the periodic refresh while someone is typing an amount in the board.
function liveIdle() {
  const a = document.activeElement;
  return !(a && a.closest('#live') && a.matches('input[type=number]'));
}
