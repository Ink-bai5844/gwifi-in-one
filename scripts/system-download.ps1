param(
    [ValidateSet('prepare', 'install', 'status', 'dual', 'wired', 'wifi', 'off', 'uninstall')]
    [string]$Action = 'status'
)

$ErrorActionPreference = 'Stop'
$taskNode = Get-Command node -ErrorAction Stop
$taskHelper = Join-Path $PSScriptRoot 'gwifi-system.cjs'
if ($Action -in @('dual', 'wired', 'wifi', 'off')) {
    & $taskNode.Source $taskHelper mode $Action
} else {
    & $taskNode.Source $taskHelper $Action
}
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
