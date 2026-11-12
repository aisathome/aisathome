# Silent install, update and uninstall of the Windows installer, on a machine without a GPU,
# an engine or a gateway (the Windows CI job; it changes the machine, so run it only on a
# throwaway one, as administrator):
#
#   pwsh node/packaging/windows/installer_test.ps1 -Setup <aisathome-node-setup-windows-amd64.exe>
#
# It seeds a config, as the setup page writes it, with an extra permission entry for Everyone;
# installs; checks the service (virtual account, delayed automatic start, restart on failure,
# running, answering on 127.0.0.1:7381), the data folder's permissions and the shortcut;
# installs again over it (an update keeps config and password); uninstalls silently (service
# and program gone, the data folder kept with only its documented files); installs again and
# uninstalls with /PURGEDATA (data folder gone); then a fresh install without a config (the
# service runs, "/" leads to the setup page, no engine runs).
#
# SPDX-License-Identifier: Apache-2.0
param([Parameter(Mandatory)][string]$Setup)
$ErrorActionPreference = 'Stop'
$Setup = (Resolve-Path $Setup).Path
$name = 'aisathome-node'
$app = Join-Path $env:ProgramFiles 'AIsAtHome'
$exe = Join-Path $app 'aisathome-node.exe'
$data = Join-Path $env:ProgramData 'AIsAtHome'
$shortcut = Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\AIs@home node.url'
$logs = Join-Path ([IO.Path]::GetTempPath()) 'aisathome-installer-test'
New-Item -ItemType Directory -Force $logs | Out-Null
$account = "NT SERVICE\$name"
$admins = @('NT AUTHORITY\SYSTEM', 'BUILTIN\Administrators', $account)
$me = [Security.Principal.WindowsIdentity]::GetCurrent().Name
$step = 0

function Fail([string]$msg) {
  Get-ChildItem $logs -Filter *.log -ErrorAction SilentlyContinue | ForEach-Object {
    Write-Host "----- $($_.Name)"; Get-Content $_.FullName -Tail 60
  }
  $nodeLog = Join-Path $data 'logs\node.log'
  if (Test-Path $nodeLog) { Write-Host '----- node.log'; Get-Content $nodeLog -Tail 40 }
  throw $msg
}

function Invoke-Logged([string]$file, [string[]]$argList) {
  $script:step++
  $log = Join-Path $logs "$script:step-$([IO.Path]::GetFileNameWithoutExtension($file)).log"
  Write-Host "> $file $argList"
  $p = Start-Process -FilePath $file -ArgumentList ($argList + "/LOG=`"$log`"") -Wait -PassThru
  if ($p.ExitCode -ne 0) { Fail "$file $argList`: exit code $($p.ExitCode)" }
}

function Install { Invoke-Logged $Setup @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART') }

function Uninstall([string[]]$extra) {
  Invoke-Logged (Join-Path $app 'unins000.exe') (@('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART') + $extra)
  # The uninstaller finishes in a copy of itself; wait for it to remove the program.
  for ($i = 0; $i -lt 60 -and (Test-Path $exe); $i++) { Start-Sleep -Seconds 1 }
}

function Identities($path) {
  @((Get-Acl $path).Access | ForEach-Object { $_.IdentityReference.Value } | Sort-Object -Unique)
}

function Same($a, $b) { -not (Compare-Object @($a | Sort-Object -Unique) @($b | Sort-Object -Unique)) }

# Answers from the service's agent: exit 1 is "answered, not connected" (3: nothing answered).
function Wait-Status {
  for ($i = 0; $i -lt 30; $i++) {
    $out = & $exe status -json | Out-String
    if ($LASTEXITCODE -eq 1) { return $out | ConvertFrom-Json }
    Start-Sleep -Seconds 1
  }
  Fail "the service's agent did not answer on 127.0.0.1:7381 (last exit $LASTEXITCODE)"
}

function Check-Installed {
  $svc = Get-CimInstance Win32_Service -Filter "Name='$name'"
  if (-not $svc) { Fail 'no service after install' }
  if ($svc.StartName -ne $account) { Fail "service account $($svc.StartName), want $account" }
  $delayed = (Get-ItemProperty "HKLM:\SYSTEM\CurrentControlSet\Services\$name").DelayedAutostart
  if ($svc.StartMode -ne 'Auto' -or $delayed -ne 1) {
    Fail "start mode $($svc.StartMode), DelayedAutostart $delayed; want Auto, 1"
  }
  if ($svc.PathName -ne "`"$exe`"") { Fail "service path $($svc.PathName), want `"$exe`" (quoted)" }
  if ($svc.State -ne 'Running') { Fail "service state $($svc.State), want Running" }
  $failure = sc.exe qfailure $name | Out-String
  if (($failure | Select-String -AllMatches 'RESTART').Matches.Count -lt 3) { Fail "restart on failure not set: $failure" }
  $flag = sc.exe qfailureflag $name | Out-String
  if ($flag -notmatch 'TRUE') { Fail "restart also on the agent's own failures not set: $flag" }
  $user = (Get-Process -Id $svc.ProcessId -IncludeUserName).UserName
  if ($user -ne $account) { Fail "the agent runs as $user, want $account" }

  $st = Wait-Status
  if ($st.node_id -ne 'ci-installer' -or $st.connected -or -not $st.controls.enabled) {
    Fail "unexpected status (want ci-installer, not connected, controls on): $($st | ConvertTo-Json -Depth 4)"
  }

  $acl = Get-Acl $data
  if (-not $acl.AreAccessRulesProtected) { Fail "$data inherits from ProgramData" }
  if ($acl.Owner -ne 'BUILTIN\Administrators') { Fail "$data is owned by $($acl.Owner)" }
  $ids = Identities $data
  if (-not (Same $ids $admins)) { Fail "$data allows $($ids -join ', '); want only $($admins -join ', ')" }
  $ids = Identities (Join-Path $data 'node.json')
  if (-not (Same $ids $admins)) { Fail "node.json allows $($ids -join ', '); want only $($admins -join ', ')" }
  $pwFile = Join-Path $data 'ui-password'
  $ids = Identities $pwFile
  if (-not (Same $ids ($admins + $me))) { Fail "ui-password allows $($ids -join ', '); want $($admins -join ', ') and $me" }
  if ((Get-Content -Raw $pwFile).Trim().Length -lt 12) { Fail 'ui-password is too short' }
  if (-not (Test-Path $shortcut)) { Fail "no Start menu entry $shortcut" }
  $nodeLog = Join-Path $data 'logs\node.log'
  if (-not (Select-String -Quiet -Path $nodeLog -Pattern 'msg=starting')) { Fail "no start line in $nodeLog" }
  return $svc.ProcessId
}

if (Get-Service $name -ErrorAction SilentlyContinue) { throw "service $name exists before the test" }
if (Test-Path $app) { throw "$app exists before the test" }
if (Test-Path $data) { Remove-Item -Recurse -Force $data }

# The config, as the setup page writes it: no engine and no gateway, so the agent runs and
# reports "not connected". The extra entry for Everyone must be gone after the install.
New-Item -ItemType Directory -Force $data | Out-Null
$cfg = Join-Path $data 'node.json'
Set-Content -Encoding ascii $cfg `
  '{"node_id":"ci-installer","gateway_url":"ws://127.0.0.1:1/v1/node/connect","token":"ci","upstream":"http://127.0.0.1:1","models":[{"name":"ci","slots":1,"ctx":4096}]}'
icacls $cfg /grant '*S-1-1-0:(R)' | Out-Null
if ((Identities $cfg) -notcontains 'Everyone') { throw 'could not seed the Everyone entry' }

Write-Host '== install'
Install
$pid1 = Check-Installed
$password = Get-Content -Raw (Join-Path $data 'ui-password')
$config = Get-Content -Raw $cfg

Write-Host '== update (install over it)'
Install
$pid2 = Check-Installed
if ($pid2 -eq $pid1) { Fail 'the update did not restart the service' }
if ((Get-Content -Raw (Join-Path $data 'ui-password')) -ne $password) { Fail 'the update changed the password' }
if ((Get-Content -Raw $cfg) -ne $config) { Fail 'the update changed the config' }

Write-Host '== service stop disables (a queued restart must not start it during an update)'
& $exe service stop
if ($LASTEXITCODE -ne 0) { Fail "service stop: exit $LASTEXITCODE" }
$svc = Get-CimInstance Win32_Service -Filter "Name='$name'"
if ($svc.State -ne 'Stopped' -or $svc.StartMode -ne 'Disabled') {
  Fail "after service stop: state $($svc.State), start mode $($svc.StartMode); want Stopped, Disabled"
}
& $exe service install -start
if ($LASTEXITCODE -ne 0) { Fail "service install -start: exit $LASTEXITCODE" }
Check-Installed | Out-Null

Write-Host '== uninstall, keeping the data'
Uninstall @()
if (Get-Service $name -ErrorAction SilentlyContinue) { Fail 'the service is still there after uninstall' }
if (Get-Process -Name $name -ErrorAction SilentlyContinue) { Fail 'an agent still runs after uninstall' }
if (Test-Path $app) { Fail "$app is still there: $(Get-ChildItem -Recurse -Name $app)" }
if (Test-Path $shortcut) { Fail 'the Start menu entry is still there' }
# The data folder keeps only the documented files: config, password, state and logs.
$left = @(Get-ChildItem -Force -Name $data)
$documented = @('node.json', 'ui-password', 'state.json', 'logs', 'setup-state.json', 'models')
$extra = @($left | Where-Object { $documented -notcontains $_ })
if ($extra.Count -gt 0) { Fail "undocumented files left in ${data}: $($extra -join ', ')" }
foreach ($f in 'node.json', 'ui-password') {
  if ($left -notcontains $f) { Fail "the uninstall deleted $f, which it must keep" }
}

Write-Host '== install again, uninstall with /PURGEDATA'
Install
Check-Installed | Out-Null
Uninstall @('/PURGEDATA')
if (Get-Service $name -ErrorAction SilentlyContinue) { Fail 'the service is still there after uninstall' }
if (Test-Path $app) { Fail "$app is still there" }
if (Test-Path $data) { Fail "/PURGEDATA left $data" }

Write-Host '== fresh install: no config, the setup page'
# As on a volunteer's PC: no node.json and no token. The service must run (not exit and be
# restarted), the shortcut's "/" must lead to the setup page, and the engine must stay stopped
# until the setup has a model and a token.
Install
$svc = Get-CimInstance Win32_Service -Filter "Name='$name'"
if ($svc.State -ne 'Running') { Fail "fresh install: service state $($svc.State), want Running" }
$st = Wait-Status
if ($st.connected -or -not $st.controls.enabled -or $st.setup.mode -ne 'setup' -or $st.setup.configured -or $st.engine_process) {
  Fail "fresh install: want setup mode, not configured, controls on, no engine process: $($st | ConvertTo-Json -Depth 4)"
}
# HttpClient without redirects, not Invoke-WebRequest -MaximumRedirection 0: PowerShell 7
# reports the unfollowed redirect as an error, which -ErrorAction Stop (and this script's
# $ErrorActionPreference) turns into an exception without the response, so the 303 was lost.
$code = 0; $loc = ''; $why = ''
$handler = [Net.Http.HttpClientHandler]::new()
$handler.AllowAutoRedirect = $false
$client = [Net.Http.HttpClient]::new($handler)
try {
  $r = $client.GetAsync('http://127.0.0.1:7381/').GetAwaiter().GetResult()
  $code = [int]$r.StatusCode; $loc = "$($r.Headers.Location)"
} catch {
  $why = "($($_.Exception.Message))"
} finally {
  $client.Dispose()
}
if ($code -ne 303 -or $loc -ne '/setup') { Fail "fresh install: / answered $code $loc $why, want 303 to /setup" }
$page = Invoke-WebRequest -Uri 'http://127.0.0.1:7381/setup' -UseBasicParsing
if ($page.StatusCode -ne 200 -or $page.Content -notmatch 'setup\.js') { Fail "fresh install: /setup answered $($page.StatusCode)" }
Start-Sleep -Seconds 5
$svc2 = Get-CimInstance Win32_Service -Filter "Name='$name'"
if ($svc2.ProcessId -ne $svc.ProcessId -or $svc2.State -ne 'Running') { Fail 'fresh install: the agent restarted or stopped' }
if (Get-Process -Name 'llama-server' -ErrorAction SilentlyContinue) { Fail 'fresh install: an engine runs before the setup' }
if (Test-Path (Join-Path $data 'node.json')) { Fail 'fresh install: node.json appeared without the setup' }
Uninstall @('/PURGEDATA')
if (Test-Path $data) { Fail "/PURGEDATA left $data" }
Write-Host 'installer test passed'
