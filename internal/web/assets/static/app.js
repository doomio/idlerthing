// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// idlerthing app.js — progressive enhancement, no inline scripts (CSP-safe).

// Make table rows with .row-link clickable (except interactive children).
// data-href is always a server-built path (e.g. "/servers/12"), never user
// input — keep it that way, it is assigned straight to location.
document.addEventListener('click', function (e) {
  const row = e.target.closest('tr.row-link');
  if (!row?.dataset.href) return;
  if (e.target.closest('a, button, form, input, select, textarea')) return;
  window.location.href = row.dataset.href;
});

// Copy buttons (.copy-btn with data-copy text).
document.addEventListener('click', function (e) {
  const btn = e.target.closest('.copy-btn');
  if (!btn) return;
  const text = btn.dataset.copy || '';
  if (navigator.clipboard?.writeText) {
    navigator.clipboard.writeText(text).then(function () {
      btn.textContent = '✓';
      setTimeout(function () { btn.textContent = '⧉'; }, 1500);
    });
  }
});

// Keep the accent color picker and its hex text field in sync (settings page).
document.addEventListener('input', function (e) {
  if (e.target.classList.contains('color-swatch')) {
    const text = e.target.closest('.input-group')?.querySelector('input[type="text"]');
    if (text) text.value = e.target.value;
  }
  if (e.target.name === 'accent_color') {
    const picker = e.target.closest('.input-group')?.querySelector('.color-swatch');
    if (picker && /^#[0-9a-fA-F]{6}$/.test(e.target.value)) picker.value = e.target.value;
  }
});

// ---------- htmx enhancements ----------

// A failed request must not look like a successful one. Before this, a 500 from
// a swap (a broken sort link, a catalog save that blew up) changed nothing on
// screen: no message, no spinner, and the click appeared to have done the work.
function setAlert(message) {
  const slot = document.getElementById('htmx-alert');
  if (!slot) return;
  slot.textContent = '';
  if (!message) return;
  const box = document.createElement('div');
  box.className = 'flash flash-err';
  box.setAttribute('role', 'alert');
  box.textContent = message;
  slot.appendChild(box);
}

document.body.addEventListener('htmx:beforeRequest', function () { setAlert(''); });
document.body.addEventListener('htmx:responseError', function (e) {
  const status = e.detail && e.detail.xhr ? e.detail.xhr.status : 0;
  setAlert('That request failed' + (status ? ' (HTTP ' + status + ')' : '') +
    ' — nothing was changed.');
});
document.body.addEventListener('htmx:sendError', function () {
  setAlert('The request could not be sent — is the server still reachable?');
});

// A catalog mutation swaps only its own list, so the counts in the sidebar are
// re-fetched when the response asks for it (HX-Trigger: counts-changed). The
// nav key comes from the item that is highlighted right now, so the refreshed
// column keeps the same selection.
document.body.addEventListener('counts-changed', function () {
  const active = document.querySelector('#sidebar-nav .nav-item.active');
  const href = active ? (active.getAttribute('href') || '') : '';
  const nav = href.replace(/^\/(?:catalogs\/)?/, '');
  htmx.ajax('GET', '/partials/counts?nav=' + encodeURIComponent(nav),
    { target: '#sidebar-nav', swap: 'outerHTML' });
});

// data-confirm on a plain form: the protection hx-confirm gives htmx forms, for
// the ones that post normally (rotating the API token). Without JavaScript the
// form submits as before, which is why only genuinely destructive actions use
// it — the delete forms have no plain action at all.
document.addEventListener('submit', function (e) {
  const msg = e.target.dataset ? e.target.dataset.confirm : '';
  if (msg && !window.confirm(msg)) e.preventDefault();
});
