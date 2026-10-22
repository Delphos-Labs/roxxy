/**
 * Package List Component Helpers
 * These functions help generate reusable HTML templates for the package list
 */

// Group color scheme
const GROUP_COLORS = {
    bg: ['bg-purple-100', 'bg-blue-200', 'bg-cyan-200', 'bg-teal-200', 'bg-green-200'],
    hover: ['hover:bg-purple-200', 'hover:bg-blue-300', 'hover:bg-cyan-300', 'hover:bg-teal-300', 'hover:bg-green-300'],
    text: ['text-purple-700', 'text-indigo-700', 'text-cyan-800', 'text-teal-800', 'text-green-800'],
    icon: ['text-purple-600', 'text-blue-700', 'text-cyan-700', 'text-teal-700', 'text-green-700']
};

/**
 * Get group colors based on depth
 */
function getGroupColors(depth) {
    const idx = Math.min(depth, GROUP_COLORS.bg.length - 1);
    return {
        bgColor: GROUP_COLORS.bg[idx] || 'bg-gray-100',
        hoverColor: GROUP_COLORS.hover[idx] || 'hover:bg-gray-200',
        textColor: GROUP_COLORS.text[idx] || 'text-gray-700',
        iconColor: GROUP_COLORS.icon[idx] || 'text-gray-600'
    };
}

/**
 * Generate version display HTML
 * @param {string} type - Package type
 * @param {string} pkgName - Package name
 * @param {object} pkg - Package version object
 * @param {object} pkgGroup - Package group object
 * @param {boolean} fullMetadata - Whether to show full metadata
 */
function renderVersionHTML(type, pkgName, pkg, pkgGroup, fullMetadata = false) {
    const baseHTML = `
        <div class="flex items-center justify-between">
            <div class="flex items-center space-x-4 flex-1 flex-wrap">
                <span class="text-sm font-medium text-gray-900" x-text="pkg.version || 'N/A'"></span>
                ${fullMetadata ? `<span x-show="pkg.version === 'N/A'" class="text-xs text-gray-400 italic" x-text="\`ID: \${pkg.id ? (pkg.id.split('/').pop() || pkg.id) : (pkg.version || 'N/A')}\`"></span>` : ''}
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
    `;
    
    let metadataHTML = '';
    
    if (fullMetadata) {
        metadataHTML = `
            <div x-show="pkg.architecture" class="mt-1">
                <span class="text-xs text-gray-500">Architecture<span x-show="type === 'apk' && pkg.architecture.includes(',')">s</span>: </span>
                <span class="text-xs text-gray-700 font-mono" x-text="pkg.architecture"></span>
            </div>
            <div x-show="pkg.license || (pkg.metadata && pkg.metadata.license) || (type === 'apt' ? false : pkgGroup.license)" class="mt-1">
                <span class="text-xs text-gray-500">License: </span>
                <span class="text-xs text-gray-700" x-text="pkg.license || (pkg.metadata && pkg.metadata.license ? (typeof pkg.metadata.license === 'string' ? pkg.metadata.license : (pkg.metadata.license && pkg.metadata.license.type ? pkg.metadata.license.type : '')) : '') || (type === 'apt' ? '' : (pkgGroup.license || ''))"></span>
            </div>
            <div x-show="type !== 'apt' && (pkg.description || (pkg.metadata && pkg.metadata.description))" class="mt-1">
                <span class="text-xs text-gray-600" x-text="pkg.description || (pkg.metadata && pkg.metadata.description) || ''"></span>
            </div>
            <div x-show="(pkg.dependencies && Array.isArray(pkg.dependencies) && pkg.dependencies.length > 0) || (pkg.metadata && pkg.metadata.dependencies && ((Array.isArray(pkg.metadata.dependencies) && pkg.metadata.dependencies.length > 0) || (typeof pkg.metadata.dependencies === 'object' && !Array.isArray(pkg.metadata.dependencies) && Object.keys(pkg.metadata.dependencies).length > 0)))" class="mt-1">
                <span class="text-xs text-gray-500">Depends: </span>
                <span class="text-xs text-gray-600" x-text="(pkg.dependencies && Array.isArray(pkg.dependencies)) ? pkg.dependencies.join(', ') : (pkg.metadata && pkg.metadata.dependencies ? (Array.isArray(pkg.metadata.dependencies) ? pkg.metadata.dependencies.join(', ') : (typeof pkg.metadata.dependencies === 'object' && !Array.isArray(pkg.metadata.dependencies) ? Object.keys(pkg.metadata.dependencies).join(', ') : '')) : '')"></span>
            </div>
        `;
    } else {
        metadataHTML = `
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
        `;
    }
    
    const extendedMetadataHTML = `
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
            ${(type === 'npm' || type === 'pypi') ? `
                <div x-show="pkg.metadata && pkg.metadata.author" class="text-xs">
                    <span class="text-gray-500">Author: </span>
                    <span class="text-gray-700" x-text="(pkg.metadata && pkg.metadata.author) ? (typeof pkg.metadata.author === 'string' ? pkg.metadata.author : (pkg.metadata.author && pkg.metadata.author.name ? pkg.metadata.author.name : '')) : ''"></span>
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
            ` : ''}
            ${type === 'apk' ? `
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
                <div x-show="pkg.metadata.homepage" class="text-xs">
                    <span class="text-gray-500">Homepage: </span>
                    <a :href="pkg.metadata.homepage" target="_blank" class="text-blue-600 hover:underline" x-text="pkg.metadata.homepage"></a>
                </div>
            ` : ''}
        </div>
    `;
    
    if (fullMetadata) {
        return baseHTML + metadataHTML + extendedMetadataHTML + `
            <div x-show="!pkg.name || !pkg.version || pkg.name === 'N/A' || pkg.version === 'N/A'" class="mt-1">
                <span class="text-xs text-gray-400 italic" x-text="\`ID: \${pkg.id ? (pkg.id.split('/').pop() || pkg.id) : (pkg.version || 'N/A')}\`"></span>
            </div>
        `;
    }
    
    return baseHTML + metadataHTML + extendedMetadataHTML;
}

// Export for use in templates
if (typeof window !== 'undefined') {
    window.PackageListHelpers = {
        getGroupColors,
        renderVersionHTML
    };
}

if (typeof module !== 'undefined' && module.exports) {
    module.exports = {
        getGroupColors,
        renderVersionHTML
    };
}
