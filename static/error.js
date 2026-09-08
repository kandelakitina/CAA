(() => {
  const back = document.querySelector("[data-error-back]");
  if (!back || !document.referrer || history.length < 2) return;
  try {
    if (new URL(document.referrer).origin !== location.origin) return;
    back.hidden = false;
    back.addEventListener("click", () => history.back());
  } catch (_) {}
})();
