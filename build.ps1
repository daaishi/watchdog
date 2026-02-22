Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

$projectRoot = $PSScriptRoot
$distDir = Join-Path $projectRoot "dist"

# ── Version ──────────────────────────────────────────────────────────────

$versionFile = Join-Path $projectRoot "VERSION"
$current = (Get-Content $versionFile -Raw).Trim()
$parts = $current -split "\."
$major = [int]$parts[0]
$minor = [int]$parts[1]
$patch = [int]$parts[2]

Write-Host ""
Write-Host "  Current version: v$current" -ForegroundColor Cyan
Write-Host ""
Write-Host "  [1] Patch  -> v$major.$minor.$($patch+1)"
Write-Host "  [2] Minor  -> v$major.$($minor+1).0"
Write-Host "  [3] Major  -> v$($major+1).0.0"
Write-Host "  [4] No change (build as v$current)"
Write-Host ""

$choice = Read-Host "  Select (1-4)"

switch ($choice) {
    "1" { $patch++ }
    "2" { $minor++; $patch = 0 }
    "3" { $major++; $minor = 0; $patch = 0 }
    "4" { }
    default {
        Write-Host "  Invalid selection. Aborted." -ForegroundColor Red
        exit 1
    }
}

$version = "$major.$minor.$patch"
Set-Content -Path $versionFile -Value $version -NoNewline

# ── Kill running dist\watchdog.exe ───────────────────────────────────────

$distExe = Join-Path $distDir "watchdog.exe"
if (Test-Path $distExe) {
    $resolved = (Resolve-Path $distExe).Path
    $procs = Get-Process -Name "watchdog" -ErrorAction SilentlyContinue |
        Where-Object { $_.Path -eq $resolved }
    if ($procs) {
        Write-Host "  Stopping running watchdog.exe ..." -ForegroundColor Yellow
        $procs | Stop-Process -Force
        Start-Sleep -Seconds 2
    }
}

# ── Build ────────────────────────────────────────────────────────────────

if (-not (Test-Path $distDir)) { New-Item -ItemType Directory -Path $distDir | Out-Null }

Write-Host ""
Write-Host "  Building watchdog.exe v$version ..." -ForegroundColor Green
Write-Host ""

$ldflags = "-s -w -X main.Version=$version"
Push-Location $projectRoot
& go build -ldflags $ldflags -o (Join-Path $distDir "watchdog.exe") .
$buildResult = $LASTEXITCODE
Pop-Location

if ($buildResult -ne 0) {
    Write-Host "  Build FAILED." -ForegroundColor Red
    Read-Host "  Press Enter to exit"
    exit 1
}

# ── Copy assets ──────────────────────────────────────────────────────────

Copy-Item -Force (Join-Path $projectRoot "config.json") (Join-Path $distDir "config.json")
Copy-Item -Force (Join-Path $projectRoot "README.md")   (Join-Path $distDir "README.md")

# ── Create zip (flat structure) ──────────────────────────────────────────

$zipName = "watchdog-v$version.zip"
$zipPath = Join-Path $projectRoot $zipName

Write-Host "  Creating $zipName ..." -ForegroundColor Green

if (Test-Path $zipPath) { Remove-Item $zipPath }

Push-Location $distDir
Compress-Archive -Path "watchdog.exe", "config.json", "README.md" -DestinationPath $zipPath
Pop-Location

# ── Summary ──────────────────────────────────────────────────────────────

Write-Host ""
Write-Host "  Done!" -ForegroundColor Green
Write-Host ""
Write-Host "  dist\"
Get-ChildItem $distDir | ForEach-Object { Write-Host "    $($_.Name)" }
Write-Host ""
Write-Host "  Archive: $zipName"
Write-Host "  Version: v$version"
Write-Host ""
Read-Host "  Press Enter to exit"
