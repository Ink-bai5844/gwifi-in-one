$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
Push-Location -LiteralPath $projectRoot
try {
    $goCommand = Get-Command go -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if (!$goCommand) {
        throw 'Go was not found in PATH. Install Go 1.26 or later, then restart your terminal and editor.'
    }
    $goExe = $goCommand.Source
    Write-Output "Go: $goExe"
    & $goExe version
    if ($LASTEXITCODE -ne 0) { throw 'Go version check failed' }
    & $goExe test ./...
    if ($LASTEXITCODE -ne 0) { throw 'Tests failed' }
    & $goExe vet ./...
    if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
    New-Item -ItemType Directory -Force -Path 'bin' | Out-Null
    & $goExe build -trimpath -o 'bin\gwifi.exe' ./cmd/gwifi
    if ($LASTEXITCODE -ne 0) { throw 'Build failed' }
    Write-Output (Join-Path $projectRoot 'bin\gwifi.exe')
} finally {
    Pop-Location
}
