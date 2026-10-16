'use strict';
(() => {
  const tab = (which) => {
    const login = which === 'login';
    $('f-login').classList.toggle('hidden', !login);
    $('f-register').classList.toggle('hidden', login);
    $('tab-login').className = 'btn' + (login ? '' : ' ghost');
    $('tab-register').className = 'btn' + (login ? ' ghost' : '');
  };
  $('tab-login').addEventListener('click', () => tab('login'));
  $('tab-register').addEventListener('click', () => tab('register'));

  const done = (me, text) => {
    if (text) flash('ok', text);
    location.href = me.role === 'admin' ? '/admin' : '/account';
  };
  const busy = (form, on) => { const b = form.querySelector('button[type=submit]'); b.disabled = on; };

  $('f-login').addEventListener('submit', async (e) => {
    e.preventDefault();
    busy($('f-login'), true);
    const r = await api('/api/v1/auth/login', { method: 'POST', body: { email: $('l-email').value, password: $('l-pass').value } });
    busy($('f-login'), false);
    if (r.ok) done(r.data, `Welcome back, ${r.data.name}.`);
    else if (r.status === 401) toast('err', 'Invalid email or password.');
    else if (r.status === 429) toast('err', 'Too many attempts. This account is locked for a few minutes.', { id: 'rate' });
    else if (r.status) toast('err', errText(r, 'Login failed.'));
  });

  $('f-register').addEventListener('submit', async (e) => {
    e.preventDefault();
    busy($('f-register'), true);
    const r = await api('/api/v1/auth/register', { method: 'POST',
      body: { name: $('r-name').value, email: $('r-email').value, password: $('r-pass').value } });
    busy($('f-register'), false);
    if (r.ok) done(r.data, 'Account created. Next step: submit your KYC details.');
    else if (r.status === 409) toast('err', 'An account with this email already exists. Try logging in.');
    else if (r.status && r.status !== 429) toast('err', errText(r, 'Registration failed.'));
  });

  // Already logged in? Skip the form.
  api('/api/v1/me').then((r) => { if (r.ok) done(r.data); });
})();
