(() => {
  'use strict';
  const key = 'salcara-hub-theme';
  const media = matchMedia('(prefers-color-scheme: dark)');
  const valid = value => ['light', 'dark', 'system'].includes(value) ? value : 'system';
  let preference = 'system';
  try { preference = valid(localStorage.getItem(key)); } catch { /* Private browsing can disable preference storage. */ }
  function apply() {
    document.documentElement.dataset.theme = preference === 'system' ? (media.matches ? 'dark' : 'light') : preference;
  }
  // Apply before the stylesheet is loaded to avoid flashing the wrong theme.
  apply();
  function bind() {
    const select = document.getElementById('theme-mode');
    if (!select) return;
    select.value = preference;
    select.addEventListener('change', () => {
      preference = valid(select.value);
      apply();
      try {
        if (preference === 'system') localStorage.removeItem(key);
        else localStorage.setItem(key, preference);
      } catch { /* The selected theme still works for this page. */ }
    });
    addEventListener('storage', event => {
      if (event.key !== key && event.key !== null) return;
      preference = valid(event.newValue);
      select.value = preference;
      apply();
    });
  }
  media.addEventListener('change', () => { if (preference === 'system') apply(); });
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bind, { once: true });
  else bind();
})();
