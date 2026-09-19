# Douyinie repository completion gate (CODING_STANDARDS.md §14).
# Usage:
#   powershell -File scripts/gate.ps1         # Fast gate: unit & steering tests (~15s-1m)
#   powershell -File scripts/gate.ps1 -Full   # Full gate: includes heavy seam1 & seam2 integration (~5m)
param(
    [switch]$Full
)
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

    if ($Full) {
        Write-Host "==> Running full test suite (including seam1 & seam2)..."
        go test ./...
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    } else {
        Write-Host "==> Running fast unit & steering tests..."
        go test ./test/steering/... ./cmd/... ./internal/...
        if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
    }
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
