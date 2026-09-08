(() => {
  let theme = "light";
  try { if (localStorage.getItem("neva-theme") === "dark") theme = "dark"; } catch (_) {}
  document.documentElement.dataset.theme = theme;
  document.addEventListener("DOMContentLoaded", () => {
    const buttons = document.querySelectorAll("[data-theme-toggle]");
    function render() {
      buttons.forEach(button => {
        const dark = document.documentElement.dataset.theme === "dark";
        button.textContent = dark ? "Светлая тема" : "Тёмная тема";
        button.setAttribute("aria-label", dark ? "Включить светлую тему" : "Включить тёмную тему");
      });
    }
    buttons.forEach(button => button.addEventListener("click", () => {
      theme = document.documentElement.dataset.theme === "dark" ? "light" : "dark";
      document.documentElement.dataset.theme = theme;
      try { localStorage.setItem("neva-theme", theme); } catch (_) {}
      render();
    }));
    render();
  });
})();
