# Developer setup of a source checkout on Windows: builds the dashboard bundle
# and icebath.exe with it embedded, then hands the binary to its own
# install-self verb to publish the command, seed the home and sync skills.
#   -RuntimeOnly          build only, publish nothing
#   -TestExtras           workspaces the test environment prepares, every workspace when empty
param([switch]$RuntimeOnly, [string]$TestExtras = '')

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

Write-Host ""
Write-Host "Icebath Setup" -ForegroundColor Cyan
Write-Host ""
if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    Write-Host "x Go is required (see go.mod for the version)" -ForegroundColor Red
    exit 1
}

Write-Host "-> Building the dashboard bundle..." -ForegroundColor Cyan
go run ./cmd/icebath-build web
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "-> Building icebath..." -ForegroundColor Cyan
$env:CGO_ENABLED = "0"
go build -tags webdist,goolm -trimpath -o icebath.exe ./cmd/icebath
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "OK Built $PSScriptRoot\icebath.exe" -ForegroundColor Green

Write-Host "-> Preparing the test environment..." -ForegroundColor Cyan
go mod download
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
$npmArgs = @("ci", "--no-audit", "--no-fund")
if ($TestExtras) {
    $npmArgs += "--include-workspace-root=true"
    foreach ($w in $TestExtras.Split(",")) { $npmArgs += @("--workspace", $w.Trim()) }
}
npm @npmArgs
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "OK Test environment ready" -ForegroundColor Green

if ($RuntimeOnly) { exit 0 }

if (-not (Test-Path ".env") -and (Test-Path ".env.example")) {
    Copy-Item ".env.example" ".env"
    Write-Host "OK Created .env from template" -ForegroundColor Green
}

& .\icebath.exe install-self --dir $PSScriptRoot --from-checkout --non-interactive
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host ""
Write-Host "OK Setup complete!" -ForegroundColor Green
Write-Host ""
