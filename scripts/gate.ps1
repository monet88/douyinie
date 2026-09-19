# Douyinie repository completion gate (CODING_STANDARDS.md §14).
# Usage: powershell -File scripts/gate.ps1
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Push-Location $repoRoot
try {
    Write-Host "==> Checking gofmt..."
    $goFiles = git ls-files '*.go'
    if ($goFiles) {
        $unformatted = gofmt -l $goFiles
        if ($unformatted) {
            Write-Error "gofmt found unformatted files:`n$($unformatted -join "`n")"
            exit 1
        }
    }

    Write-Host "==> Running go vet..."
    go vet ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "==> Running go test..."
    go test ./...
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "==> Running git diff --check..."
    git diff --check
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "==> Running gitnexus detect-changes..."
    gitnexus detect-changes --scope all
    if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

    Write-Host "`n==> ALL GATES PASSED." -ForegroundColor Green
} finally {
    Pop-Location
}
