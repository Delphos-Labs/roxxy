(() => {
  /**
   * Utility functions for Roxxy UI
   */

  function escapeHtml(text) {
    if (!text) return "";
    const div = document.createElement("div");
    div.textContent = text;
    return div.innerHTML;
  }

  function formatBytes(bytes) {
    if (!bytes || typeof bytes !== "number" || isNaN(bytes)) return "0 B";
    const k = 1024;
    const sizes = ["B", "KB", "MB", "GB", "TB"];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return Math.round((bytes / Math.pow(k, i)) * 100) / 100 + " " + sizes[i];
  }

  function formatDate(date) {
    return date ? new Date(date).toLocaleDateString() : "N/A";
  }

  function getTypeIcon(type) {
    return (
      {
        apk: "ti ti-brand-alpine",
        apt: "ti ti-brand-debian",
        npm: "ti ti-brand-npm",
        pypi: "ti ti-brand-python",
        go: "ti ti-brand-golang",
      }[type.toLowerCase()] || "ti ti-package"
    );
  }

  function formatGoPackageName(name, type) {
    if (type !== "go") return name;
    if (name && name.includes("/")) {
      const parts = name.split("/");
      return parts[parts.length - 1];
    }
    return name;
  }

  /**
   * Parse a version string into comparable parts
   * Handles: v1.2.3, 1.2.3, 1.2.3-alpha, 1.2.3-rc1, 1.2, 1, latest, alpine, etc.
   */
  function parseVersion(str) {
    if (!str) return { isVersion: false, original: str };

    // Special tags that should sort first
    const specialTags = ['latest', 'stable', 'edge', 'main', 'master'];
    const lowerStr = str.toLowerCase();
    if (specialTags.includes(lowerStr)) {
      return { isVersion: false, isSpecial: true, priority: specialTags.indexOf(lowerStr), original: str };
    }

    // Try to parse as version: optional 'v' prefix, then numbers separated by dots
    // Captures: v1.2.3-alpha.1 -> ['1', '2', '3', 'alpha.1']
    const match = str.match(/^v?(\d+)(?:\.(\d+))?(?:\.(\d+))?(?:[-.](.+))?$/i);
    if (!match) {
      return { isVersion: false, original: str };
    }

    return {
      isVersion: true,
      major: parseInt(match[1], 10) || 0,
      minor: parseInt(match[2], 10) || 0,
      patch: parseInt(match[3], 10) || 0,
      prerelease: match[4] || '',
      original: str
    };
  }

  /**
   * Compare two version strings with semantic versioning awareness
   * Returns: negative if a < b, positive if a > b, 0 if equal
   */
  function compareVersions(a, b) {
    const va = parseVersion(a);
    const vb = parseVersion(b);

    // Special tags come first
    if (va.isSpecial && vb.isSpecial) return va.priority - vb.priority;
    if (va.isSpecial) return -1;
    if (vb.isSpecial) return 1;

    // Versions come before non-versions
    if (va.isVersion && !vb.isVersion) return -1;
    if (!va.isVersion && vb.isVersion) return 1;

    // Both non-versions: string compare
    if (!va.isVersion && !vb.isVersion) {
      return va.original.localeCompare(vb.original);
    }

    // Both versions: compare major.minor.patch
    if (va.major !== vb.major) return va.major - vb.major;
    if (va.minor !== vb.minor) return va.minor - vb.minor;
    if (va.patch !== vb.patch) return va.patch - vb.patch;

    // Same version numbers, compare prerelease
    // No prerelease > with prerelease (1.0.0 > 1.0.0-alpha)
    if (!va.prerelease && vb.prerelease) return 1;
    if (va.prerelease && !vb.prerelease) return -1;
    if (!va.prerelease && !vb.prerelease) return 0;

    // Both have prerelease, compare alphanumerically with numeric awareness
    return compareAlphanumeric(va.prerelease, vb.prerelease);
  }

  /**
   * Compare strings with numeric awareness (alpha2 < alpha10)
   */
  function compareAlphanumeric(a, b) {
    const aParts = a.split(/(\d+)/);
    const bParts = b.split(/(\d+)/);

    for (let i = 0; i < Math.max(aParts.length, bParts.length); i++) {
      const aPart = aParts[i] || '';
      const bPart = bParts[i] || '';

      const aNum = parseInt(aPart, 10);
      const bNum = parseInt(bPart, 10);

      if (!isNaN(aNum) && !isNaN(bNum)) {
        if (aNum !== bNum) return aNum - bNum;
      } else {
        const cmp = aPart.localeCompare(bPart);
        if (cmp !== 0) return cmp;
      }
    }
    return 0;
  }

  function getGroupColors(depth) {
    const bgColors = [
      "bg-purple-100",
      "bg-blue-200",
      "bg-cyan-200",
      "bg-teal-200",
      "bg-green-200",
    ];
    const hoverColors = [
      "hover:bg-purple-200",
      "hover:bg-blue-300",
      "hover:bg-cyan-300",
      "hover:bg-teal-300",
      "hover:bg-green-300",
    ];
    const textColors = [
      "text-purple-700",
      "text-indigo-700",
      "text-cyan-800",
      "text-teal-800",
      "text-green-800",
    ];
    const iconColors = [
      "text-purple-600",
      "text-blue-700",
      "text-cyan-700",
      "text-teal-700",
      "text-green-700",
    ];

    const index = Math.min(depth, bgColors.length - 1);
    return {
      bgColor: bgColors[index] || "bg-gray-100",
      hoverColor: hoverColors[index] || "hover:bg-gray-200",
      textColor: textColors[index] || "text-gray-700",
      iconColor: iconColors[index] || "text-gray-600",
    };
  }

  // Export utilities to global scope
  window.RoxxyUtils = {
    escapeHtml,
    formatBytes,
    formatDate,
    getTypeIcon,
    formatGoPackageName,
    getGroupColors,
    parseVersion,
    compareVersions,
    compareAlphanumeric,
  };
})();
