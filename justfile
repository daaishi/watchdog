# Watchdog build / release tasks — run with `just <recipe>`
# Requires: just, Go, PowerShell 7 (pwsh), gh (for release).

set shell := ["pwsh", "-NoProfile", "-Command"]

# List available recipes
default:
    @just --list

# Print the current version
version:
    @(Get-Content VERSION -Raw).Trim()

# Run locally for development (Ctrl+C to stop)
run:
    go run .

# Build the release binary into dist/ (version embedded from VERSION)
build:
    #!pwsh
    $ErrorActionPreference = 'Stop'
    $v = (Get-Content VERSION -Raw).Trim()
    New-Item -ItemType Directory -Force dist | Out-Null
    # Stop a running dist/watchdog.exe so the build can overwrite it.
    $exe = (Resolve-Path -ErrorAction SilentlyContinue dist/watchdog.exe)
    if ($exe) {
        Get-Process watchdog -ErrorAction SilentlyContinue |
            Where-Object { $_.Path -eq $exe.Path } | Stop-Process -Force
        Start-Sleep -Milliseconds 500
    }
    Write-Host "Building watchdog.exe v$v ..." -ForegroundColor Green
    go build -ldflags "-s -w -X main.Version=$v" -o dist/watchdog.exe .
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
    Write-Host "-> dist/watchdog.exe (v$v)" -ForegroundColor Green

# Build + assemble a flat zip (watchdog.exe + clean config + README)
package: build
    #!pwsh
    $ErrorActionPreference = 'Stop'
    $v = (Get-Content VERSION -Raw).Trim()
    Copy-Item -Force config.dist.json dist/config.json
    Copy-Item -Force README.md dist/README.md
    $zip = "watchdog-v$v.zip"
    if (Test-Path $zip) { Remove-Item $zip }
    Compress-Archive -Path dist/watchdog.exe, dist/config.json, dist/README.md -DestinationPath $zip
    Write-Host "-> $zip" -ForegroundColor Green
    Get-Item $zip | Select-Object Name, Length

# Bump VERSION (patch | minor | major) and commit
bump level:
    #!pwsh
    $ErrorActionPreference = 'Stop'
    $p = (Get-Content VERSION -Raw).Trim().Split('.')
    $maj = [int]$p[0]; $min = [int]$p[1]; $pat = [int]$p[2]
    switch ('{{level}}') {
        'patch' { $pat++ }
        'minor' { $min++; $pat = 0 }
        'major' { $maj++; $min = 0; $pat = 0 }
        default { throw "level must be: patch | minor | major" }
    }
    $v = "$maj.$min.$pat"
    Set-Content VERSION -Value $v -NoNewline
    git add VERSION
    git commit -m "Bump version to $v"
    Write-Host "Bumped to v$v" -ForegroundColor Green

# Package, push, and create/update the GitHub release for the current VERSION
release: package
    #!pwsh
    $ErrorActionPreference = 'Stop'
    $v = (Get-Content VERSION -Raw).Trim()
    $tag = "v$v"
    $zip = "watchdog-$tag.zip"
    git push origin HEAD
    gh release view $tag *> $null
    if ($LASTEXITCODE -eq 0) {
        Write-Host "Release $tag exists — uploading asset ..." -ForegroundColor Yellow
        gh release upload $tag $zip --clobber
    } else {
        gh release create $tag $zip --title $tag --generate-notes
    }
    Write-Host "Released $tag" -ForegroundColor Green

# Remove build artifacts
clean:
    #!pwsh
    Remove-Item -Recurse -Force dist -ErrorAction SilentlyContinue
    Remove-Item -Force watchdog-v*.zip -ErrorAction SilentlyContinue
    Write-Host "cleaned" -ForegroundColor Green
