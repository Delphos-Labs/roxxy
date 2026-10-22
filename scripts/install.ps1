# Roxxy Windows Installer
# Installs roxxy to %LOCALAPPDATA%\roxxy and optionally adds to PATH

$ErrorActionPreference = "Stop"

$installDir = "$env:LOCALAPPDATA\roxxy"

# Find binaries (check current directory first, then build output)
$roxxyBin = if (Test-Path "roxxy.exe") { "roxxy.exe" } elseif (Test-Path ".\roxxy.exe") { ".\roxxy.exe" } else { $null }
$roarBin = if (Test-Path "roar.exe") { "roar.exe" } elseif (Test-Path ".\roar.exe") { ".\roar.exe" } else { $null }

if (-not $roxxyBin) {
    Write-Host "Error: roxxy.exe not found in current directory." -ForegroundColor Red
    Write-Host "Run 'make' first to build roxxy." -ForegroundColor Yellow
    exit 1
}

# Create install directory
Write-Host "Installing roxxy..." -ForegroundColor Cyan
New-Item -ItemType Directory -Force -Path $installDir | Out-Null

# Copy binaries
Copy-Item -Force $roxxyBin "$installDir\roxxy.exe"
Write-Host "  Installed: $installDir\roxxy.exe" -ForegroundColor Green

if ($roarBin) {
    Copy-Item -Force $roarBin "$installDir\roar.exe"
    Write-Host "  Installed: $installDir\roar.exe" -ForegroundColor Green
}

# Check if already in PATH
$userPath = [Environment]::GetEnvironmentVariable("PATH", "User")
if ($userPath -like "*$installDir*") {
    Write-Host ""
    Write-Host "Already in PATH." -ForegroundColor Green
} else {
    Write-Host ""
    Write-Host "Add to PATH?" -ForegroundColor Yellow
    Write-Host "This will add $installDir to your user PATH." -ForegroundColor Gray
    Write-Host ""
    $response = Read-Host "Press Y to add to PATH, any other key to skip"

    if ($response -eq 'Y' -or $response -eq 'y') {
        [Environment]::SetEnvironmentVariable("PATH", "$userPath;$installDir", "User")
        $env:PATH += ";$installDir"
        Write-Host ""
        Write-Host "Added to PATH." -ForegroundColor Green
        Write-Host "Restart your terminal for changes to take effect." -ForegroundColor Yellow
    } else {
        Write-Host ""
        Write-Host "Skipped PATH modification." -ForegroundColor Gray
        Write-Host "To add manually, run:" -ForegroundColor Gray
        Write-Host "  [Environment]::SetEnvironmentVariable('PATH', `$env:PATH + ';$installDir', 'User')" -ForegroundColor White
    }
}

Write-Host ""
Write-Host "Installation complete!" -ForegroundColor Green
Write-Host ""
Write-Host "Next steps:" -ForegroundColor Cyan
Write-Host "  1. Restart your terminal"
Write-Host "  2. Run: roxxy server"
Write-Host "  3. Or install as service: roxxy service install"
Write-Host ""
