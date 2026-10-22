/**
 * Package List Component Generator
 * Generates DOM elements functionally to avoid massive nested HTML templates
 */

// Color scheme for nested groups
const GROUP_COLORS = {
    bg: ['bg-purple-100', 'bg-blue-200', 'bg-cyan-200', 'bg-teal-200', 'bg-green-200'],
    hover: ['hover:bg-purple-200', 'hover:bg-blue-300', 'hover:bg-cyan-300', 'hover:bg-teal-300', 'hover:bg-green-300'],
    text: ['text-purple-700', 'text-indigo-700', 'text-cyan-800', 'text-teal-800', 'text-green-800'],
    icon: ['text-purple-600', 'text-blue-700', 'text-cyan-700', 'text-teal-700', 'text-green-700']
};

function getGroupColors(depth) {
    const idx = Math.min(depth, GROUP_COLORS.bg.length - 1);
    return {
        bg: GROUP_COLORS.bg[idx] || 'bg-gray-100',
        hover: GROUP_COLORS.hover[idx] || 'hover:bg-gray-200',
        text: GROUP_COLORS.text[idx] || 'text-gray-700',
        icon: GROUP_COLORS.icon[idx] || 'text-gray-600'
    };
}

/**
 * Generate type header button
 */
function createTypeHeader(type) {
    const button = document.createElement('button');
    button.setAttribute('@click.prevent', 'toggleType(type)');
    button.setAttribute('type', 'button');
    button.className = 'w-full flex items-center justify-between px-4 py-3 bg-gray-50 hover:bg-gray-100 rounded-lg transition-colors text-left';
    
    button.innerHTML = `
        <div class="flex items-center space-x-3">
            <i :class="getTypeIcon(type)" class="icon text-gray-600"></i>
            <span class="font-semibold text-gray-900 capitalize" x-text="type"></span>
            <span class="text-sm text-gray-500" x-text="\`(\${(packageTree[type] && packageTree[type].count) || 0} packages)\`"></span>
        </div>
        <i :class="expandedTypes[type] === true ? 'ti ti-chevron-down' : 'ti ti-chevron-right'" class="icon text-gray-400"></i>
    `;
    
    return button;
}

/**
 * Generate group button
 */
function createGroupButton(type, groupPath, groupKey, depth, count) {
    const colors = getGroupColors(depth);
    const button = document.createElement('button');
    button.setAttribute('@click.prevent', `toggleGroup(type, groupPath)`);
    button.setAttribute('type', 'button');
    button.className = `w-full flex items-center justify-between px-3 py-2 ${colors.bg} ${colors.hover} rounded transition-colors text-left mb-1`;
    
    button.innerHTML = `
        <div class="flex items-center space-x-2">
            <i :class="expandedGroups[\`\${type}:\${groupPath}\`] === true ? 'ti ti-chevron-down' : 'ti ti-chevron-right'" 
               :class="\`icon \${getGroupColors(${depth}).iconColor} text-xs\`"></i>
            <span :class="\`text-xs font-medium \${getGroupColors(${depth}).textColor}\`" 
                  x-text="type === 'go' ? formatGoPackageName('${groupKey}', type) : '${groupKey}'"></span>
            <span :class="\`text-xs \${getGroupColors(${depth}).iconColor}\`" 
                  x-text="\`(${count} packages)\`"></span>
        </div>
    `;
    
    return button;
}

/**
 * Generate package button
 */
function createPackageButton(type, pkgName, pkgPath, versionCount, hasSubPackages = false) {
    const button = document.createElement('button');
    button.setAttribute('@click.prevent', `togglePackage(type, '${pkgPath}')`);
    button.setAttribute('type', 'button');
    button.className = 'w-full flex items-center justify-between px-4 py-2 bg-gray-25 hover:bg-gray-50 rounded transition-colors text-left';
    
    const countText = hasSubPackages 
        ? `\`(\${Object.keys(pkgGroup.packages || {}).length} package\${Object.keys(pkgGroup.packages || {}).length !== 1 ? 's' : ''})\``
        : `\`(${versionCount} version${versionCount !== 1 ? 's' : ''})\``;
    
    button.innerHTML = `
        <div class="flex items-center space-x-2 flex-1">
            <span class="text-sm font-medium text-gray-700" x-text="formatGoPackageName('${pkgName}', type) || 'N/A'"></span>
            <span class="text-xs text-gray-500" x-text="${countText}"></span>
        </div>
        <i :class="expandedPackages[\`\${type}:${pkgPath}\`] === true ? 'ti ti-chevron-down' : 'ti ti-chevron-right'" 
           class="icon text-gray-400 text-xs"></i>
    `;
    
    return button;
}

/**
 * Generate package metadata display
 */
function createPackageMetadata(pkgGroup) {
    const div = document.createElement('div');
    div.setAttribute('x-show', `pkgGroup.description || pkgGroup.license || (pkgGroup.dependencies && Array.isArray(pkgGroup.dependencies) && pkgGroup.dependencies.length > 0)`);
    div.className = 'px-4 py-2 mb-2 bg-blue-50 border border-blue-200 rounded text-xs space-y-1';
    
    div.innerHTML = `
        <div x-show="pkgGroup.description" class="text-gray-700">
            <span class="font-medium text-gray-600">Description: </span>
            <span x-text="pkgGroup.description"></span>
        </div>
        <div x-show="pkgGroup.license" class="text-gray-700">
            <span class="font-medium text-gray-600">License: </span>
            <span x-text="pkgGroup.license"></span>
        </div>
        <div x-show="pkgGroup.dependencies && Array.isArray(pkgGroup.dependencies) && pkgGroup.dependencies.length > 0" class="text-gray-700">
            <span class="font-medium text-gray-600">Dependencies: </span>
            <span x-text="(pkgGroup.dependencies && Array.isArray(pkgGroup.dependencies)) ? pkgGroup.dependencies.join(', ') : ''"></span>
        </div>
    `;
    
    return div;
}

/**
 * Generate version item
 */
function createVersionItem(type, pkgName, pkg) {
    const div = document.createElement('div');
    div.setAttribute('x-show', `packageMatchesSearch('${pkgName}', pkg.version)`);
    div.className = 'px-4 py-2 bg-white border border-gray-100 rounded hover:bg-gray-50 transition-colors';
    
    const isClickable = `(type === 'npm' || type === 'pypi' || type === 'go' || type === 'apk') && pkg.name && pkg.version && !pkg.loadingMetadata`;
    div.setAttribute(':class', `{'cursor-pointer': ${isClickable}}`);
    
    const clickHandler = `(type === 'npm' && pkg.cached && !pkg.loadingMetadata && loadNPMMetadata(pkg, $event)) || 
                         (type === 'pypi' && pkg.name && pkg.version && !pkg.loadingMetadata && loadPyPIMetadata(pkg, $event)) || 
                         (type === 'go' && pkg.name && pkg.version && !pkg.loadingMetadata && loadGoMetadata(pkg, $event)) || 
                         (type === 'apk' && pkg.name && pkg.version && !pkg.loadingMetadata && loadAPKMetadata(pkg, $event))`;
    div.setAttribute('@click', clickHandler);
    
    div.innerHTML = `
        <div class="flex items-center justify-between">
            <div class="flex items-center space-x-4 flex-1 flex-wrap">
                <span class="text-sm font-medium text-gray-900" x-text="pkg.version || 'N/A'"></span>
                <span class="text-xs text-gray-500" x-text="formatBytes(pkg.size)"></span>
                <span x-show="pkg.cached" class="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-green-100 text-green-800">
                    <i class="ti ti-check icon mr-1"></i>
                    <span>Cached</span>
                </span>
                <span x-show="!pkg.cached" class="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-gray-100 text-gray-600">
                    <i class="ti ti-cloud icon mr-1"></i>
                    <span>Available</span>
                </span>
                <span x-show="pkg.access_count > 0" class="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-blue-100 text-blue-800" x-text="\`\${pkg.access_count}x\`"></span>
                <span x-show="pkg.cached_at" class="text-xs text-gray-400" x-text="formatDate(pkg.cached_at)"></span>
                <span x-show="(type === 'npm' || type === 'pypi' || type === 'go' || type === 'apk') && pkg.loadingMetadata" class="inline-flex items-center px-2 py-0.5 rounded-full text-xs font-medium bg-yellow-100 text-yellow-800">
                    <i class="ti ti-loader icon mr-1 animate-spin"></i>
                    <span>Loading...</span>
                </span>
                <span x-show="(type === 'npm' && pkg.cached && !pkg.loadingMetadata && !pkg.metadata) || ((type === 'pypi' || type === 'go' || type === 'apk') && pkg.name && pkg.version && !pkg.loadingMetadata && !pkg.metadata)" class="text-xs text-gray-400 italic">(click to load metadata)</span>
            </div>
        </div>
        ${createVersionMetadataHTML(type)}
    `;
    
    return div;
}

/**
 * Generate version metadata HTML
 */
function createVersionMetadataHTML(type) {
    return `
        <!-- Show version-specific license/description if available (from index) -->
        <div x-show="pkg.license || (type !== 'apt' && pkg.description)" class="mt-1 pt-1 border-t border-gray-100 space-y-1">
            <div x-show="pkg.license" class="text-xs">
                <span class="text-gray-500">License: </span>
                <span class="text-gray-700" x-text="pkg.license"></span>
            </div>
            <div x-show="type !== 'apt' && pkg.description" class="text-xs text-gray-600" x-text="pkg.description"></div>
        </div>
        <div x-show="pkg.architecture" class="mt-1">
            <span class="text-xs text-gray-500">Architecture: </span>
            <span class="text-xs text-gray-700 font-mono" x-text="pkg.architecture"></span>
        </div>
        <!-- Metadata display -->
        <div x-show="(type === 'npm' || type === 'pypi' || type === 'go' || type === 'apk') && pkg.metadata" class="mt-2 pt-2 border-t border-gray-200 space-y-1">
            <div x-show="pkg.metadata.description" class="text-xs text-gray-600" x-text="pkg.metadata.description"></div>
            ${type === 'go' ? `
                <div x-show="pkg.metadata.goVersion" class="text-xs">
                    <span class="text-gray-500">Go Version: </span>
                    <span class="text-gray-700" x-text="pkg.metadata.goVersion"></span>
                </div>
                <div x-show="pkg.metadata.published" class="text-xs">
                    <span class="text-gray-500">Published: </span>
                    <span class="text-gray-700" x-text="pkg.metadata.published"></span>
                </div>
                <div x-show="pkg.metadata && pkg.metadata.directories && Array.isArray(pkg.metadata.directories) && pkg.metadata.directories.length > 0" class="text-xs">
                    <span class="text-gray-500">Directories: </span>
                    <span class="text-gray-700" x-text="(pkg.metadata && pkg.metadata.directories && Array.isArray(pkg.metadata.directories)) ? pkg.metadata.directories.join(', ') : ''"></span>
                </div>
            ` : ''}
        </div>
    `;
}

/**
 * Generate full metadata display for version (used in flat groups)
 */
function createFullVersionMetadata(type) {
    return `
        <!-- Show architecture if available -->
        <div x-show="pkg.architecture" class="mt-1">
            <span class="text-xs text-gray-500">Architecture<span x-show="type === 'apk' && pkg.architecture.includes(',')">s</span>: </span>
            <span class="text-xs text-gray-700 font-mono" x-text="pkg.architecture"></span>
        </div>
        <!-- Show license if available -->
        <div x-show="pkg.license || (pkg.metadata && pkg.metadata.license) || (type === 'apt' ? false : pkgGroup.license)" class="mt-1">
            <span class="text-xs text-gray-500">License: </span>
            <span class="text-xs text-gray-700" x-text="pkg.license || (pkg.metadata && pkg.metadata.license ? (typeof pkg.metadata.license === 'string' ? pkg.metadata.license : (pkg.metadata.license && pkg.metadata.license.type ? pkg.metadata.license.type : '')) : '') || (type === 'apt' ? '' : (pkgGroup.license || ''))"></span>
        </div>
        <!-- Show description if available -->
        <div x-show="type !== 'apt' && (pkg.description || (pkg.metadata && pkg.metadata.description))" class="mt-1">
            <span class="text-xs text-gray-600" x-text="pkg.description || (pkg.metadata && pkg.metadata.description) || ''"></span>
        </div>
        <!-- Show dependencies if available -->
        <div x-show="(pkg.dependencies && Array.isArray(pkg.dependencies) && pkg.dependencies.length > 0) || (pkg.metadata && pkg.metadata.dependencies && ((Array.isArray(pkg.metadata.dependencies) && pkg.metadata.dependencies.length > 0) || (typeof pkg.metadata.dependencies === 'object' && !Array.isArray(pkg.metadata.dependencies) && Object.keys(pkg.metadata.dependencies).length > 0)))" class="mt-1">
            <span class="text-xs text-gray-500">Depends: </span>
            <span class="text-xs text-gray-600" x-text="(pkg.dependencies && Array.isArray(pkg.dependencies)) ? pkg.dependencies.join(', ') : (pkg.metadata && pkg.metadata.dependencies ? (Array.isArray(pkg.metadata.dependencies) ? pkg.metadata.dependencies.join(', ') : (typeof pkg.metadata.dependencies === 'object' && !Array.isArray(pkg.metadata.dependencies) ? Object.keys(pkg.metadata.dependencies).join(', ') : '')) : '')"></span>
        </div>
        <!-- Show additional metadata from package.json for NPM/PyPI/Go/APK -->
        <div x-show="(type === 'npm' || type === 'pypi' || type === 'go' || type === 'apk') && pkg.metadata" class="mt-2 pt-2 border-t border-gray-200 space-y-1">
            ${createExtendedMetadataHTML(type)}
        </div>
        <!-- Show ID for N/A entries to help debug -->
        <div x-show="!pkg.name || !pkg.version || pkg.name === 'N/A' || pkg.version === 'N/A'" class="mt-1">
            <span class="text-xs text-gray-400 italic" x-text="\`ID: \${pkg.id ? (pkg.id.split('/').pop() || pkg.id) : (pkg.version || 'N/A')}\`"></span>
        </div>
    `;
}

/**
 * Generate extended metadata HTML (NPM/PyPI/Go/APK specific)
 */
function createExtendedMetadataHTML(type) {
    let html = '';
    
    if (type === 'npm' || type === 'pypi') {
        html += `
            <div x-show="pkg.metadata && pkg.metadata.author" class="text-xs">
                <span class="text-gray-500">Author: </span>
                <span class="text-gray-700" x-text="(pkg.metadata && pkg.metadata.author) ? (typeof pkg.metadata.author === 'string' ? pkg.metadata.author : (pkg.metadata.author && pkg.metadata.author.name ? pkg.metadata.author.name : '')) : ''"></span>
            </div>
            <div x-show="pkg.license || (pkg.metadata && pkg.metadata.license) || pkgGroup.license" class="text-xs">
                <span class="text-gray-500">License: </span>
                <span class="text-gray-700" x-text="pkg.license || (pkg.metadata && pkg.metadata.license ? (typeof pkg.metadata.license === 'string' ? pkg.metadata.license : (pkg.metadata.license && pkg.metadata.license.type ? pkg.metadata.license.type : '')) : '') || pkgGroup.license || ''"></span>
            </div>
            <div x-show="pkg.metadata.homepage" class="text-xs">
                <span class="text-gray-500">Homepage: </span>
                <a :href="pkg.metadata.homepage" target="_blank" class="text-blue-600 hover:underline" x-text="pkg.metadata.homepage"></a>
            </div>
            <div x-show="pkg.metadata.repository" class="text-xs">
                <span class="text-gray-500">Repository: </span>
                <a :href="typeof pkg.metadata.repository === 'string' ? pkg.metadata.repository : (pkg.metadata.repository && pkg.metadata.repository.url ? pkg.metadata.repository.url : '')" target="_blank" class="text-blue-600 hover:underline" x-text="typeof pkg.metadata.repository === 'string' ? pkg.metadata.repository : (pkg.metadata.repository && pkg.metadata.repository.url ? pkg.metadata.repository.url : '')"></a>
            </div>
            <div x-show="pkg.metadata.keywords && Array.isArray(pkg.metadata.keywords) && pkg.metadata.keywords.length > 0" class="text-xs">
                <span class="text-gray-500">Keywords: </span>
                <span class="text-gray-700" x-text="(pkg.metadata.keywords && Array.isArray(pkg.metadata.keywords)) ? pkg.metadata.keywords.join(', ') : ''"></span>
            </div>
        `;
    }
    
    if (type === 'go') {
        html += `
            <div x-show="pkg.metadata.description" class="text-xs mt-1">
                <span class="text-gray-600" x-text="pkg.metadata.description"></span>
            </div>
            <div x-show="pkg.metadata.goVersion" class="text-xs">
                <span class="text-gray-500">Go Version: </span>
                <span class="text-gray-700" x-text="pkg.metadata.goVersion"></span>
            </div>
            <div x-show="pkg.metadata.published" class="text-xs">
                <span class="text-gray-500">Published: </span>
                <span class="text-gray-700" x-text="pkg.metadata.published"></span>
            </div>
            <div x-show="pkg.metadata && pkg.metadata.directories && Array.isArray(pkg.metadata.directories) && pkg.metadata.directories.length > 0" class="text-xs">
                <span class="text-gray-500">Directories: </span>
                <span class="text-gray-700" x-text="(pkg.metadata && pkg.metadata.directories && Array.isArray(pkg.metadata.directories)) ? pkg.metadata.directories.join(', ') : ''"></span>
            </div>
        `;
    }
    
    if (type === 'apk') {
        html += `
            <div x-show="pkg.metadata.description" class="text-xs mt-1">
                <span class="text-gray-600" x-text="pkg.metadata.description"></span>
            </div>
            <div x-show="pkg.metadata.architecture" class="text-xs">
                <span class="text-gray-500">Architecture: </span>
                <span class="text-gray-700 font-mono" x-text="pkg.metadata.architecture"></span>
            </div>
            <div x-show="pkg.metadata.size" class="text-xs">
                <span class="text-gray-500">Size: </span>
                <span class="text-gray-700" x-text="pkg.metadata.size"></span>
            </div>
            <div x-show="pkg.license || (pkg.metadata && pkg.metadata.license) || pkgGroup.license" class="text-xs">
                <span class="text-gray-500">License: </span>
                <span class="text-gray-700" x-text="pkg.license || (pkg.metadata && pkg.metadata.license ? pkg.metadata.license : '') || pkgGroup.license || ''"></span>
            </div>
            <div x-show="pkg.metadata.homepage" class="text-xs">
                <span class="text-gray-500">Homepage: </span>
                <a :href="pkg.metadata.homepage" target="_blank" class="text-blue-600 hover:underline" x-text="pkg.metadata.homepage"></a>
            </div>
        `;
    }
    
    return html;
}

// Export functions for use in templates
if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
        getGroupColors,
        createTypeHeader,
        createGroupButton,
        createPackageButton,
        createPackageMetadata,
        createVersionItem,
        createVersionMetadataHTML,
        createFullVersionMetadata,
        createExtendedMetadataHTML
    };
}
