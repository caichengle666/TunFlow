$ErrorActionPreference = "Stop"

Set-Location $PSScriptRoot
$env:CGO_ENABLED = "1"

go mod tidy
go run github.com/wailsapp/wails/v2/cmd/wails@v2.15.0 build

$wintunVersion = "0.14.1"
$archive = Join-Path $env:TEMP "wintun-$wintunVersion.zip"
$extract = Join-Path $env:TEMP "wintun-$wintunVersion"

if (-not (Test-Path (Join-Path $extract "wintun/bin/amd64/wintun.dll"))) {
    Invoke-WebRequest "https://www.wintun.net/builds/wintun-$wintunVersion.zip" -OutFile $archive
    if (Test-Path $extract) { Remove-Item -Recurse -Force $extract }
    Expand-Archive -Path $archive -DestinationPath $extract -Force
}

$target = Join-Path $PSScriptRoot "build/bin/wintun.dll"
Copy-Item (Join-Path $extract "wintun/bin/amd64/wintun.dll") $target -Force

Write-Host "TunFlow desktop build completed under desktop/build/bin."
