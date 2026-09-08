(() => {
  const button = document.querySelector("[data-storage-check]");
  const result = document.querySelector("[data-storage-result]");
  if (!button || !result) return;
  button.addEventListener("click", async () => {
    button.disabled = true;
    result.textContent = "Проверяем подключение…";
    result.className = "storage-status";
    try {
      const response = await fetch("/admin/storage/check", { headers: { Accept: "application/json" }, cache: "no-store" });
      if (response.redirected) throw new Error("session");
      const data = await response.json();
      result.textContent = data.message;
      result.className = "storage-status " + (response.ok ? "storage-status--ok" : "storage-status--error");
    } catch (_) {
      result.textContent = "Не удалось выполнить проверку. Проверьте соединение и обновите страницу.";
      result.className = "storage-status storage-status--error";
    } finally {
      button.disabled = false;
    }
  });
})();
