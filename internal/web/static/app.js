// Relata's only client-side script.
//
// It does two small things that plain HTML cannot: confirm destructive actions,
// and let HTMX requests carry the CSRF token. Everything else is server
// rendered. There is no inline script anywhere, which is what allows the
// Content-Security-Policy to forbid it outright.

(function () {
  "use strict";

  // Confirm destructive submissions. The prompt text lives in the markup so
  // that it is translatable and reviewable alongside the form it guards.
  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!(form instanceof HTMLFormElement)) return;
    var message = form.getAttribute("data-confirm");
    if (message && !window.confirm(message)) {
      event.preventDefault();
    }
  });

  // HTMX issues requests outside the normal form flow, so the CSRF token has to
  // be attached explicitly. It is read from the form field the server rendered,
  // never from a cookie.
  // Copy-to-clipboard for a freshly issued API token. Delegated, so it works
  // for content rendered after load. The value is read from the DOM only; it is
  // never sent anywhere.
  document.addEventListener("click", function (event) {
    var trigger = event.target.closest("[data-copy]");
    if (!trigger) {
      return;
    }
    var source = document.getElementById(trigger.getAttribute("data-copy"));
    if (!source) {
      console.warn("Copy control has no matching source element");
      return;
    }
    var original = trigger.textContent;
    function showStatus(message) {
      trigger.textContent = message;
      window.setTimeout(function () {
        trigger.textContent = original;
      }, 2000);
    }
    if (!navigator.clipboard) {
      console.warn("Clipboard API is unavailable in this browser context");
      showStatus("Copy unavailable");
      return;
    }
    navigator.clipboard.writeText(source.textContent.trim()).then(function () {
      showStatus("Copied");
    }).catch(function () {
      // Do not log the clipboard value or the full error object: the source is
      // a bearer credential and browser errors add no actionable context here.
      console.warn("Clipboard write was refused");
      showStatus("Copy failed");
    });
  });

  document.body.addEventListener("htmx:configRequest", function (event) {
    var field = document.querySelector('input[name="csrf_token"]');
    if (field && field.value) {
      event.detail.headers["X-CSRF-Token"] = field.value;
    }
  });
})();
