'use strict';
(() => {
  const tab = (which) => {
    const login = which === 'login';
    $('f-login').classList.toggle('hidden', !login);
    $('f-register').classList.toggle('hidden', login);
    $('tab-login').className = 'btn' + (login ? '' : ' ghost');
    $('tab-register').className = 'btn' + (login ? ' ghost' : '');
    $('msg').className = 'msg';
  };
  $('tab-login').addEventListener('click', () => tab('login'));
  $('tab-register').addEventListener('click', () => tab('register'));

  const done = (me) => { location.href = me.role === 'admin' ? '/admin' : '/account'; };

  $('f-login').addEventListener('submit', async (e) => {
    e.preventDefault();
    const r = await api('/api/v1/auth/login', { method: 'POST', body: { email: $('l-email').value, password: $('l-pass').value } });
    if (r.ok) done(r.data); else showMsg($('msg'), false, r.data?.error || 'Login failed');
  });

  $('f-register').addEventListener('submit', async (e) => {
    e.preventDefault();
    const r = await api('/api/v1/auth/register', { method: 'POST',
      body: { name: $('r-name').value, email: $('r-email').value, password: $('r-pass').value } });
    if (r.ok) done(r.data); else showMsg($('msg'), false, r.data?.error || 'Registration failed');
  });

  // Already logged in? Skip the form.
  api('/api/v1/me').then((r) => { if (r.ok) done(r.data); });
})();
