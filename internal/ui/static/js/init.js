(() => {
  // Suppress Tailwind CDN warning
  if (window.tailwind) {
    window.tailwind.config = window.tailwind.config || {};
    window.tailwind.config.silent = true;
  }
  // Initialize activeTab
  if (typeof window !== "undefined") {
    window.activeTab = undefined;
  }
})();
