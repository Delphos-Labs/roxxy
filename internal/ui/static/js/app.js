(() => {
  document.addEventListener("alpine:init", () => {
    Alpine.data("appData", () => ({
      activeTab: "dashboard",
      loading: false,
      loadingStats: true,
      loadingPackages: true,
      loadingDocker: true,
      buildTime: "",
      stats: {
        total_cache_size: 0,
        package_count: 0,
        docker_image_count: 0,
        total_time_saved: "0s",
        cache_hit_rate: 0,
        url_cache_count: 0,
      },
      packages: [],
      packageTree: {},
      packageGroups: {},
      loadedGroups: {},
      expandedTypes: {},
      expandedPackages: {},
      expandedGroups: {},
      dockerRepos: [],
      packageSearch: "",
      packageTotal: 0,
      showUncached: false,
      // Sorting state
      packageSortBy: "name",
      packageSortOrder: "asc",
      dockerSortBy: "name",
      dockerSortOrder: "asc",
      // Docker expansion state
      expandedDockerHosts: {},
      expandedDockerNamespaces: {},
      expandedDockerRepos: {},
      dockerRepoTags: {},
      loadingDockerTags: {},
      // Selection/bulk state
      selectionMode: false,
      selectedPackages: [],
      selectedDockerItems: [],
      selectedURLCacheItems: [],
      showDeleteModal: false,
      deleteModalItems: [],
      deleteModalType: "",
      // Image override state
      dockerOverrides: [],
      // URL cache state
      urlCacheEntries: [],
      urlCacheTotal: 0,
      loadingURLCache: false,
      urlCacheSortBy: "hostname",
      urlCacheSortOrder: "asc",
      urlCacheSearch: "",
      expandedHosts: {},
      async checkBuildTime() {
        try {
          const res = await fetch("/api/v1/buildinfo");
          if (res.ok) {
            const newBuildTime = (await res.json()).build_time || "";
            if (newBuildTime && this.buildTime && newBuildTime !== this.buildTime) {
              (console.log(
                "[checkBuildTime] Build time changed, reloading page. Old:",
                this.buildTime,
                "New:",
                newBuildTime,
              ),
                window.location.reload());
              return;
            }
          }
        } catch (e) {
          console.warn("[checkBuildTime] Failed to check build time:", e);
        }
      },
      async init() {
        this.loading = false;
        try {
          const res = await fetch("/api/v1/buildinfo");
          if (res.ok) {
            const data = await res.json();
            ((this.buildTime = data.build_time || ""),
              console.log("[init] Stored build time:", this.buildTime));
          }
        } catch (e) {
          console.warn("[init] Failed to get build time:", e);
        }
        await this.parseIndexIfNeeded();
        try {
          await Promise.all([
            this.loadStats(),
            this.loadPackages(),
            this.loadDockerRepos(),
          ]);
        } catch (e) {
          console.error("Failed to load some data:", e);
        }
        (this.expandedTypes || (this.expandedTypes = {}),
          this.expandedGroups || (this.expandedGroups = {}),
          this.expandedPackages || (this.expandedPackages = {}),
          this.$watch("packageTree", () => {
            (this.expandedTypes || (this.expandedTypes = {}),
              this.expandedGroups || (this.expandedGroups = {}),
              this.expandedPackages || (this.expandedPackages = {}));
          }));
      },
      async parseIndexIfNeeded() {
        try {
          const res = await fetch("/api/v1/index/parse", { method: "POST" });
          if (res.ok) {
            const data = await res.json();
            data.total_parsed > 0 &&
              console.log(
                `[parseIndexIfNeeded] Parsed ${data.total_parsed} index files`,
              );
          }
        } catch (e) {
          console.log(
            "[parseIndexIfNeeded] Index parsing failed (may be in progress or no index files):",
            e,
          );
        }
      },
      async loadStats() {
        this.loadingStats = true;
        try {
          const res = await fetch("/api/v1/stats/summary");
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const data = await res.json();
          this.stats = {
            total_cache_size: data.total_cache_size || 0,
            package_count: data.package_count || 0,
            docker_image_count: data.docker_image_count || 0,
            total_time_saved: data.total_time_saved || "0s",
            cache_hit_rate: data.cache_hit_rate || 0,
            url_cache_count: data.url_cache_count || 0,
            package_count_by_type: data.package_count_by_type || {},
          };
        } catch (e) {
          console.error("Failed to load stats:", e);
        } finally {
          this.loadingStats = false;
        }
      },
      async loadPackages() {
        this.loadingPackages = true;
        try {
          ((this.loadedGroups = {}), await this.loadPackageGroups());
        } catch (e) {
          console.error("Failed to load packages:", e);
        } finally {
          this.loadingPackages = false;
        }
      },
      async loadPackageGroups() {
        try {
          const params = new URLSearchParams();
          params.append("show_uncached", this.showUncached ? "true" : "false");
          const url = `/api/v1/packages/groups?${params.toString()}`,
            res = await fetch(url);
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const data = await res.json();
          if (data.packages)
            for (const [pkgType, packages] of Object.entries(data.packages))
              !packages ||
                !Array.isArray(packages) ||
                (packages.length > 0 &&
                  console.log(
                    `[loadPackageGroups] Type ${pkgType}: ${packages.length} packages`,
                  ));
          if (data.packages) {
            (this.buildTreeFromFlatList(data.packages),
              (this.packageTotal = data.total || 0));
            for (const type of ["apk", "apt", "npm", "pypi", "go"])
              if (this.packageTree[type] && this.packageTree[type].count > 0) {
                const packages = this.packageTree[type].packages
                    ? Object.keys(this.packageTree[type].packages).length
                    : 0,
                  rootCount = this.packageTree[type].groups
                    ? Object.keys(this.packageTree[type].groups).length
                    : 0;
                console.log(
                  `[loadPackageGroups] ${type}: ${this.packageTree[type].count} packages (${packages} root, ${rootCount} groups)`,
                );
              }
            this.collapseSingleChildGroups();
            this.autoExpandSingleItems();
            this.renderPackageList();
          } else
            ((this.packageGroups = data.groups || {}),
              (this.packageTotal = data.total || 0),
              this.buildPackageTreeFromGroups(),
              this.collapseSingleChildGroups(),
              this.autoExpandSingleItems(),
              this.renderPackageList());
        } catch (e) {
          console.error("Failed to load package groups:", e);
        }
      },
      buildTreeFromFlatList(packagesByType) {
        this.packageTree || (this.packageTree = {});
        const knownTypes = ["apk", "apt", "npm", "pypi", "go"];
        for (const type of knownTypes)
          this.packageTree[type]
            ? ((this.packageTree[type].groups = {}),
              (this.packageTree[type].packages = {}),
              (this.packageTree[type].count = 0))
            : (this.packageTree[type] = { groups: {}, packages: {}, count: 0 });
        const seenPackages = new Set();
        for (const [type, pkgList] of Object.entries(packagesByType))
          if (!(!pkgList || !Array.isArray(pkgList))) {
            (pkgList.length > 0 &&
              console.log(
                `[buildTreeFromFlatList] Processing type ${type} with ${pkgList.length} packages`,
              ),
              this.packageTree[type] ||
                (this.packageTree[type] = { groups: {}, packages: {}, count: 0 }));
            for (const pkg of pkgList) {
              const uniqueKey = `${type}:${pkg.name}`;
              if (seenPackages.has(uniqueKey)) {
                console.warn(
                  `[buildTreeFromFlatList] Skipping duplicate package: ${uniqueKey}`,
                );
                continue;
              }
              if (
                type === "npm" &&
                pkg.name.startsWith("@") &&
                !pkg.name.includes("/")
              ) {
                console.warn(
                  `[buildTreeFromFlatList] Skipping scoped package without sub-package: ${pkg.name}`,
                );
                continue;
              }
              if ((seenPackages.add(uniqueKey), (type === "apt" || type === "apk") && pkg.group)) {
                const groupParts = pkg.group.split("/");
                let groups = this.packageTree[type].groups;
                for (let existingPkg = 0; existingPkg < groupParts.length; existingPkg++) {
                  const existingIds = groupParts[existingPkg];
                  (groups[existingIds] ||
                    (groups[existingIds] = { groups: {}, packages: {}, count: 0, loaded: false }),
                    groups[existingIds].groups || (groups[existingIds].groups = {}));
                  const newVersions = (pkg.versions || []).length;
                  ((groups[existingIds].count += newVersions), existingPkg < groupParts.length - 1 && (groups = groups[existingIds].groups));
                }
                const lastGroup = groupParts[groupParts.length - 1],
                  groupNode = groups[lastGroup];
                groupNode.packages || (groupNode.packages = {});
                const pkgName = pkg.name;
                if (groupNode.packages[pkgName]) {
                  const existingPkg = groupNode.packages[pkgName],
                    existingIds = new Set(
                      (existingPkg.versions || []).map((y) => y.id || y.version),
                    ),
                    newVersions = (pkg.versions || [])
                      .map(
                        (y) => (
                          y.id ||
                            (y.id =
                              y.version +
                              (y.architecture ? `:${y.architecture}` : "")),
                          y
                        ),
                      )
                      .filter((y) => !existingIds.has(y.id));
                  if (newVersions.length > 0) {
                    ((existingPkg.versions = [...(existingPkg.versions || []), ...newVersions]),
                      (existingPkg.count = existingPkg.versions.length));
                    let ver = this.packageTree[type].groups;
                    for (let parentGroups = 0; parentGroups < groupParts.length; parentGroups++)
                      ver[groupParts[parentGroups]] &&
                        ((ver[groupParts[parentGroups]].count += newVersions.length),
                        parentGroups < groupParts.length - 1 && (ver = ver[groupParts[parentGroups]].groups));
                  }
                } else {
                  const existingPkg = (pkg.versions || []).map(
                    (m) => (
                      m.id ||
                        (m.id =
                          m.version +
                          (m.architecture ? `:${m.architecture}` : "")),
                      m
                    ),
                  );
                  groupNode.packages[pkgName] = {
                    versions: existingPkg,
                    count: existingPkg.length,
                    loaded: true,
                    description: pkg.description,
                    license: pkg.license,
                    dependencies:
                      pkg.dependencies &&
                      Array.isArray(pkg.dependencies) &&
                      pkg.dependencies.length > 0
                        ? pkg.dependencies
                        : undefined,
                    fullName: pkg.name,
                  };
                }
                this.packageTree[type].count += (pkg.versions || []).length;
                continue;
              }
              const nameParts = pkg.name.split("/"),
                shortName = nameParts[nameParts.length - 1];
              if (type === "go" && nameParts.length === 2) {
                const hostName = nameParts[0],
                  pkgShortName = nameParts[1];
                this.packageTree[type].groups[hostName] ||
                  (this.packageTree[type].groups[hostName] = {
                    groups: {},
                    packages: {},
                    count: 0,
                  });
                const hostGroup = this.packageTree[type].groups[hostName];
                hostGroup.packages || (hostGroup.packages = {});
                // Use short name as key, store full name in fullName property
                if (hostGroup.packages[pkgShortName]) {
                  const existingPkgNode = hostGroup.packages[pkgShortName],
                    existingIds = new Set(
                      (existingPkgNode.versions || []).map((k) => k.id || k.version),
                    ),
                    newVersions = (pkg.versions || [])
                      .map(
                        (k) => (
                          k.id ||
                            (k.id =
                              k.version +
                              (k.architecture ? `:${k.architecture}` : "")),
                          k
                        ),
                      )
                      .filter((k) => !existingIds.has(k.id));
                  newVersions.length > 0 &&
                    ((existingPkgNode.versions = [...(existingPkgNode.versions || []), ...newVersions]),
                    (existingPkgNode.count = existingPkgNode.versions.length),
                    (hostGroup.count += newVersions.length),
                    (this.packageTree[type].count += newVersions.length));
                } else {
                  const versionsWithId = (pkg.versions || []).map(
                    (d) => (
                      d.id ||
                        (d.id =
                          d.version +
                          (d.architecture ? `:${d.architecture}` : "")),
                      d
                    ),
                  );
                  ((hostGroup.packages[pkgShortName] = {
                    versions: versionsWithId,
                    count: versionsWithId.length,
                    loaded: true,
                    description: pkg.description,
                    license: pkg.license,
                    dependencies:
                      pkg.dependencies &&
                      Array.isArray(pkg.dependencies) &&
                      pkg.dependencies.length > 0
                        ? pkg.dependencies
                        : undefined,
                    fullName: pkg.name,
                  }),
                    (hostGroup.count += versionsWithId.length),
                    (this.packageTree[type].count += versionsWithId.length));
                }
                continue;
              }
              let currentGroups = this.packageTree[type].groups;
              for (let groupParts = 0; groupParts < nameParts.length - 1; groupParts++) {
                const groups = nameParts[groupParts];
                (currentGroups[groups] || (currentGroups[groups] = { groups: {}, packages: {}, count: 0 }),
                  currentGroups[groups].groups || (currentGroups[groups].groups = {}),
                  currentGroups[groups].packages || (currentGroups[groups].packages = {}),
                  (currentGroups = currentGroups[groups].groups));
              }
              if (nameParts.length > 1) {
                let groupParts = this.packageTree[type].groups;
                for (let groups = 0; groups < nameParts.length - 1; groups++)
                  if (groups === nameParts.length - 2) {
                    groupParts[nameParts[groups]].packages || (groupParts[nameParts[groups]].packages = {});
                    let lastGroup = null;
                    for (const [existingName, existingPkgData] of Object.entries(groupParts[nameParts[groups]].packages || {}))
                      if (existingPkgData.fullName === pkg.name) {
                        lastGroup = existingPkgData;
                        break;
                      }
                    if (
                      (!lastGroup && groupParts[nameParts[groups]].packages[shortName] && (lastGroup = groupParts[nameParts[groups]].packages[shortName]),
                      lastGroup)
                    ) {
                      (lastGroup.versions || []).forEach((d) => {
                        d.id ||
                          (d.id =
                            d.version +
                            (d.architecture ? `:${d.architecture}` : ""));
                      });
                      const groupNode = new Set(
                          (lastGroup.versions || []).map((d) => d.id || d.version),
                        ),
                        pkgName = (pkg.versions || [])
                          .map(
                            (d) => (
                              d.id ||
                                (d.id =
                                  d.version +
                                  (d.architecture ? `:${d.architecture}` : "")),
                              d
                            ),
                          )
                          .filter((d) => !groupNode.has(d.id));
                      (pkgName.length > 0 &&
                        ((lastGroup.versions = [...(lastGroup.versions || []), ...pkgName]),
                        (lastGroup.count = lastGroup.versions.length),
                        typeof groupParts[nameParts[groups]].count != "number" && (groupParts[nameParts[groups]].count = 0),
                        (groupParts[nameParts[groups]].count += pkgName.length),
                        (this.packageTree[type].count += pkgName.length)),
                        !lastGroup.description &&
                          pkg.description &&
                          (lastGroup.description = pkg.description),
                        !lastGroup.license && pkg.license && (lastGroup.license = pkg.license),
                        !lastGroup.dependencies &&
                          pkg.dependencies &&
                          Array.isArray(pkg.dependencies) &&
                          pkg.dependencies.length > 0 &&
                          (lastGroup.dependencies = pkg.dependencies),
                        !lastGroup.fullName && pkg.name && (lastGroup.fullName = pkg.name));
                    } else {
                      const groupNode = (pkg.versions || []).map(
                        (existingIds) => (
                          existingIds.id ||
                            (existingIds.id =
                              existingIds.version +
                              (existingIds.architecture ? `:${existingIds.architecture}` : "")),
                          existingIds
                        ),
                      );
                      groupParts[nameParts[groups]].packages[shortName] = {
                        versions: groupNode,
                        count: groupNode.length,
                        loaded: true,
                        description: pkg.description,
                        license: pkg.license,
                        dependencies:
                          pkg.dependencies &&
                          Array.isArray(pkg.dependencies) &&
                          pkg.dependencies.length > 0
                            ? pkg.dependencies
                            : undefined,
                        fullName: pkg.name,
                      };
                      const pkgName = groupNode.length;
                      (typeof groupParts[nameParts[groups]].count != "number" && (groupParts[nameParts[groups]].count = 0),
                        (groupParts[nameParts[groups]].count += pkgName),
                        (this.packageTree[type].count += pkgName));
                      let existingPkg = this.packageTree[type].groups;
                      for (let existingIds = 0; existingIds < groups; existingIds++)
                        existingPkg[nameParts[existingIds]] &&
                          (typeof existingPkg[nameParts[existingIds]].count != "number" &&
                            (existingPkg[nameParts[existingIds]].count = 0),
                          (existingPkg[nameParts[existingIds]].count += pkgName),
                          (existingPkg = existingPkg[nameParts[existingIds]].groups));
                    }
                    break;
                  } else groupParts = groupParts[nameParts[groups]].groups;
              } else if (
                (this.packageTree[type].packages ||
                  (this.packageTree[type].packages = {}),
                this.packageTree[type].packages[shortName])
              ) {
                const groupParts = this.packageTree[type].packages[shortName];
                (groupParts.versions || []).forEach((h) => {
                  h.id ||
                    (h.id =
                      h.version + (h.architecture ? `:${h.architecture}` : ""));
                });
                const groups = new Set(
                    (groupParts.versions || []).map((h) => h.id || h.version),
                  ),
                  lastGroup = (pkg.versions || [])
                    .map(
                      (h) => (
                        h.id ||
                          (h.id =
                            h.version +
                            (h.architecture ? `:${h.architecture}` : "")),
                        h
                      ),
                    )
                    .filter((h) => !groups.has(h.id));
                (lastGroup.length > 0 &&
                  ((groupParts.versions = [...(groupParts.versions || []), ...lastGroup]),
                  (groupParts.count = groupParts.versions.length),
                  (this.packageTree[type].count += lastGroup.length)),
                  !groupParts.description &&
                    pkg.description &&
                    (groupParts.description = pkg.description),
                  !groupParts.license && pkg.license && (groupParts.license = pkg.license),
                  !groupParts.dependencies &&
                    pkg.dependencies &&
                    Array.isArray(pkg.dependencies) &&
                    pkg.dependencies.length > 0 &&
                    (groupParts.dependencies = pkg.dependencies),
                  !groupParts.fullName && pkg.name && (groupParts.fullName = pkg.name));
              } else {
                const groupParts = (pkg.versions || []).map(
                  (r) => (
                    r.id ||
                      (r.id =
                        r.version +
                        (r.architecture ? `:${r.architecture}` : "")),
                    r
                  ),
                );
                ((this.packageTree[type].packages[shortName] = {
                  versions: groupParts,
                  count: groupParts.length,
                  loaded: true,
                  description: pkg.description,
                  license: pkg.license,
                  dependencies:
                    pkg.dependencies &&
                    Array.isArray(pkg.dependencies) &&
                    pkg.dependencies.length > 0
                      ? pkg.dependencies
                      : undefined,
                  fullName: pkg.name,
                }),
                  (this.packageTree[type].count += groupParts.length));
              }
            }
          }
        for (const type of knownTypes)
          if (this.packageTree[type] && this.packageTree[type].count > 0) {
            const pkgList = this.packageTree[type].packages
                ? Object.keys(this.packageTree[type].packages).length
                : 0,
              pkg = this.packageTree[type].groups
                ? Object.keys(this.packageTree[type].groups).length
                : 0;
            console.log(
              `[buildTreeFromFlatList] ${type}: ${this.packageTree[type].count} packages (${pkgList} root, ${pkg} groups)`,
            );
          }
      },
      async loadPackagesForGroup(type, groupPath) {
        const cacheKey = `${type}:${groupPath}`;
        if (!this.loadedGroups[cacheKey])
          try {
            const params = new URLSearchParams();
            (params.append("type", type),
              params.append("group", groupPath),
              params.append("show_uncached", this.showUncached ? "true" : "false"));
            const url = `/api/v1/packages/groups?${params.toString()}`;
            console.log(
              `[loadPackagesForGroup] Loading packages for ${type}:${groupPath} from ${url}`,
            );
            const res = await fetch(url);
            if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
            const data = await res.json();
            if (
              (console.log(
                `[loadPackagesForGroup] Received ${data.packages ? data.packages.length : 0} packages for ${type}:${groupPath}`,
                data,
              ),
              data.packages && data.packages.length > 0)
            ) {
              let addedCount = 0;
              (data.packages.forEach((n) => {
                const pkgType = n.type || "unknown",
                  group = n.group || "default",
                  versions = n.versions || [];
                if (versions.length === 0) {
                  console.warn(
                    `[loadPackagesForGroup] Package ${n.name} has no versions, skipping`,
                  );
                  return;
                }
                (console.log(
                  `[loadPackagesForGroup] Processing package: ${n.name} with ${versions.length} version(s), group: ${group}`,
                ),
                  versions.forEach((f) => {
                    const versionData = {
                      ...f,
                      name: n.name,
                      type: n.type,
                      group: n.group,
                      description: n.description,
                      license: n.license,
                      dependencies: n.dependencies,
                    };
                    this.packageTree[pkgType] ||
                      (this.packageTree[pkgType] = { groups: {}, count: 0 });
                    const groupParts = "/";
                    if (group.includes(groupParts)) {
                      const pkgName = group.split(groupParts);
                      let groups = this.packageTree[pkgType].groups;
                      const navigateToLeaf = ($, j, F = null, O = null) => {
                        if (j.length === 1)
                          return F && O
                            ? { node: F, packageName: j[0] }
                            : { node: $, packageName: j[0] };
                        const [x, ...P] = j;
                        let found = null;
                        if (
                          ($[x] && (found = $[x]),
                          !found && F && F.packages && F.packages[x])
                        ) {
                          if (
                            (console.log(
                              `[navigateToLeaf] Found ${x} in parentNode.packages (parent: ${O}), using it for sub-package, tail: ${P.join(groupParts)}`,
                            ),
                            (found = F.packages[x]),
                            found.packages || (found.packages = {}),
                            P.length === 1)
                          )
                            return { node: found, packageName: P[0] };
                          if (P.length > 1)
                            return (found.groups || (found.groups = {}), navigateToLeaf(found.groups, P, found, x));
                        }
                        return found &&
                          found.packages &&
                          (!found.groups || Object.keys(found.groups).length === 0)
                          ? P.length === 1
                            ? { node: found, packageName: P[0] }
                            : (found.groups || (found.groups = {}),
                              navigateToLeaf(found.groups, P, found, x))
                          : ($[x] ||
                              ($[x] =
                                pkgType === "go"
                                  ? { groups: {}, packages: {}, count: 0 }
                                  : { groups: {}, count: 0 }),
                            pkgType === "go" &&
                              !$[x].packages &&
                              ($[x].packages = {}),
                            $[x].groups || ($[x].groups = {}),
                            navigateToLeaf($[x].groups, P, $[x], x));
                      };
                      console.log(
                        `[loadPackagesForGroup] Navigating to group: ${group}, parts: ${pkgName.join(groupParts)}`,
                      );
                      const leaf = navigateToLeaf(groups, pkgName);
                      if (!leaf) {
                        (console.error(
                          `[loadPackagesForGroup] Failed to navigate to leaf for group: ${group}, parts: ${pkgName.join(groupParts)}`,
                        ),
                          console.error(
                            `[loadPackagesForGroup] Current tree at ${pkgName[0]}:`,
                            JSON.stringify(
                              this.packageTree[pkgType].groups[pkgName[0]],
                              null,
                              2,
                            ),
                          ));
                        return;
                      }
                      console.log(
                        `[loadPackagesForGroup] Successfully navigated to leaf, packageName: ${leaf.packageName}, node has packages:`,
                        !!leaf.node.packages,
                      );
                      let extractedPkgName = versionData.name || leaf.packageName || "N/A";
                      if (pkgType === "go" && versionData.name) extractedPkgName = versionData.name;
                      else {
                        const node = versionData.name || "";
                        if (node.includes("/")) {
                          const tail = node.split("/");
                          extractedPkgName = tail[tail.length - 1];
                        } else extractedPkgName = node;
                      }
                      (leaf.node.packages || (leaf.node.packages = {}),
                        console.log(
                          `[loadPackagesForGroup] Creating/updating package ${extractedPkgName} in node, leaf.packageName: ${leaf.packageName}, extractedPkgName: ${extractedPkgName}`,
                        ),
                        leaf.node.packages[extractedPkgName]
                          ? (leaf.node.packages[extractedPkgName].packages ||
                              (leaf.node.packages[extractedPkgName].packages = {}),
                            leaf.node.packages[extractedPkgName].versions ||
                              (leaf.node.packages[extractedPkgName].versions = []),
                            console.log(
                              `[loadPackagesForGroup] Package ${extractedPkgName} already exists, current versions: ${leaf.node.packages[extractedPkgName].versions.length}`,
                            ))
                          : ((leaf.node.packages[extractedPkgName] = {
                              versions: [],
                              packages: {},
                              loaded: false,
                            }),
                            console.log(
                              `[loadPackagesForGroup] Created new package entry for ${extractedPkgName}`,
                            )),
                        versionData.id ||
                          (versionData.id =
                            versionData.version +
                            (versionData.architecture ? `:${versionData.architecture}` : "")),
                        leaf.node.packages[extractedPkgName].versions.find(
                          ($) =>
                            ($.id ||
                              $.version +
                                ($.architecture
                                  ? `:${$.architecture}`
                                  : "")) === versionData.id,
                        )
                          ? console.log(
                              `[loadPackagesForGroup] Package ${versionData.name}@${versionData.version} already exists in ${extractedPkgName}`,
                            )
                          : (leaf.node.packages[extractedPkgName].versions.push(versionData),
                            addedCount++,
                            console.log(
                              `[loadPackagesForGroup] Added package ${versionData.name}@${versionData.version} to ${extractedPkgName} at path: ${group}, versions now: ${leaf.node.packages[extractedPkgName].versions.length}`,
                            )));
                    } else {
                      (this.packageTree[pkgType].groups[group] ||
                        (this.packageTree[pkgType].groups[group] = {
                          packages: {},
                          count: 0,
                        }),
                        this.packageTree[pkgType].groups[group].packages ||
                          (this.packageTree[pkgType].groups[group].packages = {}));
                      let pkgName = { ...versionData };
                      if (pkgType === "apk" && pkgName.name && pkgName.version) {
                        const navigateToLeaf = pkgName.name.split("-");
                        if (navigateToLeaf.length > 1) {
                          const leaf = navigateToLeaf[navigateToLeaf.length - 1];
                          leaf.match(/^\d/) &&
                            pkgName.version.match(/^r\d+$/) &&
                            ((pkgName.name = navigateToLeaf.slice(0, -1).join("-")),
                            (pkgName.version = leaf + "-" + pkgName.version));
                        }
                      }
                      const groups = pkgName.name || "N/A";
                      if (
                        (this.packageTree[pkgType].groups[group].packages[groups] ||
                          (this.packageTree[pkgType].groups[group].packages[groups] = {
                            versions: [],
                          }),
                        pkgType === "apk" && pkgName.version)
                      ) {
                        const navigateToLeaf = this.packageTree[pkgType].groups[group].packages[
                          groups
                        ].versions.find(
                          (T) => T.version === pkgName.version && T.name === pkgName.name,
                        );
                        if (navigateToLeaf) {
                          if (pkgName.architecture && navigateToLeaf.architecture) {
                            const leaf = navigateToLeaf.architecture
                              .split(",")
                              .map((v) => v.trim());
                            leaf.includes(pkgName.architecture) ||
                              (navigateToLeaf.architecture = [...leaf, pkgName.architecture].join(
                                ", ",
                              ));
                          } else
                            pkgName.architecture &&
                              !navigateToLeaf.architecture &&
                              (navigateToLeaf.architecture = pkgName.architecture);
                          (pkgName.size && !navigateToLeaf.size && (navigateToLeaf.size = pkgName.size),
                            pkgName.access_count &&
                              !navigateToLeaf.access_count &&
                              (navigateToLeaf.access_count = pkgName.access_count));
                          return;
                        }
                      }
                      (pkgName.id ||
                        (pkgName.id =
                          pkgName.version +
                          (pkgName.architecture ? `:${pkgName.architecture}` : "")),
                        this.packageTree[pkgType].groups[group].packages[groups].versions.find(
                          (w) => w.id === pkgName.id,
                        ) ||
                          (this.packageTree[pkgType].groups[group].packages[
                            groups
                          ].versions.push(pkgName),
                          addedCount++));
                    }
                  }));
              }),
                console.log(
                  `[loadPackagesForGroup] Added ${addedCount} packages to tree for ${type}:${groupPath}`,
                ));
              const sortPackages = (n) => {
                  if (!n || !n.packages) return;
                  const pkgType = Object.keys(n.packages).sort((h, f) =>
                      h.localeCompare(f),
                    ),
                    group = {};
                  (pkgType.forEach((h) => {
                    (n.packages[h].versions &&
                      n.packages[h].versions.sort((f, d) =>
                        (d.version || "").localeCompare(f.version || ""),
                      ),
                      (group[h] = n.packages[h]));
                  }),
                    (n.packages = group));
                },
                separator = "/";
              if (groupPath.includes(separator)) {
                const pkg = groupPath.split(separator);
                let pkgType = this.packageTree[type]?.groups;
                for (const group of pkg)
                  if (pkgType && pkgType[group]) ((pkgType = pkgType[group]), pkgType.groups && (pkgType = pkgType.groups));
                  else {
                    pkgType = null;
                    break;
                  }
                (pkgType && pkgType.packages && sortPackages(pkgType),
                  console.log(
                    `[loadPackagesForGroup] Tree structure at ${groupPath}:`,
                    pkgType,
                  ));
              } else
                (this.packageTree[type]?.groups[groupPath] &&
                  sortPackages(this.packageTree[type].groups[groupPath]),
                  console.log(
                    `[loadPackagesForGroup] Tree structure at ${groupPath}:`,
                    this.packageTree[type]?.groups[groupPath],
                  ));
            } else
              console.log(
                `[loadPackagesForGroup] No packages to add for ${type}:${groupPath}`,
              );
            this.loadedGroups[cacheKey] = true;
          } catch (a) {
            console.error(`Failed to load packages for group ${groupPath}:`, a);
          }
      },
      clearPackagesForGroup(type, groupPath) {
        (console.log(
          `[clearPackagesForGroup] No longer clearing packages for ${type}:${groupPath} as they are persistently in the tree.`,
        ),
          this.loadedGroups && delete this.loadedGroups[`${type}:${groupPath}`]);
      },
      buildPackageTree() {
        const tree = {};
        this.packages.forEach((u) => {
          const type = u.type || "unknown",
            name = u.name || "N/A";
          tree[type] || (tree[type] = { groups: {}, count: 0 });
          let group = u.group || "default";
          if (type === "apt" && u.group && u.group.includes(":")) {
            const parts = u.group.split(":").filter((p) => p && p.trim());
            if (parts.length === 0) group = "default";
            else {
              let groups = tree[type].groups;
              (!groups || typeof groups != "object") && ((groups = {}), (tree[type].groups = groups));
              let valid = true;
              for (let depth = 0; depth < parts.length; depth++) {
                const part = parts[depth];
                if (!(!part || !part.trim())) {
                  if (!groups || typeof groups != "object") {
                    valid = false;
                    break;
                  }
                  if (
                    ((!groups[part] || typeof groups[part] != "object" || groups[part] === null) &&
                      (groups[part] =
                        depth === parts.length - 1
                          ? { packages: {}, count: 0 }
                          : { groups: {}, count: 0 }),
                    depth === parts.length - 1)
                  )
                    (groups[part].packages || (groups[part].packages = {}),
                      typeof groups[part].count != "number" && (groups[part].count = 0),
                      groups[part].packages[name] || (groups[part].packages[name] = { versions: [] }),
                      groups[part].packages[name].versions.push(u),
                      groups[part].count++,
                      tree[type].count++);
                  else {
                    (groups[part].groups || (groups[part].groups = {}),
                      typeof groups[part].count != "number" && (groups[part].count = 0),
                      groups[part].count++);
                    const subGroups = groups[part].groups;
                    !subGroups || typeof subGroups != "object"
                      ? ((groups[part].groups = {}), (groups = groups[part].groups))
                      : (groups = subGroups);
                  }
                }
              }
              if (valid) return;
            }
          }
          tree[type].groups[group] || (tree[type].groups[group] = { packages: {}, count: 0 });
          let normalizedPkg = { ...u };
          if (type === "apk" && normalizedPkg.name && normalizedPkg.version) {
            const parts = normalizedPkg.name.split("-");
            if (parts.length > 1) {
              const groups = parts[parts.length - 1];
              groups.match(/^\d/) &&
                normalizedPkg.version.match(/^r\d+$/) &&
                ((normalizedPkg.name = parts.slice(0, -1).join("-")),
                (normalizedPkg.version = groups + "-" + normalizedPkg.version));
            }
          }
          const pkgName = normalizedPkg.name || name;
          if (
            (tree[type].groups[group].packages[pkgName] ||
              (tree[type].groups[group].packages[pkgName] = { versions: [] }),
            type === "apk" && normalizedPkg.version)
          ) {
            const parts = tree[type].groups[group].packages[pkgName].versions.find(
              (p) => p.version === normalizedPkg.version && p.name === normalizedPkg.name,
            );
            if (parts) {
              if (normalizedPkg.architecture && parts.architecture) {
                const groups = parts.architecture.split(",").map((n) => n.trim());
                groups.includes(normalizedPkg.architecture) ||
                  (parts.architecture = [...groups, normalizedPkg.architecture].join(", "));
              } else
                normalizedPkg.architecture &&
                  !parts.architecture &&
                  (parts.architecture = normalizedPkg.architecture);
              (normalizedPkg.size && !parts.size && (parts.size = normalizedPkg.size),
                normalizedPkg.access_count &&
                  !parts.access_count &&
                  (parts.access_count = normalizedPkg.access_count));
              return;
            }
          }
          (tree[type].groups[group].packages[pkgName].versions.push(normalizedPkg),
            tree[type].groups[group].count++,
            tree[type].count++);
        });
        const sortGroups = (u, a = false) => {
          Object.keys(u)
            .sort((c, o) => c.localeCompare(o))
            .forEach((c) => {
              const group = u[c];
              if (group.packages) {
                const normalizedPkg = Object.keys(group.packages).sort((i, p) =>
                    i.localeCompare(p),
                  ),
                  pkgName = {};
                (normalizedPkg.forEach((i) => {
                  (group.packages[i].versions.sort((p, n) =>
                    (n.version || "").localeCompare(p.version || ""),
                  ),
                    (pkgName[i] = group.packages[i]));
                }),
                  (group.packages = pkgName));
              } else
                group.groups &&
                  (sortGroups(group.groups, true),
                  (group.count = Object.values(group.groups).reduce(
                    (g, s) => g + (s.count || 0),
                    0,
                  )));
            });
        };
        (Object.keys(tree).forEach((u) => {
          sortGroups(tree[u].groups, u === "apt");
        }),
          (this.packageTree = tree),
          console.log("Package tree built:", JSON.stringify(tree, null, 2)));
      },
      buildPackageTreeFromGroups() {
        const deepCopy = (c) => {
            const typeData = {};
            return (Object.keys(c || {}).forEach((g) => {
              const groupData = c[g];
              ((typeData[g] = {
                groups: groupData.groups ? deepCopy(groupData.groups) : {},
                packages: {},
                count: groupData.count || 0,
                loaded: groupData.loaded || false,
              }),
                groupData.packages &&
                  Object.keys(groupData.packages).forEach((i) => {
                    typeData[g].packages[i] = {
                      ...groupData.packages[i],
                      versions: [...(groupData.packages[i].versions || [])],
                    };
                  }));
            }), typeData);
          },
          tree = {};
        (Object.keys(this.packageTree || {}).forEach((c) => {
          tree[c] = {
            groups: deepCopy(this.packageTree[c].groups),
            count: this.packageTree[c].count || 0,
          };
        }),
          Object.keys(this.packageGroups || {}).forEach((c) => {
            tree[c] || (tree[c] = { groups: {}, count: 0 });
            const typeData = this.packageGroups[c];
            if (c === "apt" && typeData.hierarchy) {
              const groupData = typeData.hierarchy;
              Object.keys(groupData).forEach((i) => {
                const processHierarchy = groupData[i],
                  key = processHierarchy.count || 0;
                (tree[c].groups[i]
                  ? (tree[c].groups[i].count = key)
                  : (tree[c].groups[i] = { groups: {}, count: key }),
                  Object.keys(processHierarchy).forEach((r) => {
                    if (r === "count") return;
                    const path = processHierarchy[r],
                      count = path.count || 0;
                    (tree[c].groups[i].groups[r]
                      ? (tree[c].groups[i].groups[r].count = count)
                      : (tree[c].groups[i].groups[r] = { groups: {}, count: count }),
                      Object.keys(path).forEach((f) => {
                        if (f === "count") return;
                        const subValue = path[f],
                          fullPath = `${i}:${r}:${f}`,
                          subCount = subValue.count || 0,
                          existingPackages = tree[c].groups[i]?.groups[r]?.groups[f]?.packages;
                        tree[c].groups[i].groups[r].groups[f]
                          ? ((tree[c].groups[i].groups[r].groups[f].count = subCount),
                            existingPackages
                              ? (tree[c].groups[i].groups[r].groups[f].packages =
                                  existingPackages)
                              : tree[c].groups[i].groups[r].groups[f].packages ||
                                (tree[c].groups[i].groups[r].groups[f].packages =
                                  {}))
                          : (tree[c].groups[i].groups[r].groups[f] = {
                              packages: existingPackages || {},
                              count: subCount,
                              loaded: this.loadedGroups[fullPath] || false,
                            });
                      }));
                  }));
              });
            }
            if (c === "go" && typeData.hierarchy) {
              const groupData = typeData.hierarchy,
                pkgName = new Set(),
                processHierarchy = (n, r, l = []) => {
                  Object.keys(n).forEach((h) => {
                    if (h === "count") return;
                    const subKey = n[h],
                      subValue = [...l, h],
                      fullPath = c === "go" ? "/" : ":",
                      subCount = subValue.join(fullPath);
                    if (
                      !Object.keys(subKey).some((y) => y !== "count") &&
                      subKey.count !== undefined
                    ) {
                      const existingPackages = h;
                      if (pkgName.has(existingPackages)) return;
                      (pkgName.add(existingPackages),
                        r.packages || (r.packages = {}),
                        r.packages[existingPackages]
                          ? r.packages[existingPackages].loaded ||
                            (r.packages[existingPackages].count = subKey.count || 0)
                          : (r.packages[existingPackages] = {
                              versions: [],
                              count: subKey.count || 0,
                              loaded: this.loadedGroups[subCount] || false,
                            }));
                    } else {
                      if (
                        (r.groups || (r.groups = {}),
                        r.packages && r.packages[h])
                      )
                        return;
                      r.groups[h]
                        ? (r.groups[h].count = subKey.count || 0)
                        : (r.groups[h] = {
                            groups: {},
                            packages: {},
                            count: subKey.count || 0,
                          });
                      const existingPackages = {};
                      (Object.keys(subKey).forEach((w) => {
                        w !== "count" && (existingPackages[w] = subKey[w]);
                      }),
                        Object.keys(existingPackages).length > 0 && processHierarchy(existingPackages, r.groups[h], subValue));
                    }
                  });
                };
              Object.keys(groupData).forEach((n) => {
                const value = groupData[n],
                  path = value.count || 0;
                (tree[c].groups[n]
                  ? (tree[c].groups[n].count = path)
                  : (tree[c].groups[n] = { groups: {}, packages: {}, count: path }),
                  processHierarchy(value, tree[c].groups[n], [n]));
              });
            }
            const groupName = new Set();
            (c === "go" &&
              typeData.hierarchy &&
              Object.keys(typeData.hierarchy).forEach((s) => {
                groupName.add(s);
              }),
              Object.keys(typeData).forEach((s) => {
                if (
                  s === "hierarchy" ||
                  (c === "go" && (groupName.has(s) || s.includes(":"))) ||
                  (c === "go" && tree[c].groups[s] && groupName.has(s))
                )
                  return;
                const pkgName = typeData[s],
                  processHierarchy = tree[c].groups[s]?.packages;
                tree[c].groups[s]
                  ? (groupName.has(s) || (tree[c].groups[s].count = pkgName.count || 0),
                    processHierarchy
                      ? (tree[c].groups[s].packages = processHierarchy)
                      : tree[c].groups[s].packages ||
                        (tree[c].groups[s].packages = {}))
                  : (tree[c].groups[s] = {
                      packages: processHierarchy || {},
                      count: pkgName.count || 0,
                      loaded: this.loadedGroups[s] || false,
                    });
              }));
          }));
        const recalcCounts = (c, o) => {
            Object.keys(c).forEach((g) => {
              const groupData = c[g];
              if (groupData.packages && groupData.loaded)
                groupData.count = Object.values(groupData.packages).reduce(
                  (i, p) => i + p.versions.length,
                  0,
                );
              else if (groupData.groups || groupData.packages) {
                if (
                  o === "go" &&
                  groupData.packages &&
                  (!groupData.groups || Object.keys(groupData.groups).length === 0) &&
                  groupData.count !== undefined &&
                  groupData.count > 0
                )
                  return;
                let pkgName = 0;
                groupData.groups &&
                  (recalcCounts(groupData.groups, o),
                  (pkgName = Object.values(groupData.groups)
                    .map((r) => r.count || 0)
                    .reduce((r, l) => r + l, 0)));
                let processHierarchy = 0;
                groupData.packages &&
                  typeof groupData.packages == "object" &&
                  (processHierarchy = Object.keys(groupData.packages).length);
                const key = pkgName + processHierarchy;
                (key > 0 ||
                  (groupData.groups && Object.keys(groupData.groups).length > 0) ||
                  (groupData.packages && Object.keys(groupData.packages).length > 0)) &&
                  (groupData.count = key);
              }
            });
          },
          countPackages = (c) => {
            let typeData = 0;
            return (c.packages &&
              typeof c.packages == "object" &&
              (typeData += Object.keys(c.packages).length), c.groups &&
              typeof c.groups == "object" &&
              (typeData += Object.values(c.groups).reduce((g, s) => g + countPackages(s), 0)), typeData);
          };
        (Object.keys(tree).forEach((c) => {
          recalcCounts(tree[c].groups, c);
          let typeData = 0;
          tree[c].groups &&
            typeof tree[c].groups == "object" &&
            (typeData = Object.values(tree[c].groups).reduce((s, i) => s + countPackages(i), 0));
          let groupName = 0;
          (tree[c].packages &&
            typeof tree[c].packages == "object" &&
            (groupName = Object.keys(tree[c].packages).length),
            (tree[c].count = groupName + typeData));
        }),
          (this.packageTree = tree));
      },
      toggleType(type) {
        this.expandedTypes || (this.expandedTypes = {});
        const current = this.expandedTypes[type] || false;
        this.expandedTypes[type] = !current;
      },
      autoExpandSingleItems() {
        // Auto-expand types, groups, and packages that have only 1 child
        this.expandedTypes || (this.expandedTypes = {});
        this.expandedGroups || (this.expandedGroups = {});
        this.expandedPackages || (this.expandedPackages = {});

        for (const type of ["apk", "apt", "npm", "pypi", "go"]) {
          const typeData = this.packageTree[type];
          if (!typeData) continue;

          const groupKeys = Object.keys(typeData.groups || {});
          const pkgKeys = Object.keys(typeData.packages || {});

          // Auto-expand type if it has only 1 group or 1 root package
          if (groupKeys.length === 1 && pkgKeys.length === 0) {
            this.expandedTypes[type] = true;
            const groupKey = groupKeys[0];
            const groupData = typeData.groups[groupKey];

            // Auto-expand the single group
            this.expandedGroups[`${type}:${groupKey}`] = true;

            // If group has only 1 package, auto-expand it
            const groupPkgKeys = Object.keys(groupData.packages || {});
            if (groupPkgKeys.length === 1) {
              const pkgKey = groupPkgKeys[0];
              const pkgData = groupData.packages[pkgKey];
              const pkgPath = pkgData.fullName || pkgKey;
              this.expandedPackages[`${type}:${pkgPath}`] = true;
            }
          } else if (pkgKeys.length === 1 && groupKeys.length === 0) {
            this.expandedTypes[type] = true;
            const pkgKey = pkgKeys[0];
            const pkgData = typeData.packages[pkgKey];
            const pkgPath = pkgData.fullName || pkgKey;
            this.expandedPackages[`${type}:${pkgPath}`] = true;
          }
        }
      },
      collapseSingleChildGroups() {
        // Collapse chains of single-child groups into combined paths (like GitHub)
        // e.g., github.com/ -> user/ -> repo/ becomes github.com/user/repo/
        // Also: dario.cat/ -> mergo becomes dario.cat/mergo (package promoted to root)
        for (const type of ["apk", "apt", "npm", "pypi", "go"]) {
          const typeData = this.packageTree[type];
          if (!typeData || !typeData.groups) continue;

          const separator = type === "go" ? "/" : "/";

          const collapseGroup = (groups, parentPackages = null) => {
            const groupKeys = Object.keys(groups);
            for (const key of groupKeys) {
              const group = groups[key];
              if (!group) continue;

              // Recursively collapse children first
              if (group.groups && Object.keys(group.groups).length > 0) {
                collapseGroup(group.groups, group.packages);
              }

              // Check if this group has exactly 1 child group and no packages
              const childGroupKeys = Object.keys(group.groups || {});
              const packageKeys = Object.keys(group.packages || {});

              if (childGroupKeys.length === 1 && packageKeys.length === 0) {
                const childKey = childGroupKeys[0];
                const childGroup = group.groups[childKey];

                // Merge: combine names and promote child's content
                // Avoid double separators (key may already end with separator)
                const needsSep = !key.endsWith(separator);
                const newKey = needsSep ? `${key}${separator}${childKey}` : `${key}${childKey}`;

                // Copy child's properties to parent
                group.groups = childGroup.groups || {};
                group.packages = childGroup.packages || {};
                // Keep the larger count (child should have accurate count)
                group.count = childGroup.count || group.count;

                // Delete old key and add new combined key in parent
                delete groups[key];
                groups[newKey] = group;

                // Continue collapsing in case there's another single child
                collapseGroup(groups, parentPackages);
                return; // Restart since we modified the object we're iterating
              }

              // If group has 0 child groups and exactly 1 package, mark it for template optimization
              // The template will render this as a single expandable item showing versions directly
              if (childGroupKeys.length === 0 && packageKeys.length === 1) {
                const pkgKey = packageKeys[0];
                const pkgData = group.packages[pkgKey];
                // Mark the group so template knows to render it specially
                group.isSinglePackage = true;
                group.singlePackageName = pkgKey;
                group.singlePackageData = pkgData;
              }
            }
          };

          collapseGroup(typeData.groups, typeData.packages);
        }
      },
      getPackageKey(type, pkg, groupPath, name) {
        return pkg && pkg.fullName
          ? `${type}:${pkg.fullName}`
          : groupPath && name
            ? `${type}:${groupPath}${type === "go" ? "/" : ":"}${name}`
            : `${type}:${name}`;
      },
      async togglePackage(type, name) {
        this.expandedPackages || (this.expandedPackages = {});
        const key = `${type}:${name}`,
          current = this.expandedPackages[key] || false;
        if (
          ((this.expandedPackages[key] = !current),
          console.log(`[togglePackage] ${type}:${name}, current: ${current}, key: ${key}`),
          !current && (type === "npm" || type === "pypi" || type === "go" || type === "apk"))
        ) {
          const typeGroup = this.packageTree[type];
          if (!typeGroup || !typeGroup.groups) {
            console.log(`[togglePackage] No typeGroup or groups for ${type}:${name}`);
            return;
          }
          const searchName = name;
          let pkgGroup = null,
            foundGroupPath = null;
          const findPackage = (found, r, l = []) => {
            for (const path in found) {
              const pkgName = found[path],
                pkgData = [...l, path];
              if (type === "go" && pkgName.packages)
                for (const separator in pkgName.packages) {
                  const subPkgName = pkgName.packages[separator];
                  if (r.startsWith(separator + "/")) {
                    const subPkg = r.substring(separator.length + 1);
                    if (subPkgName.packages && subPkgName.packages[subPkg]) {
                      console.log(
                        `[togglePackage] Found package ${r} as sub-package ${subPkg} of ${separator} at path: ${pkgData.join("/")}.packages.${separator}.packages`,
                      );
                      const w = type === "go" ? "/" : ":";
                      return ((foundGroupPath = pkgData.join(w)), subPkgName.packages[subPkg]);
                    }
                  }
                }
              if (pkgName.packages && pkgName.packages[r]) {
                console.log(
                  `[togglePackage] Found package ${r} at path: ${pkgData.join(".")}.packages`,
                );
                const separator = type === "go" ? "/" : ":";
                return (type === "go" ? (foundGroupPath = pkgData.join(separator)) : (foundGroupPath = [...pkgData, r].join(separator)), pkgName.packages[r]);
              }
              if (pkgName.groups) {
                const separator = findPackage(pkgName.groups, r, pkgData);
                if (separator) return separator;
              }
            }
            return null;
          };
          if (((pkgGroup = findPackage(typeGroup.groups, searchName)), !pkgGroup)) {
            (console.log(
              `[togglePackage] Package group not found for ${type}:${name}`,
            ),
              console.log(
                "[togglePackage] Tree structure:",
                JSON.stringify(this.packageTree[type], null, 2),
              ));
            return;
          }
          if ((pkgGroup._groupPath && (foundGroupPath = pkgGroup._groupPath), !pkgGroup.versions)) {
            console.log(
              `[togglePackage] Package group found but no versions for ${type}:${name}`,
            );
            return;
          }
          if (
            (console.log(
              `[togglePackage] Found ${pkgGroup.versions.length} versions for ${type}:${name}`,
            ),
            type === "go" &&
              (!pkgGroup.versions || pkgGroup.versions.length === 0) &&
              !pkgGroup.loaded &&
              foundGroupPath)
          ) {
            (console.log(
              `[togglePackage] Loading packages for group ${foundGroupPath} (package ${name} has no versions)`,
            ),
              await this.loadPackagesForGroup(type, foundGroupPath));
            const found = findPackage(typeGroup.groups, searchName);
            if (found && found.versions && found.versions.length > 0)
              ((pkgGroup = found),
                console.log(
                  `[togglePackage] After loading, found ${pkgGroup.versions.length} versions for ${type}:${name}`,
                ));
            else {
              console.log(
                `[togglePackage] After loading, still no versions for ${type}:${name}`,
              );
              return;
            }
          }
          const metadataPromises = [];
          for (const found of pkgGroup.versions)
            found.metadata ||
              found.loadingMetadata ||
              (type === "npm" && (!found.cached || !found.name || !found.version)) ||
              ((type === "pypi" || type === "go" || type === "apk") &&
                (!found.name || !found.version)) ||
              (type === "npm"
                ? metadataPromises.push(this.loadNPMMetadata(found, null))
                : type === "pypi"
                  ? metadataPromises.push(this.loadPyPIMetadata(found, null))
                  : type === "go"
                    ? metadataPromises.push(this.loadGoMetadata(found, null))
                    : type === "apk" && metadataPromises.push(this.loadAPKMetadata(found, null)));
          metadataPromises.length > 0 &&
            Promise.all(metadataPromises).catch((found) => {
              console.error(
                `Failed to load metadata for some versions of ${name}:`,
                found,
              );
            });
        }
      },
      async toggleGroup(type, groupName) {
        this.expandedGroups || (this.expandedGroups = {});
        const key = `${type}:${groupName}`,
          current = this.expandedGroups[key] || false;
        this.expandedGroups[key] = !current;
      },
      async loadNPMMetadata(pkg, event) {
        if (
          pkg.metadata ||
          pkg.loadingMetadata ||
          !pkg.cached ||
          !pkg.name ||
          !pkg.version
        )
          return;
        event && (event.preventDefault(), event.stopPropagation());
        const row = event?.target?.closest(".px-4.py-2"),
          scrollContainer = document.querySelector(".packages-table") || window,
          scrollTop = scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop,
          rowTop = row
            ? row.getBoundingClientRect().top +
              (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop)
            : null;
        pkg.loadingMetadata = true;
        try {
          let url = "/api/v1/packages/npm/";
          if (pkg.name.startsWith("@")) {
            const currentRow = pkg.name.split("/");
            currentRow.length === 2
              ? (url += encodeURIComponent(currentRow[0]) + "/" + encodeURIComponent(currentRow[1]))
              : (url += encodeURIComponent(pkg.name));
          } else url += encodeURIComponent(pkg.name);
          url += "/" + encodeURIComponent(pkg.version) + "/metadata";
          const res = await fetch(url);
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const metadata = await res.json();
          ((pkg.metadata = metadata),
            !pkg.description && metadata.description && (pkg.description = metadata.description),
            (!pkg.dependencies || pkg.dependencies.length === 0) &&
              metadata.dependencies &&
              typeof metadata.dependencies == "object" &&
              !Array.isArray(metadata.dependencies) &&
              (pkg.dependencies = Object.keys(metadata.dependencies)),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              if (rowTop !== null && row) {
                const currentRow = row;
                if (currentRow) {
                  const diff =
                    currentRow.getBoundingClientRect().top +
                    (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop) -
                    rowTop;
                  scrollContainer === window
                    ? window.scrollTo(0, scrollTop + diff)
                    : (scrollContainer.scrollTop = scrollTop + diff);
                } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
              } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } catch (g) {
          (console.error(
            `Failed to load metadata for ${pkg.name}@${pkg.version}:`,
            g,
          ),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } finally {
          pkg.loadingMetadata = false;
        }
      },
      async loadPyPIMetadata(pkg, event) {
        if (pkg.metadata || pkg.loadingMetadata || !pkg.name || !pkg.version) return;
        event && (event.preventDefault(), event.stopPropagation());
        const row = event?.target?.closest(".px-4.py-2"),
          scrollContainer = document.querySelector(".packages-table") || window,
          scrollTop = scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop,
          rowTop = row
            ? row.getBoundingClientRect().top +
              (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop)
            : null;
        pkg.loadingMetadata = true;
        try {
          const url = `/api/v1/packages/pypi/${encodeURIComponent(pkg.name)}/${encodeURIComponent(pkg.version)}/metadata`,
            res = await fetch(url);
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const metadata = await res.json();
          ((pkg.metadata = metadata),
            !pkg.description && metadata.description && (pkg.description = metadata.description),
            (!pkg.dependencies || pkg.dependencies.length === 0) &&
              metadata.dependencies &&
              Array.isArray(metadata.dependencies) &&
              (pkg.dependencies = metadata.dependencies),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              if (rowTop !== null && row) {
                const currentRow = row;
                if (currentRow) {
                  const diff =
                    currentRow.getBoundingClientRect().top +
                    (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop) -
                    rowTop;
                  scrollContainer === window
                    ? window.scrollTo(0, scrollTop + diff)
                    : (scrollContainer.scrollTop = scrollTop + diff);
                } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
              } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } catch (g) {
          (console.error(
            `Failed to load metadata for ${pkg.name}@${pkg.version}:`,
            g,
          ),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } finally {
          pkg.loadingMetadata = false;
        }
      },
      async loadGoMetadata(pkg, event) {
        if (pkg.metadata || pkg.loadingMetadata || !pkg.name || !pkg.version) return;
        event && (event.preventDefault(), event.stopPropagation());
        const row = event?.target?.closest(".px-4.py-2"),
          scrollContainer = document.querySelector(".packages-table") || window,
          scrollTop = scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop,
          rowTop = row
            ? row.getBoundingClientRect().top +
              (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop)
            : null;
        pkg.loadingMetadata = true;
        try {
          const url = `/api/v1/packages/go/${encodeURIComponent(pkg.name)}/${encodeURIComponent(pkg.version)}/metadata`,
            res = await fetch(url);
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const metadata = await res.json();
          ((pkg.metadata = metadata),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              if (rowTop !== null && row) {
                const currentRow = row;
                if (currentRow) {
                  const diff =
                    currentRow.getBoundingClientRect().top +
                    (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop) -
                    rowTop;
                  scrollContainer === window
                    ? window.scrollTo(0, scrollTop + diff)
                    : (scrollContainer.scrollTop = scrollTop + diff);
                } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
              } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } catch (g) {
          (console.error(
            `Failed to load metadata for ${pkg.name}@${pkg.version}:`,
            g,
          ),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } finally {
          pkg.loadingMetadata = false;
        }
      },
      async loadAPKMetadata(pkg, event) {
        if (pkg.metadata || pkg.loadingMetadata || !pkg.name || !pkg.version) return;
        event && (event.preventDefault(), event.stopPropagation());
        const row = event?.target?.closest(".px-4.py-2"),
          scrollContainer = document.querySelector(".packages-table") || window,
          scrollTop = scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop,
          rowTop = row
            ? row.getBoundingClientRect().top +
              (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop)
            : null;
        pkg.loadingMetadata = true;
        try {
          const url = `/api/v1/packages/apk/${encodeURIComponent(pkg.name)}/${encodeURIComponent(pkg.version)}/metadata`,
            res = await fetch(url);
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const metadata = await res.json();
          ((pkg.metadata = metadata),
            !pkg.description && metadata.description && (pkg.description = metadata.description),
            (!pkg.dependencies || pkg.dependencies.length === 0) &&
              metadata.dependencies &&
              Array.isArray(metadata.dependencies) &&
              (pkg.dependencies = metadata.dependencies),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              if (rowTop !== null && row) {
                const currentRow = row;
                if (currentRow) {
                  const diff =
                    currentRow.getBoundingClientRect().top +
                    (scrollContainer === window ? window.pageYOffset : scrollContainer.scrollTop) -
                    rowTop;
                  scrollContainer === window
                    ? window.scrollTo(0, scrollTop + diff)
                    : (scrollContainer.scrollTop = scrollTop + diff);
                } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
              } else scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } catch (g) {
          (console.error(
            `Failed to load metadata for ${pkg.name}@${pkg.version}:`,
            g,
          ),
            await this.$nextTick(),
            requestAnimationFrame(() => {
              scrollContainer === window ? window.scrollTo(0, scrollTop) : (scrollContainer.scrollTop = scrollTop);
            }));
        } finally {
          pkg.loadingMetadata = false;
        }
      },
      async loadDockerOverrides() {
        try {
          const res = await fetch("/api/v1/overrides");
          if (res.ok) {
            this.dockerOverrides = await res.json();
          } else {
            this.dockerOverrides = [];
          }
        } catch (err) {
          console.warn("[loadDockerOverrides] Failed:", err);
          this.dockerOverrides = [];
        }
        this.renderOverridesTable();
      },
      async deleteDockerOverride(source) {
        if (!confirm(`Remove override for "${source}"?`)) return;
        try {
          const res = await fetch("/api/v1/overrides/" + encodeURIComponent(source), { method: "DELETE" });
          if (res.ok || res.status === 204) {
            this.dockerOverrides = this.dockerOverrides.filter(o => o.source !== source);
            this.renderOverridesTable();
          } else {
            alert("Failed to delete override: " + res.statusText);
          }
        } catch (err) {
          alert("Error deleting override: " + err.message);
        }
      },
      renderOverridesTable() {
        const container = document.getElementById("docker-overrides-container");
        if (!container) return;
        if (!this.dockerOverrides || this.dockerOverrides.length === 0) {
          container.innerHTML = "";
          container.style.display = "none";
          return;
        }
        const esc = RoxxyUtils.escapeHtml;
        let html = '<div class="bg-white rounded-lg shadow overflow-hidden">';
        html += '<div class="px-4 py-3 border-b border-gray-200 flex items-center justify-between">';
        html += '<div class="flex items-center space-x-2">';
        html += '<i class="ti ti-arrows-right-left icon text-blue-600"></i>';
        html += '<h3 class="text-sm font-semibold text-gray-700">Image Overrides</h3>';
        html += '<span class="text-xs text-gray-400 font-normal">Requests for source images are transparently served from the target instead</span>';
        html += '</div></div>';
        html += '<table class="min-w-full divide-y divide-gray-200">';
        html += '<thead class="bg-gray-50"><tr>';
        html += '<th class="px-4 py-2 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Source</th>';
        html += '<th class="px-4 py-2 text-left text-xs font-medium text-gray-500 uppercase tracking-wider"></th>';
        html += '<th class="px-4 py-2 text-left text-xs font-medium text-gray-500 uppercase tracking-wider">Target</th>';
        html += '<th class="px-4 py-2 text-right text-xs font-medium text-gray-500 uppercase tracking-wider"></th>';
        html += '</tr></thead><tbody class="bg-white divide-y divide-gray-200">';
        this.dockerOverrides.forEach((override) => {
          html += '<tr class="hover:bg-gray-50">';
          html += `<td class="px-4 py-2 whitespace-nowrap"><code class="text-sm text-gray-900 bg-gray-100 px-2 py-0.5 rounded">${esc(override.source)}</code></td>`;
          html += '<td class="px-2 py-2 text-gray-400"><i class="ti ti-arrow-right icon"></i></td>';
          html += `<td class="px-4 py-2 whitespace-nowrap"><code class="text-sm text-blue-700 bg-blue-50 px-2 py-0.5 rounded">${esc(override.target)}</code></td>`;
          html += `<td class="px-4 py-2 text-right"><button onclick="window.alpineApp.deleteDockerOverride('${esc(override.source.replace(/'/g, "\\'"))}')" class="text-red-400 hover:text-red-600 transition-colors" title="Remove override"><i class="ti ti-trash icon"></i></button></td>`;
          html += '</tr>';
        });
        html += '</tbody></table></div>';
        container.innerHTML = html;
        container.style.display = "block";
      },
      async loadDockerRepos() {
        (console.log("[loadDockerRepos] Starting..."),
          (this.loadingDocker = true));
        this.loadDockerOverrides();
        const skeleton = document.getElementById("docker-loading-skeleton"),
          container = document.getElementById("docker-table-container"),
          wrapper = document.getElementById("docker-tab-wrapper");
        (console.log("[loadDockerRepos] Elements found:", {
          skeleton: !!skeleton,
          container: !!container,
          wrapper: !!wrapper,
          wrapperVisible: wrapper
            ? window.getComputedStyle(wrapper).display !== "none"
            : false,
        }),
          skeleton && (skeleton.style.display = "block"),
          container && (container.style.display = "none"));
        try {
          const res = await fetch("/api/v1/docker/repositories");
          if (!res.ok) throw new Error(`HTTP ${res.status}: ${res.statusText}`);
          const data = await res.json();
          (console.log("[loadDockerRepos] API response:", data),
            (this.dockerRepos = data.repositories || []),
            console.log(
              "[loadDockerRepos] dockerRepos set, length:",
              this.dockerRepos.length,
            ),
            console.log(
              "[loadDockerRepos] CALLING renderDockerTableDirectly NOW",
            ),
            typeof this.renderDockerTableDirectly == "function"
              ? (this.renderDockerTableDirectly(),
                console.log(
                  "[loadDockerRepos] renderDockerTableDirectly returned",
                ))
              : console.error(
                  "[loadDockerRepos] renderDockerTableDirectly is not a function!",
                ));
        } catch (a) {
          (console.error("[loadDockerRepos] Failed to load Docker repos:", a),
            (this.dockerRepos = []),
            container &&
              ((container.innerHTML =
                '<div class="bg-white rounded-lg shadow p-6"><p class="text-center text-sm text-red-500">Error loading Docker repositories</p></div>'),
              (container.style.display = "block")));
        } finally {
          ((this.loadingDocker = false),
            console.log(
              "[loadDockerRepos] Loading complete, loadingDocker:",
              this.loadingDocker,
            ),
            skeleton && (skeleton.style.display = "none"),
            container && (container.style.display = "block"));
        }
      },
      renderDockerTableDirectly() {
        console.log("[renderDockerTableDirectly] Starting, dockerRepos.length:", this.dockerRepos.length);
        const container = document.getElementById("docker-table-container");
        if (!container) {
          console.error("[renderDockerTableDirectly] docker-table-container not found!");
          return;
        }
        console.log("[renderDockerTableDirectly] Container found");
        container.innerHTML = "";
        if (this.dockerRepos.length === 0) {
          container.innerHTML = '<div class="bg-white rounded-lg shadow p-6"><p class="text-center text-sm text-gray-500">No Docker repositories found</p></div>';
          container.style.setProperty("display", "block", "important");
          return;
        }

        const esc = RoxxyUtils.escapeHtml;
        const fmt = RoxxyUtils.formatBytes;

        // Group repos by host/namespace
        const grouped = {};
        this.dockerRepos.forEach((repo) => {
          const name = repo.name || "";
          const parts = name.split("/");
          let host, namespace, repoName;
          if (parts.length >= 3) {
            host = parts[0];
            namespace = parts[1];
            repoName = parts.slice(2).join("/");
          } else if (parts.length === 2) {
            host = parts[0];
            namespace = parts[1];
            repoName = "";
          } else {
            host = "other";
            namespace = "default";
            repoName = name;
          }
          if (namespace === "library" && repoName === "") {
            repoName = parts[1] || name;
            namespace = "library";
          }
          grouped[host] = grouped[host] || {};
          grouped[host][namespace] = grouped[host][namespace] || [];
          grouped[host][namespace].push({
            name: repoName || name,
            fullName: name,
            tagCount: repo.tag_count || 0,
            size: repo.size || 0,
            date: repo.date || "",
            slices: repo.slices || repo.tag_count || 0,
            license: repo.license || "",
            description: repo.description || "",
          });
        });

        // Sort repos based on sort settings
        const sortBy = this.dockerSortBy || 'name';
        const sortOrder = this.dockerSortOrder || 'asc';
        const asc = sortOrder !== 'desc';

        const sortedHosts = Object.keys(grouped).sort();
        sortedHosts.forEach((host) => {
          Object.keys(grouped[host]).sort().forEach((ns) => {
            grouped[host][ns].sort((a, b) => {
              let cmp = 0;
              switch (sortBy) {
                case 'size':
                  cmp = a.size - b.size;
                  break;
                case 'date':
                  cmp = (a.date || '').localeCompare(b.date || '');
                  break;
                case 'tag_count':
                  cmp = a.tagCount - b.tagCount;
                  break;
                case 'name':
                default:
                  cmp = a.name.localeCompare(b.name);
              }
              return asc ? cmp : -cmp;
            });
          });
        });

        // Use same level colors as packages
        const getLevelColor = (d) => this.levelColors[Math.min(d, this.levelColors.length - 1)];

        let html = '<div class="bg-white rounded-lg shadow overflow-hidden p-4">';
        sortedHosts.forEach((host) => {
          const namespaces = Object.keys(grouped[host]).sort();
          const repoCount = namespaces.reduce((sum, ns) => sum + grouped[host][ns].length, 0);
          const hostExpanded = this.expandedDockerHosts[host];

          // Host level (depth 0)
          html += `<div class="mb-2">`;
          html += `<button onclick="window.alpineApp.toggleDockerHost('${esc(host.replace(/'/g, "\\'"))}')" class="w-full flex items-center justify-between p-3 rounded-lg hover:bg-blue-50 border border-transparent hover:border-blue-200 transition-all text-left">`;
          html += `<div class="flex items-center space-x-2">`;
          html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform ${hostExpanded ? 'rotate-90' : ''}"></i>`;
          html += `<i class="ti ti-server icon text-gray-600"></i>`;
          html += `<span class="text-gray-700 font-medium">${esc(host)}</span>`;
          html += `</div>`;
          html += `<span class="text-gray-900 font-semibold text-lg">${repoCount}</span>`;
          html += `</button>`;
          html += `<div class="${hostExpanded ? '' : 'hidden'}">`;

          namespaces.forEach((ns) => {
            const nsKey = `${host}/${ns}`;
            const repos = grouped[host][ns];
            const nsExpanded = this.expandedDockerNamespaces[nsKey];
            const depth = 1;
            const indent = depth * 1.5;
            const color = getLevelColor(depth);

            // Namespace level (depth 1)
            html += `<div class="${color}" style="padding-left:${indent}rem">`;
            html += `<button onclick="window.alpineApp.toggleDockerNamespace('${esc(nsKey.replace(/'/g, "\\'"))}')" class="w-full flex items-center space-x-2 px-3 py-2 hover:brightness-95 text-left">`;
            html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform ${nsExpanded ? 'rotate-90' : ''}"></i>`;
            html += `<i class="ti ti-folder icon text-gray-500"></i>`;
            html += `<span class="font-medium text-gray-800">${esc(ns)}</span>`;
            html += `<span class="text-sm text-gray-500">(${repos.length})</span>`;
            html += `</button>`;
            html += `<div class="${nsExpanded ? '' : 'hidden'}">`;

            // Render repos (depth 2)
            repos.forEach((repo) => {
              const isExpanded = this.expandedDockerRepos[repo.fullName];
              const tags = this.dockerRepoTags[repo.fullName] || [];
              const isLoading = this.loadingDockerTags[repo.fullName];
              const repoDepth = 2;
              const repoIndent = repoDepth * 1.5;
              const repoColor = getLevelColor(repoDepth);

              html += `<div class="${repoColor}" style="padding-left:${repoIndent}rem">`;
              html += `<button onclick="window.alpineApp.toggleDockerRepo('${esc(repo.fullName.replace(/'/g, "\\'"))}')" class="w-full flex items-center space-x-2 px-3 py-1.5 hover:brightness-95 text-left">`;
              html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform text-xs ${isExpanded ? 'rotate-90' : ''}"></i>`;
              html += `<i class="ti ti-brand-docker icon text-blue-400 text-sm"></i>`;
              html += `<span class="text-sm font-medium text-gray-700">${esc(repo.name)}</span>`;
              html += `<span class="text-xs text-gray-500">(${repo.tagCount} tag${repo.tagCount !== 1 ? 's' : ''})</span>`;
              html += `</button>`;

              // Expanded tag details (depth 3)
              html += `<div class="${isExpanded ? '' : 'hidden'} space-y-1 pb-1" style="padding-left:${(repoDepth + 1) * 1.5}rem">`;

              // Repo-level metadata container (like packages)
              if (isExpanded && (repo.description || repo.license || repo.size > 0 || repo.slices > 0)) {
                html += `<div class="px-3 py-1 text-xs bg-blue-50 border border-blue-100 rounded mb-1 text-gray-600">`;
                if (repo.description) html += `<div>${esc(repo.description.substring(0, 200))}${repo.description.length > 200 ? '...' : ''}</div>`;
                const metaParts = [];
                if (repo.license) metaParts.push(`License: ${esc(repo.license)}`);
                if (repo.size > 0) metaParts.push(fmt(repo.size));
                if (repo.slices > 0) metaParts.push(`${repo.slices} layers`);
                if (metaParts.length > 0) html += `<div>${metaParts.join(' · ')}</div>`;
                html += `</div>`;
              }
              if (isLoading) {
                html += `<div class="px-3 py-2 text-sm text-gray-400"><i class="ti ti-loader icon animate-spin mr-2"></i>Loading tags...</div>`;
              } else if (isExpanded && tags.length === 0) {
                html += `<div class="px-3 py-2 text-sm text-gray-400">No tags found</div>`;
              } else if (isExpanded) {
                // Select all button for this repo (only in selection mode)
                if (this.selectionMode && tags.length > 0) {
                  const allSelected = tags.every(t => {
                    const tName = typeof t === 'string' ? t : t.tag;
                    return this.selectedDockerItems.some(i => i.repo === repo.fullName && i.tag === tName);
                  });
                  html += `<button onclick="event.stopPropagation(); window.alpineApp.selectAllDockerRepoTags('${esc(repo.fullName.replace(/'/g, "\\'"))}')" class="mb-1 px-2 py-1 text-xs ${allSelected ? 'bg-blue-100 text-blue-700' : 'bg-gray-100 text-gray-600'} hover:bg-blue-200 rounded flex items-center gap-1">`;
                  html += `<i class="ti ti-checkbox icon text-xs"></i>`;
                  html += allSelected ? 'Deselect All' : 'Select All';
                  html += `</button>`;
                }
                // Sort tags based on current sort settings
                const sortedTags = [...tags].sort((a, b) => {
                  const aName = typeof a === 'string' ? a : a.tag;
                  const bName = typeof b === 'string' ? b : b.tag;
                  let cmp = 0;
                  switch (this.dockerSortBy) {
                    case 'size':
                      cmp = (a.size || 0) - (b.size || 0);
                      break;
                    case 'date':
                      cmp = new Date(a.date || 0) - new Date(b.date || 0);
                      break;
                    case 'name':
                    default:
                      // Use semantic version comparison
                      cmp = RoxxyUtils.compareVersions(aName, bName);
                      break;
                  }
                  return this.dockerSortOrder === 'desc' ? -cmp : cmp;
                });
                // Render tags like package versions
                for (const tag of sortedTags) {
                  const tagName = typeof tag === 'string' ? tag : tag.tag;
                  const tagDigest = tag.digest ? tag.digest.substring(7, 19) : '';
                  const tagSize = tag.size ? fmt(tag.size) : '';
                  const tagDate = tag.date || '';
                  const tagLayers = tag.layers || 0;
                  const isTagSelected = this.selectedDockerItems.some(i => i.repo === repo.fullName && i.tag === tagName);

                  html += `<div class="flex flex-wrap items-center gap-2 px-3 py-2 text-sm bg-white border border-gray-100 rounded hover:bg-gray-50 group">`;
                  // Selection checkbox (only in selection mode)
                  if (this.selectionMode) {
                    html += `<input type="checkbox" class="rounded border-gray-300 text-blue-600 cursor-pointer" ${isTagSelected ? 'checked' : ''} onclick="event.stopPropagation(); window.alpineApp.toggleDockerSelection('${esc(repo.fullName.replace(/'/g, "\\'"))}', '${esc(tagName.replace(/'/g, "\\'"))}')">`;
                  }
                  html += `<span class="font-medium text-gray-900">${esc(tagName)}</span>`;
                  if (tagSize) html += `<span class="text-xs text-gray-500">${tagSize}</span>`;
                  if (tagLayers) html += `<span class="text-xs text-gray-400">${tagLayers} layers</span>`;
                  if (tagDigest) html += `<span class="text-xs text-gray-400 font-mono">${esc(tagDigest)}...</span>`;
                  if (tagDate) html += `<span class="text-xs text-gray-400">${tagDate}</span>`;
                  html += `<button onclick="event.stopPropagation(); window.alpineApp.deleteDockerTag('${esc(repo.fullName.replace(/'/g, "\\'"))}', '${esc(tagName.replace(/'/g, "\\'"))}')" class="ml-auto opacity-0 group-hover:opacity-100 transition-opacity p-1 text-red-400 hover:text-red-600 hover:bg-red-50 rounded" title="Delete tag"><i class="ti ti-trash icon text-sm"></i></button>`;
                  html += `</div>`;
                }
              }
              html += `</div></div>`;
            });

            html += `</div></div>`;
          });

          html += `</div></div>`;
        });
        html += '</div>';

        container.innerHTML = html;
        container.style.removeProperty("display");
        container.style.setProperty("display", "block", "important");
        container.style.setProperty("visibility", "visible", "important");

        const skeleton = document.getElementById("docker-loading-skeleton");
        if (skeleton) skeleton.style.setProperty("display", "none", "important");
        console.log("[renderDockerTableDirectly] HTML rendered, length:", html.length);
      },
      // URL Cache methods
      async loadURLCache() {
        this.loadingURLCache = true;
        try {
          const res = await fetch('/api/v1/cache/url');
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          const data = await res.json();
          this.urlCacheEntries = data.entries || [];
          this.urlCacheTotal = data.total || 0;
          this.renderURLCacheTable();
        } catch (e) {
          console.error('Failed to load URL cache:', e);
          this.urlCacheEntries = [];
          this.urlCacheTotal = 0;
        } finally {
          this.loadingURLCache = false;
        }
      },
      renderURLCacheTable() {
        const container = document.getElementById('url-cache-container');
        if (!container) return;

        const esc = RoxxyUtils.escapeHtml;
        const fmt = RoxxyUtils.formatBytes;

        // Filter by search
        let entries = this.urlCacheEntries;
        if (this.urlCacheSearch) {
          const search = this.urlCacheSearch.toLowerCase();
          entries = entries.filter(e =>
            e.Hostname.toLowerCase().includes(search) ||
            e.Path.toLowerCase().includes(search)
          );
        }

        // Sort entries
        const sortBy = this.urlCacheSortBy;
        const asc = this.urlCacheSortOrder !== 'desc';
        entries = [...entries].sort((a, b) => {
          let cmp = 0;
          switch (sortBy) {
            case 'hostname':
              cmp = a.Hostname.localeCompare(b.Hostname);
              if (cmp === 0) cmp = a.Path.localeCompare(b.Path);
              break;
            case 'path':
              cmp = a.Path.localeCompare(b.Path);
              break;
            case 'size':
              cmp = (a.Size || 0) - (b.Size || 0);
              break;
            case 'cached_at':
              cmp = new Date(a.CachedAt || 0) - new Date(b.CachedAt || 0);
              break;
            case 'last_accessed':
              cmp = new Date(a.LastAccessed || 0) - new Date(b.LastAccessed || 0);
              break;
            case 'access_count':
              cmp = (a.AccessCount || 0) - (b.AccessCount || 0);
              break;
          }
          return asc ? cmp : -cmp;
        });

        if (entries.length === 0) {
          container.innerHTML = '<p class="text-gray-500 text-sm text-center py-4">No cached URLs found</p>';
          return;
        }

        // Group by hostname
        const byHost = {};
        for (const entry of entries) {
          if (!byHost[entry.Hostname]) byHost[entry.Hostname] = [];
          byHost[entry.Hostname].push(entry);
        }

        const hosts = Object.keys(byHost).sort();
        let html = '<div class="space-y-1">';

        for (const host of hosts) {
          const hostEntries = byHost[host];
          const isExpanded = this.expandedHosts[host];
          const hostSize = hostEntries.reduce((sum, e) => sum + (e.Size || 0), 0);

          // Host row
          html += `<div class="bg-purple-100">`;
          html += `<button onclick="window.alpineApp.toggleURLCacheHost('${esc(host)}')" class="w-full flex items-center space-x-2 px-3 py-2 hover:bg-purple-200 text-left">`;
          html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform text-sm ${isExpanded ? 'rotate-90' : ''}"></i>`;
          html += `<i class="ti ti-world icon text-purple-600 text-sm"></i>`;
          html += `<span class="font-medium text-gray-800">${esc(host)}</span>`;
          html += `<span class="text-sm text-gray-500">(${hostEntries.length} URLs, ${fmt(hostSize)})</span>`;
          html += `</button>`;

          // Host entries (expanded)
          html += `<div class="${isExpanded ? '' : 'hidden'}" style="padding-left: 1.5rem">`;

          // Select all for host (in selection mode)
          if (this.selectionMode && hostEntries.length > 0) {
            const allSelected = hostEntries.every(e =>
              this.selectedURLCacheItems.some(s => s.hostname === e.Hostname && s.path === e.Path)
            );
            html += `<button onclick="event.stopPropagation(); window.alpineApp.selectAllURLCacheHost('${esc(host)}')" class="mb-1 px-2 py-1 text-xs ${allSelected ? 'bg-blue-100 text-blue-700' : 'bg-gray-100 text-gray-600'} hover:bg-blue-200 rounded flex items-center gap-1">`;
            html += `<i class="ti ti-checkbox icon text-xs"></i>`;
            html += allSelected ? 'Deselect All' : 'Select All';
            html += `</button>`;
          }

          for (const entry of hostEntries) {
            const isSelected = this.selectedURLCacheItems.some(s => s.hostname === entry.Hostname && s.path === entry.Path);
            const cachedDate = entry.CachedAt ? new Date(entry.CachedAt).toLocaleDateString() : '';
            const lastAccessed = entry.LastAccessed ? new Date(entry.LastAccessed).toLocaleDateString() : '';

            html += `<div class="flex flex-wrap items-center gap-2 px-3 py-2 text-sm bg-white border border-gray-100 rounded hover:bg-gray-50 group mb-1">`;
            if (this.selectionMode) {
              html += `<input type="checkbox" class="rounded border-gray-300 text-blue-600 cursor-pointer" ${isSelected ? 'checked' : ''} onclick="event.stopPropagation(); window.alpineApp.toggleURLCacheSelection('${esc(entry.Hostname)}', '${esc(entry.Path.replace(/'/g, "\\'"))}')">`;
            }
            html += `<span class="font-mono text-xs text-gray-700 truncate max-w-md" title="${esc(entry.Path)}">${esc(entry.Path.length > 60 ? '...' + entry.Path.slice(-57) : entry.Path)}</span>`;
            if (entry.Size) html += `<span class="text-xs text-gray-500">${fmt(entry.Size)}</span>`;
            if (entry.ContentType) html += `<span class="text-xs text-gray-400">${esc(entry.ContentType.split(';')[0])}</span>`;
            if (entry.AccessCount) html += `<span class="text-xs text-gray-400">${entry.AccessCount} hits</span>`;
            if (cachedDate) html += `<span class="text-xs text-gray-400">cached ${cachedDate}</span>`;
            html += `<button onclick="event.stopPropagation(); window.alpineApp.deleteURLCacheEntry('${esc(entry.Hostname)}', '${esc(entry.Path.replace(/'/g, "\\'"))}')" class="ml-auto opacity-0 group-hover:opacity-100 transition-opacity p-1 text-red-400 hover:text-red-600 hover:bg-red-50 rounded" title="Delete"><i class="ti ti-trash icon text-sm"></i></button>`;
            html += `</div>`;
          }

          html += `</div></div>`;
        }

        html += '</div>';
        container.innerHTML = html;
      },
      toggleURLCacheHost(host) {
        this.expandedHosts[host] = !this.expandedHosts[host];
        this.renderURLCacheTable();
      },
      toggleURLCacheSortOrder() {
        this.urlCacheSortOrder = this.urlCacheSortOrder === 'asc' ? 'desc' : 'asc';
        this.renderURLCacheTable();
      },
      toggleURLCacheSelection(hostname, path) {
        const idx = this.selectedURLCacheItems.findIndex(i => i.hostname === hostname && i.path === path);
        if (idx === -1) {
          this.selectedURLCacheItems.push({ hostname, path });
        } else {
          this.selectedURLCacheItems.splice(idx, 1);
        }
      },
      selectAllURLCacheHost(host) {
        const hostEntries = this.urlCacheEntries.filter(e => e.Hostname === host);
        const allSelected = hostEntries.every(e =>
          this.selectedURLCacheItems.some(s => s.hostname === e.Hostname && s.path === e.Path)
        );

        if (allSelected) {
          // Deselect all from this host
          this.selectedURLCacheItems = this.selectedURLCacheItems.filter(s => s.hostname !== host);
        } else {
          // Select all from this host
          for (const entry of hostEntries) {
            const exists = this.selectedURLCacheItems.some(s => s.hostname === entry.Hostname && s.path === entry.Path);
            if (!exists) {
              this.selectedURLCacheItems.push({ hostname: entry.Hostname, path: entry.Path });
            }
          }
        }
        this.renderURLCacheTable();
      },
      async deleteURLCacheEntry(hostname, path) {
        try {
          const res = await fetch('/api/v1/cache/url/delete', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ entries: [{ hostname, path }] })
          });
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          // Remove from local cache
          this.urlCacheEntries = this.urlCacheEntries.filter(e => !(e.Hostname === hostname && e.Path === path));
          this.urlCacheTotal = this.urlCacheEntries.length;
          this.renderURLCacheTable();
          await this.loadStats();
        } catch (e) {
          console.error('Failed to delete URL cache entry:', e);
          alert('Failed to delete: ' + e.message);
        }
      },
      renderPackageList() {
        const container = document.getElementById("package-list-container");
        if (!container) return;

        const tree = this.packageTree;
        if (!tree || Object.keys(tree).length === 0) {
          container.innerHTML = '<p class="text-gray-500 text-sm">No packages found</p>';
          return;
        }

        const typeOrder = ['apk', 'apt', 'npm', 'pypi', 'go'];
        const maxInitialDepth = 1; // Only render type headers + first level initially

        // Use shared rendering methods
        const esc = RoxxyUtils.escapeHtml;
        const renderPackage = (name, node, path, depth, type) => this.renderPackageHTML(name, node, path, depth, type);
        const getLevelColor = (d) => this.getLevelColor(d);

        // Render a group (directory node) - lazy: only render children if depth < maxInitialDepth
        const renderGroup = (name, node, path, depth, type) => {
          const hasGroups = node.groups && Object.keys(node.groups).length > 0;
          const hasPackages = node.packages && Object.keys(node.packages).length > 0;
          const hasVersions = node.versions && node.versions.length > 0;

          // If this is actually a package (has versions), render as package
          if (hasVersions && !hasGroups && !hasPackages) {
            return renderPackage(name, node, path, depth, type);
          }

          const color = getLevelColor(depth);
          const indent = depth * 1.5;
          const id = `grp-${type}-${path.replace(/[^a-z0-9]/gi, '-')}`;
          const count = node.count || 0;
          const shouldRenderChildren = depth < maxInitialDepth;

          let html = `<div class="${color}" style="padding-left:${indent}rem">`;
          if (shouldRenderChildren) {
            html += `<button onclick="this.nextElementSibling.classList.toggle('hidden'); this.querySelector('.chv').classList.toggle('rotate-90')" class="w-full flex items-center space-x-2 px-3 py-2 hover:brightness-95 text-left">`;
          } else {
            html += `<button onclick="window.alpineApp.expandGroup('${type}', '${path.replace(/'/g, "\\'")}', this, ${depth})" class="w-full flex items-center space-x-2 px-3 py-2 hover:brightness-95 text-left">`;
          }
          html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform"></i>`;
          html += `<i class="ti ti-folder icon text-gray-500"></i>`;
          html += `<span class="font-medium text-gray-800">${esc(name)}</span>`;
          html += `<span class="text-sm text-gray-500">(${count})</span>`;
          html += `</button>`;
          html += `<div id="${id}" class="hidden">`;

          if (shouldRenderChildren) {
            if (hasGroups) {
              for (const gn of Object.keys(node.groups).sort()) {
                html += renderGroup(gn, node.groups[gn], `${path}/${gn}`, depth + 1, type);
              }
            }
            if (hasPackages) {
              for (const pn of Object.keys(node.packages).sort()) {
                const pkg = node.packages[pn];
                const pkgPath = pkg.fullName || `${path}/${pn}`;
                const displayName = pn.includes('/') ? pn.split('/').pop() : pn;
                if (pkg.versions && pkg.versions.length > 0) {
                  html += renderPackage(displayName, pkg, pkgPath, depth + 1, type);
                } else {
                  html += renderGroup(displayName, pkg, pkgPath, depth + 1, type);
                }
              }
            }
          } else {
            const placeholderIndent = (depth + 1) * 1.5;
            html += `<div class="lazy-placeholder ${getLevelColor(depth + 1)} px-3 py-2 text-sm text-gray-400" style="padding-left:${placeholderIndent}rem"><i class="ti ti-loader icon animate-spin mr-1"></i>Loading...</div>`;
          }

          html += `</div></div>`;
          return html;
        };

        let html = '';
        for (const type of typeOrder) {
          const typeData = tree[type];
          if (!typeData || typeData.count === 0) continue;

          const typeId = `type-${type}`;
          const iconClass = RoxxyUtils.getTypeIcon(type);

          html += `<div class="mb-2" data-type="${type}">`;
          html += `<button onclick="this.nextElementSibling.classList.toggle('hidden'); this.querySelector('.chv').classList.toggle('rotate-90')" class="w-full flex items-center justify-between p-3 rounded-lg hover:bg-blue-50 border border-transparent hover:border-blue-200 transition-all text-left">`;
          html += `<div class="flex items-center space-x-2">`;
          html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform"></i>`;
          html += `<i class="${iconClass} icon"></i>`;
          html += `<span class="text-gray-700 font-medium">${type.toUpperCase()}</span>`;
          html += `</div>`;
          html += `<span class="text-gray-900 font-semibold text-lg">${typeData.count || 0}</span>`;
          html += `</button>`;
          html += `<div id="${typeId}" class="hidden">`;

          if (typeData.groups) {
            for (const gn of Object.keys(typeData.groups).sort()) {
              if (gn === 'default') continue;
              html += renderGroup(gn, typeData.groups[gn], gn, 1, type);
            }
          }
          if (typeData.packages) {
            for (const pn of Object.keys(typeData.packages).sort()) {
              const pkg = typeData.packages[pn];
              html += renderPackage(pn, pkg, pn, 1, type);
            }
          }
          html += `</div></div>`;
        }

        container.innerHTML = html || '<p class="text-gray-500 text-sm">No packages found</p>';

        // Store reference for lazy loading
        window.alpineApp = this;
      },
      expandGroup(type, path, btn, depth) {
        const node = this.getNodeByPath(type, path);
        if (!node) {
          console.error('[expandGroup] Node not found for path:', path);
          return;
        }

        const container = btn.nextElementSibling;
        if (!container) return;

        const placeholder = container.querySelector('.lazy-placeholder');
        if (!placeholder) {
          // Already loaded, just toggle
          container.classList.toggle('hidden');
          btn.querySelector('.chv')?.classList.toggle('rotate-90');
          return;
        }

        // Render children using shared methods - children are at depth + 1
        const childDepth = depth + 1;
        let html = '';

        const renderChildren = (n, p, d) => {
          const hasGroups = n.groups && Object.keys(n.groups).length > 0;
          const hasPackages = n.packages && Object.keys(n.packages).length > 0;

          if (hasGroups) {
            for (const gn of Object.keys(n.groups).sort()) {
              const child = n.groups[gn];
              const childPath = `${p}/${gn}`;
              const displayName = gn.includes('/') ? gn.split('/').pop() : gn;

              if (child.versions?.length > 0 && !child.groups && !child.packages) {
                html += this.renderPackageHTML(displayName, child, childPath, d, type);
              } else {
                html += this.renderGroupHTML(displayName, child, childPath, d, type);
              }
            }
          }

          if (hasPackages) {
            for (const pn of Object.keys(n.packages).sort()) {
              const pkg = n.packages[pn];
              const pkgPath = pkg.fullName || `${p}/${pn}`;
              const displayName = pn.includes('/') ? pn.split('/').pop() : pn;
              html += this.renderPackageHTML(displayName, pkg, pkgPath, d, type);
            }
          }
        };

        renderChildren(node, path, childDepth);
        container.innerHTML = html || '<p class="text-xs text-gray-400 px-3">Empty</p>';
        container.classList.remove('hidden');
        btn.querySelector('.chv')?.classList.add('rotate-90');
      },
      // Shared constants
      levelColors: ['bg-blue-50', 'bg-indigo-50', 'bg-purple-50', 'bg-violet-50', 'bg-fuchsia-50', 'bg-pink-50'],
      getLevelColor(depth) {
        return this.levelColors[Math.min(depth, this.levelColors.length - 1)];
      },
      renderGroupHTML(name, node, path, depth, type) {
        const esc = RoxxyUtils.escapeHtml;
        const color = this.getLevelColor(depth);
        const indent = depth * 1.5;
        const id = `grp-${type}-${path.replace(/[^a-z0-9]/gi, '-')}`;
        const count = node.count || 0;
        const placeholderIndent = (depth + 1) * 1.5;

        let html = `<div class="${color}" style="padding-left:${indent}rem">`;
        html += `<button onclick="window.alpineApp.expandGroup('${type}', '${path.replace(/'/g, "\\'")}', this, ${depth})" class="w-full flex items-center space-x-2 px-3 py-2 hover:brightness-95 text-left">`;
        html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform"></i>`;
        html += `<i class="ti ti-folder icon text-gray-500"></i>`;
        html += `<span class="font-medium text-gray-800">${esc(name)}</span>`;
        html += `<span class="text-sm text-gray-500">(${count})</span>`;
        html += `</button>`;
        html += `<div id="${id}" class="hidden">`;
        html += `<div class="lazy-placeholder ${this.getLevelColor(depth + 1)} px-3 py-2 text-sm text-gray-400" style="padding-left:${placeholderIndent}rem"><i class="ti ti-loader icon animate-spin mr-1"></i>Loading...</div>`;
        html += `</div></div>`;
        return html;
      },
      renderPackageHTML(name, node, path, depth, type) {
        const esc = RoxxyUtils.escapeHtml;
        const fmt = RoxxyUtils.formatBytes;
        const fmtDate = RoxxyUtils.formatDate;
        const count = (node.versions || []).length || node.count || 0;
        const id = `pkg-${type}-${path.replace(/[^a-z0-9]/gi, '-')}`;
        const color = this.getLevelColor(depth);
        const indent = depth * 1.5;

        let html = `<div class="${color}" style="padding-left:${indent}rem">`;
        html += `<button onclick="this.nextElementSibling.classList.toggle('hidden'); this.querySelector('.chv').classList.toggle('rotate-90')" class="w-full flex items-center space-x-2 px-3 py-1.5 hover:brightness-95 text-left">`;
        html += `<i class="ti ti-chevron-right chv icon text-gray-400 transition-transform text-xs"></i>`;
        html += `<i class="ti ti-package icon text-gray-400 text-sm"></i>`;
        html += `<span class="text-sm font-medium text-gray-700">${esc(name)}</span>`;
        html += `<span class="text-xs text-gray-500">(${count} version${count !== 1 ? 's' : ''})</span>`;
        html += `</button>`;
        html += `<div id="${id}" class="hidden space-y-1 pb-1" style="padding-left:${indent + 1.5}rem">`;
        // Determine common metadata across versions
        const versions = node.versions || [];
        const desc = node.description || (versions[0]?.description);
        const license = node.license || (versions[0]?.license);
        const archs = [...new Set(versions.map(v => v.architecture).filter(Boolean))];
        const commonArch = archs.length === 1 ? archs[0] : null;

        // Package-level metadata: Description first, then License (Arch)
        if (desc || license || commonArch) {
          html += `<div class="px-3 py-1 text-xs bg-blue-50 border border-blue-100 rounded mb-1 text-gray-600">`;
          if (desc) html += `<div>${esc(desc.substring(0, 200))}${desc.length > 200 ? '...' : ''}</div>`;
          if (license || commonArch) {
            html += `<div>`;
            if (license) html += `License: ${esc(license)}`;
            if (commonArch) html += `${license ? ' ' : ''}(${esc(commonArch)})`;
            html += `</div>`;
          }
          html += `</div>`;
        }
        for (const ver of versions) {
          const diffLicense = ver.license && ver.license !== license;
          const diffArch = ver.architecture && ver.architecture !== commonArch;
          const diffDesc = ver.description && ver.description !== desc;
          const verId = ver.id || path;

          html += `<div class="flex flex-wrap items-center gap-2 px-3 py-2 text-sm bg-white border border-gray-100 rounded hover:bg-gray-50 group">`;
          // Selection checkbox (only in selection mode)
          const isSelected = this.selectedPackages.includes(verId);
          if (this.selectionMode) {
            html += `<input type="checkbox" class="rounded border-gray-300 text-blue-600 cursor-pointer" ${isSelected ? 'checked' : ''} onclick="event.stopPropagation(); window.alpineApp.togglePackageSelection('${esc(verId.replace(/'/g, "\\'"))}')">`;
          }
          html += `<span class="font-medium text-gray-900">${esc(ver.version || 'N/A')}</span>`;
          html += `<span class="text-xs text-gray-500">${fmt(ver.size || 0)}</span>`;
          if (ver.cached) {
            html += `<span class="inline-flex items-center px-1.5 py-0.5 rounded text-xs bg-green-100 text-green-800"><i class="ti ti-check icon mr-0.5"></i>Cached</span>`;
          } else {
            html += `<span class="inline-flex items-center px-1.5 py-0.5 rounded text-xs bg-gray-100 text-gray-600"><i class="ti ti-cloud icon mr-0.5"></i>Available</span>`;
          }
          if (ver.access_count > 0) html += `<span class="text-xs text-blue-600">${ver.access_count}x</span>`;
          if (ver.cached_at) html += `<span class="text-xs text-gray-400">${fmtDate(ver.cached_at)}</span>`;
          if (diffLicense || diffArch) {
            let meta = '';
            if (diffLicense) meta += `License: ${esc(ver.license)}`;
            if (diffArch) meta += (meta ? ' ' : '') + `(${esc(ver.architecture)})`;
            html += `<span class="text-xs text-gray-500">${meta}</span>`;
          }
          // Delete button (only for cached items)
          if (ver.cached && verId) {
            html += `<button onclick="event.stopPropagation(); window.alpineApp.deletePackage('${esc(verId.replace(/'/g, "\\'"))}')" class="ml-auto opacity-0 group-hover:opacity-100 transition-opacity p-1 text-red-400 hover:text-red-600 hover:bg-red-50 rounded" title="Delete"><i class="ti ti-trash icon text-sm"></i></button>`;
          }
          html += `</div>`;
          if (diffDesc && type !== 'apt') {
            html += `<div class="px-3 pb-1 text-xs text-gray-500 -mt-1 ml-2 border-l-2 border-gray-200">`;
            html += `<span>${esc(ver.description.substring(0, 150))}${ver.description.length > 150 ? '...' : ''}</span>`;
            html += `</div>`;
          }
        }
        html += `</div></div>`;
        return html;
      },
      getNodeByPath(type, path) {
        // Handle collapsed group chains (e.g., "bookworm-updates/main" stored as single key)
        let node = this.packageTree[type];
        if (!node) return null;

        // First try direct lookup with the full remaining path at each level
        const parts = path.split('/');
        let i = 0;
        while (i < parts.length) {
          // Try to match remaining path segments as a collapsed key
          let found = false;
          for (let j = parts.length; j > i; j--) {
            const segment = parts.slice(i, j).join('/');
            if (node.groups && node.groups[segment]) {
              node = node.groups[segment];
              i = j;
              found = true;
              break;
            }
            if (node.packages && node.packages[segment]) {
              node = node.packages[segment];
              i = j;
              found = true;
              break;
            }
          }
          if (!found) return null;
        }
        return node;
      },
      filterByType(type) {
        ((this.activeTab = "packages"),
          this.$nextTick(() => {
            const element = document.querySelector(
              `[data-type="${type.toLowerCase()}"]`,
            );
            element && element.scrollIntoView({ behavior: "smooth", block: "start" });
          }));
      },
      packageMatchesSearch(name, version) {
        if (!this.packageSearch || this.packageSearch.trim() === "") return true;
        const search = this.packageSearch.toLowerCase().trim(),
          nameLower = (name || "").toLowerCase(),
          versionLower = (version || "").toLowerCase();
        return nameLower.includes(search) || versionLower.includes(search);
      },
      groupHasMatchingPackages(group) {
        if (!this.packageSearch || this.packageSearch.trim() === "") return true;
        if (!group || !group.packages) return false;
        for (const [pkgName, pkgData] of Object.entries(group.packages))
          if (pkgData && pkgData.versions) {
            for (const ver of pkgData.versions)
              if (this.packageMatchesSearch(pkgName, ver.version)) return true;
          }
        return false;
      },
      // Utility function wrappers (delegate to RoxxyUtils)
      formatBytes(bytes) {
        return RoxxyUtils.formatBytes(bytes);
      },
      formatDate(date) {
        return RoxxyUtils.formatDate(date);
      },
      getTypeIcon(type) {
        return RoxxyUtils.getTypeIcon(type);
      },
      formatGoPackageName(name, type) {
        return RoxxyUtils.formatGoPackageName(name, type);
      },
      getGroupColors(depth) {
        return RoxxyUtils.getGroupColors(depth);
      },
      // Delete a single package
      async deletePackage(id) {
        try {
          const res = await fetch(`/api/v1/packages/${encodeURIComponent(id)}`, { method: 'DELETE' });
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          // Refresh the package list and stats
          await Promise.all([this.loadPackages(), this.loadStats()]);
        } catch (e) {
          console.error('Failed to delete package:', e);
          alert('Failed to delete package: ' + e.message);
        }
      },
      // Delete a single Docker tag
      async deleteDockerTag(repo, tag) {
        try {
          const res = await fetch(`/api/v1/docker/repositories/${encodeURIComponent(repo)}/tags/${encodeURIComponent(tag)}`, { method: 'DELETE' });
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          // Remove from cached tags immediately for instant UI feedback
          if (this.dockerRepoTags[repo]) {
            this.dockerRepoTags[repo] = this.dockerRepoTags[repo].filter(t => (typeof t === 'string' ? t : t.tag) !== tag);
          }
          // Refresh repos list (updates tag counts) and stats
          await Promise.all([this.loadDockerRepos(), this.loadStats()]);
        } catch (e) {
          console.error('Failed to delete Docker tag:', e);
          alert('Failed to delete Docker tag: ' + e.message);
        }
      },
      // Toggle Docker host expansion
      toggleDockerHost(host) {
        this.expandedDockerHosts[host] = !this.expandedDockerHosts[host];
        this.renderDockerTableDirectly();
      },
      // Toggle Docker namespace expansion
      toggleDockerNamespace(nsKey) {
        this.expandedDockerNamespaces[nsKey] = !this.expandedDockerNamespaces[nsKey];
        this.renderDockerTableDirectly();
      },
      // Toggle Docker repo expansion
      async toggleDockerRepo(repoName) {
        if (this.expandedDockerRepos[repoName]) {
          this.expandedDockerRepos[repoName] = false;
          this.renderDockerTableDirectly();
          return;
        }
        this.expandedDockerRepos[repoName] = true;
        if (!this.dockerRepoTags[repoName]) {
          await this.loadDockerRepoTags(repoName);
        } else {
          this.renderDockerTableDirectly();
        }
      },
      // Load detailed tags for a Docker repo
      async loadDockerRepoTags(repoName) {
        this.loadingDockerTags[repoName] = true;
        this.renderDockerTableDirectly(); // Show loading state
        try {
          const res = await fetch(`/api/v1/docker/repositories/${encodeURIComponent(repoName)}?detailed=true`);
          if (!res.ok) throw new Error(`HTTP ${res.status}`);
          const data = await res.json();
          this.dockerRepoTags[repoName] = data.tags || [];
        } catch (e) {
          console.error('Failed to load Docker tags:', e);
          this.dockerRepoTags[repoName] = [];
        } finally {
          this.loadingDockerTags[repoName] = false;
          this.renderDockerTableDirectly(); // Show loaded tags
        }
      },
      // Sorting change handlers
      async changePackageSort(sortBy) {
        this.packageSortBy = sortBy;
        await this.loadPackages();
      },
      togglePackageSortOrder() {
        this.packageSortOrder = this.packageSortOrder === 'asc' ? 'desc' : 'asc';
        this.loadPackages();
      },
      async changeDockerSort(sortBy) {
        this.dockerSortBy = sortBy;
        this.renderDockerTableDirectly();
      },
      toggleDockerSortOrder() {
        this.dockerSortOrder = this.dockerSortOrder === 'asc' ? 'desc' : 'asc';
        this.renderDockerTableDirectly();
      },
      // Selection mode toggle
      toggleSelectionMode() {
        this.selectionMode = !this.selectionMode;
        if (!this.selectionMode) {
          this.clearSelection();
        }
        // Re-render to show/hide checkboxes
        this.renderPackageList();
        this.renderDockerTableDirectly();
        this.renderURLCacheTable();
      },
      // Toggle package selection
      togglePackageSelection(id) {
        const idx = this.selectedPackages.indexOf(id);
        if (idx === -1) {
          this.selectedPackages.push(id);
        } else {
          this.selectedPackages.splice(idx, 1);
        }
        // Update checkbox state directly without full re-render
        const checkbox = document.querySelector(`input[onclick*="${id.replace(/'/g, "\\'")}"]`);
        if (checkbox) checkbox.checked = idx === -1;
      },
      // Toggle Docker item selection
      toggleDockerSelection(repo, tag) {
        const key = `${repo}:${tag}`;
        const idx = this.selectedDockerItems.findIndex(i => `${i.repo}:${i.tag}` === key);
        if (idx === -1) {
          this.selectedDockerItems.push({ repo, tag });
        } else {
          this.selectedDockerItems.splice(idx, 1);
        }
        // Checkbox state is already toggled by the click
      },
      // Select/deselect all tags for a Docker repo
      selectAllDockerRepoTags(repoName) {
        const tags = this.dockerRepoTags[repoName] || [];
        if (tags.length === 0) return;

        // Check if all tags are already selected
        const allSelected = tags.every(t => {
          const tName = typeof t === 'string' ? t : t.tag;
          return this.selectedDockerItems.some(i => i.repo === repoName && i.tag === tName);
        });

        if (allSelected) {
          // Deselect all tags for this repo
          this.selectedDockerItems = this.selectedDockerItems.filter(i => i.repo !== repoName);
        } else {
          // Select all tags for this repo (add any not already selected)
          for (const t of tags) {
            const tName = typeof t === 'string' ? t : t.tag;
            const exists = this.selectedDockerItems.some(i => i.repo === repoName && i.tag === tName);
            if (!exists) {
              this.selectedDockerItems.push({ repo: repoName, tag: tName });
            }
          }
        }
        this.renderDockerTableDirectly();
      },
      // Check if a package is selected
      isPackageSelected(id) {
        return this.selectedPackages.includes(id);
      },
      // Check if a Docker item is selected
      isDockerSelected(repo, tag) {
        return this.selectedDockerItems.some(i => i.repo === repo && i.tag === tag);
      },
      // Select all visible packages
      selectAllPackages() {
        // Get all package IDs from the tree
        const collectIds = (node) => {
          const ids = [];
          if (node.versions) {
            for (const ver of node.versions) {
              if (ver.id) ids.push(ver.id);
            }
          }
          if (node.packages) {
            for (const pkg of Object.values(node.packages)) {
              ids.push(...collectIds(pkg));
            }
          }
          if (node.groups) {
            for (const grp of Object.values(node.groups)) {
              ids.push(...collectIds(grp));
            }
          }
          return ids;
        };
        const allIds = [];
        for (const type of ['apk', 'apt', 'npm', 'pypi', 'go']) {
          if (this.packageTree[type]) {
            allIds.push(...collectIds(this.packageTree[type]));
          }
        }
        this.selectedPackages = allIds;
      },
      // Select all Docker items
      selectAllDocker() {
        this.selectedDockerItems = [];
        for (const repo of this.dockerRepos) {
          const tags = this.dockerRepoTags[repo.name] || [];
          for (const tag of tags) {
            this.selectedDockerItems.push({ repo: repo.name, tag: tag.tag || tag });
          }
        }
      },
      // Clear all selections
      clearSelection() {
        this.selectedPackages = [];
        this.selectedDockerItems = [];
        this.selectedURLCacheItems = [];
      },
      // Open delete confirmation modal
      openDeleteModal() {
        if (this.activeTab === 'packages' && this.selectedPackages.length > 0) {
          this.deleteModalType = 'packages';
          this.deleteModalItems = [...this.selectedPackages];
          this.showDeleteModal = true;
        } else if (this.activeTab === 'docker' && this.selectedDockerItems.length > 0) {
          this.deleteModalType = 'docker';
          this.deleteModalItems = [...this.selectedDockerItems];
          this.showDeleteModal = true;
        } else if (this.activeTab === 'cache' && this.selectedURLCacheItems.length > 0) {
          this.deleteModalType = 'urlcache';
          this.deleteModalItems = [...this.selectedURLCacheItems];
          this.showDeleteModal = true;
        }
      },
      // Close delete modal
      closeDeleteModal() {
        this.showDeleteModal = false;
        this.deleteModalItems = [];
        this.deleteModalType = '';
      },
      // Confirm and execute bulk delete
      async confirmBulkDelete() {
        const deleteType = this.deleteModalType; // Save before closing modal
        try {
          if (deleteType === 'packages') {
            const res = await fetch('/api/v1/packages/bulk-delete', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ ids: this.deleteModalItems })
            });
            if (!res.ok) throw new Error(`HTTP ${res.status}`);
            const result = await res.json();
            console.log('Bulk delete result:', result);
          } else if (deleteType === 'docker') {
            const res = await fetch('/api/v1/docker/bulk-delete', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ items: this.deleteModalItems })
            });
            if (!res.ok) throw new Error(`HTTP ${res.status}`);
            const result = await res.json();
            console.log('Bulk delete result:', result);
          } else if (deleteType === 'urlcache') {
            const res = await fetch('/api/v1/cache/url/delete', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ entries: this.deleteModalItems })
            });
            if (!res.ok) throw new Error(`HTTP ${res.status}`);
            const result = await res.json();
            console.log('Bulk delete result:', result);
          }
          // Clear selection and close modal
          this.clearSelection();
          this.closeDeleteModal();
          // Clear cached data so it reloads fresh
          if (deleteType === 'docker') {
            this.dockerRepoTags = {};
            this.expandedDockerRepos = {};
          } else if (deleteType === 'packages') {
            // Clear package tree to force full reload
            this.packageTree = {};
            this.loadedGroups = {};
          } else if (deleteType === 'urlcache') {
            // Reload URL cache
            await this.loadURLCache();
          }
          await Promise.all([this.loadStats(), this.loadPackages(), this.loadDockerRepos()]);
        } catch (e) {
          console.error('Bulk delete failed:', e);
          alert('Bulk delete failed: ' + e.message);
        }
      },
    }));
  });
})();
