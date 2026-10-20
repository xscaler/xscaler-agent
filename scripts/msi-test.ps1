# Install, start, upgrade and uninstall the xScaler Collector MSI. Run as
# Administrator on a throwaway Windows host (the windows-2022 CI runner):
#
#   scripts/msi-test.ps1 -OldMsi old.msi -NewMsi new.msi
#
# <OldMsi> is the same build stamped with a lower version. The test installs
# it silently with ENROLLMENT_TOKEN and ENDPOINT, checks the service runs the
# collector, edits the config, upgrades without properties and fails unless
# the edits survive and the service comes back, then uninstalls.
param(
    [Parameter(Mandatory)] [string]$OldMsi,
    [Parameter(Mandatory)] [string]$NewMsi
)

$ErrorActionPreference = 'Stop'

$service = 'xscaler-collector'
$installDir = Join-Path $env:ProgramFiles 'xScaler Collector'
$configDir = Join-Path $env:ProgramData 'xScaler Collector'
$config = Join-Path $configDir 'supervisor.yaml'
$token = 'xse_msi_test_token'
# Nothing listens here: the supervisor retries and runs the collector anyway.
$endpoint = 'ws://127.0.0.1:4320/v1/opamp'
$logDir = if ($env:RUNNER_TEMP) { $env:RUNNER_TEMP } else { $env:TEMP }

function Fail([string]$msg) {
    Write-Host "FAIL: $msg"
    Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = $service } -MaxEvents 20 -ErrorAction SilentlyContinue |
        Format-List TimeCreated, Message
    exit 1
}
function Step([string]$msg) { Write-Host "--- $msg" }

function Invoke-Msiexec([string[]]$msiArgs, [string]$log) {
    $p = Start-Process msiexec.exe -ArgumentList ($msiArgs + @('/qn', '/norestart', '/l*v', "`"$log`"")) -Wait -PassThru
    if ($p.ExitCode -ne 0) {
        Get-Content $log -Tail 60
        Fail "msiexec $($msiArgs -join ' ') exited $($p.ExitCode)"
    }
}

function Wait-Running {
    for ($i = 0; $i -lt 60; $i++) {
        $svc = Get-Service $service -ErrorAction SilentlyContinue
        if ($svc -and $svc.Status -eq 'Running' -and (Get-Process xscaler-collector -ErrorAction SilentlyContinue)) {
            return
        }
        Start-Sleep 1
    }
    Fail "service or collector process not running"
}

function Installed-Version {
    $key = Get-ChildItem 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall' |
        Get-ItemProperty | Where-Object { $_.DisplayName -eq 'xScaler Collector' }
    if (-not $key) { return $null }
    return @($key)[0].DisplayVersion
}

Step "install $OldMsi"
$oldLog = Join-Path $logDir 'msi-old.log'
Invoke-Msiexec @('/i', "`"$((Resolve-Path $OldMsi).Path)`"", "ENROLLMENT_TOKEN=$token", "ENDPOINT=$endpoint") $oldLog
$oldVersion = Installed-Version
Write-Host "installed $oldVersion"

foreach ($f in 'xscaler-collector.exe', 'xscaler-supervisor.exe') {
    if (-not (Test-Path (Join-Path $installDir $f))) { Fail "$f not installed in $installDir" }
}
if (Select-String -Path $oldLog -Pattern $token -SimpleMatch -Quiet) { Fail "enrollment token written to the MSI log" }

Step "service"
$svc = Get-CimInstance Win32_Service -Filter "Name='$service'"
if (-not $svc) { Fail "service not registered" }
if ($svc.StartMode -ne 'Auto') { Fail "service start mode is $($svc.StartMode)" }
if ($svc.PathName -notmatch 'xscaler-supervisor\.exe"? --config') { Fail "service command line: $($svc.PathName)" }
Wait-Running

Step "config"
$yaml = Get-Content -Raw $config
if ($yaml -notmatch [regex]::Escape("Bearer $token")) { Fail "token not in $config" }
if ($yaml -notmatch [regex]::Escape($endpoint)) { Fail "endpoint not in $config" }
if ($yaml -match '(?m)^\s*reports_remote_config:') { Fail "config sets reports_remote_config" }
$acl = (Get-Acl $configDir).Access | ForEach-Object { $_.IdentityReference.Value }
foreach ($id in $acl) {
    if ($id -notmatch 'SYSTEM|Administrators') { Fail "$configDir grants access to $id" }
}

Step "edit config"
$edited = $yaml -replace '(?m)^      # environment: prod\r?$', '      environment: msi-test'
if ($edited -eq $yaml) { Fail "could not edit $config" }
[System.IO.File]::WriteAllText($config, $edited, (New-Object System.Text.UTF8Encoding $false))
Restart-Service $service
Wait-Running

Step "upgrade to $NewMsi"
$newLog = Join-Path $logDir 'msi-new.log'
Invoke-Msiexec @('/i', "`"$((Resolve-Path $NewMsi).Path)`"") $newLog
$newVersion = Installed-Version
Write-Host "upgraded $oldVersion -> $newVersion"
if ($newVersion -eq $oldVersion) { Fail "version did not change" }
$after = Get-Content -Raw $config
if ($after -ne $edited) { Fail "upgrade changed $config" }
$products = @(Get-ChildItem 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall' |
    Get-ItemProperty | Where-Object { $_.DisplayName -eq 'xScaler Collector' })
if ($products.Count -ne 1) { Fail "$($products.Count) xScaler Collector products installed after upgrade" }
Wait-Running

Step "uninstall"
Invoke-Msiexec @('/x', "`"$((Resolve-Path $NewMsi).Path)`"") (Join-Path $logDir 'msi-remove.log')
if (Get-Service $service -ErrorAction SilentlyContinue) { Fail "service still registered" }
if (Get-Process xscaler-collector, xscaler-supervisor -ErrorAction SilentlyContinue) { Fail "processes left running" }
if (Test-Path (Join-Path $installDir 'xscaler-collector.exe')) { Fail "binaries left in $installDir" }
# The config is not MSI-owned: a reinstall picks it up again.
if (-not (Test-Path $config)) { Fail "uninstall removed $config" }

Write-Host "PASS: MSI install, start, upgrade ($oldVersion -> $newVersion) and uninstall"
