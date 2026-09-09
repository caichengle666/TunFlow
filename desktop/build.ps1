$ErrorActionPreference = "Stop"

Set-Location $PSScriptRoot
$env:CGO_ENABLED = "1"

go mod tidy
go run github.com/wailsapp/wails/v2/cmd/wails@v2.15.0 build

Write-Host "TunFlow desktop build completed under desktop/build/bin."
