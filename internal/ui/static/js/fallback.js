(() => {
  // Fallback: remove x-cloak if Alpine.js doesn't initialize
  setTimeout(() => {
    const appElement = document.getElementById("app");
    if (appElement && appElement.hasAttribute("x-cloak")) {
      appElement.removeAttribute("x-cloak");
      console.warn("Alpine.js did not initialize, showing content anyway");
    }
  }, 2000);

  // Log browser info for debugging
  document.addEventListener("DOMContentLoaded", () => {
    const browserInfo = {
      userAgent: navigator.userAgent,
      isEdge: navigator.userAgent.includes("Edg"),
      isChrome:
        navigator.userAgent.includes("Chrome") &&
        !navigator.userAgent.includes("Edg"),
      isFirefox: navigator.userAgent.includes("Firefox"),
      supportsObjectEntries: typeof Object.entries === "function",
      supportsObjectKeys: typeof Object.keys === "function",
      supportsTemplateLiterals: (() => {
        try {
          eval("`test`");
          return true;
        } catch (err) {
          return false;
        }
      })(),
    };

    console.log("[Browser Info]", browserInfo);

    if (!browserInfo.supportsObjectEntries || !browserInfo.supportsObjectKeys) {
      console.error(
        "[Browser Compatibility] Missing required JavaScript features!"
      );
    }

    // Check if Alpine.js loaded
    setTimeout(() => {
      if (typeof Alpine === "undefined") {
        console.error("[Alpine.js] Failed to load!");
      } else {
        console.log(
          "[Alpine.js] Loaded successfully, version:",
          Alpine.version || "unknown"
        );
      }
    }, 1000);
  });
})();
