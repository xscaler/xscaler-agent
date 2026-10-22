# Writes or updates the supervisor config and starts the xscaler-collector
# service. The MSI runs it as SYSTEM after installing the service.
#
# - First install: render supervisor.yaml from supervisor.yaml.tmpl.
# - -Token / -Endpoint: replace those values in the existing file, leaving
#   every other line (labels, local edits) as it is.
# - Start the service once the config carries a token.
param(
    [string]$Token = '',
    [string]$Endpoint = ''
)

$ErrorActionPreference = 'Stop'

$installDir = $PSScriptRoot.TrimEnd('\')
$configDir = Join-Path $env:ProgramData 'xScaler Collector'
$stateDir = Join-Path $configDir 'state'
$config = Join-Path $configDir 'supervisor.yaml'
$utf8 = New-Object System.Text.UTF8Encoding $false

New-Item -ItemType Directory -Force -Path $configDir, $stateDir | Out-Null

# The config holds the enrollment token: SYSTEM and Administrators only.
& icacls.exe $configDir /inheritance:r /grant:r '*S-1-5-18:(OI)(CI)F' '*S-1-5-32-544:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw "icacls failed on $configDir ($LASTEXITCODE)" }

if (Test-Path $config) {
    $yaml = [System.IO.File]::ReadAllText($config)
} else {
    $yaml = [System.IO.File]::ReadAllText((Join-Path $installDir 'supervisor.yaml.tmpl'))
    $yaml = $yaml.Replace('@INSTALLDIR@', $installDir).Replace('@STATEDIR@', $stateDir)
}

if ($Endpoint) {
    $yaml = [regex]::Replace($yaml, '(?m)^(\s*endpoint:).*$', "`$1 `"$Endpoint`"")
}
if ($Token) {
    $yaml = [regex]::Replace($yaml, '(?m)^(\s*Authorization:).*$', "`$1 `"Bearer $Token`"")
}

[System.IO.File]::WriteAllText($config, $yaml, $utf8)

if ($yaml -match '(?m)^\s*Authorization:\s*"Bearer \S+"') {
    Start-Service -Name 'xscaler-collector'
} else {
    Write-Output "No enrollment token in $config; the xscaler-collector service is installed but not started."
}
