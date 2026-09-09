$ErrorActionPreference = "Stop"

Set-Location $PSScriptRoot
$env:CGO_ENABLED = "1"

go mod tidy
go build -trimpath -ldflags "-s -w" -o TunFlow.exe .

Write-Host "TunFlow.exe built successfully: $((Join-Path (Get-Location) 'TunFlow.exe'))"
