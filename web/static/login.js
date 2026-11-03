/* login.js - focus the error summary on a failed login; show/hide password toggle. */
(function () {
  var e = document.getElementById('login-error');
  if (e) e.focus();
  var btn = document.getElementById('pw-toggle');
  var input = document.getElementById('password');
  if (!btn || !input) return;
  btn.addEventListener('click', function () {
    var show = btn.getAttribute('aria-pressed') !== 'true';
    input.type = show ? 'text' : 'password';
    btn.setAttribute('aria-pressed', show ? 'true' : 'false');
    btn.textContent = show ? 'Hide password' : 'Show password';
    input.focus();
  });
})();
