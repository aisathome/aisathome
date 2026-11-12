# Silent install, update and uninstall of the Windows installer, on a machine without a GPU,
# an engine or a gateway (the Windows CI job; it changes the machine, so run it only on a
# throwaway one, as administrator):
#
#   pwsh node/packaging/windows/installer_test.ps1 -Setup <aisathome-node-setup-windows-amd64.exe>
#
# It seeds a config, as the setup page writes it, with an extra permission entry for Everyone;
# installs; checks the service (virtual account, delayed automatic start, restart on failure,
# running, answering on 127.0.0.1:7381), the data folder's permissions, the tray (its file, its
# autostart entry, the Start-menu entry, and its sign-in over the agent's pipe), and the
# shortcut; installs again over it while a tray runs (an update closes the tray and keeps
# config and password); checks who may sign in from the tray (a standard user only once
# `service install -ui-user` names it); uninstalls silently while a tray runs (service, tray
# and program gone, the data folder kept with only its documented files); installs again and
# uninstalls with /PURGEDATA (data folder gone); then a fresh install without a config (the
# service runs, "/" leads to the setup page, no engine runs, the tray shows "setup").
#
# SPDX-License-Identifier: Apache-2.0
param([Parameter(Mandatory)][string]$Setup)
$ErrorActionPreference = 'Stop'
$Setup = (Resolve-Path $Setup).Path
$name = 'aisathome-node'
$app = Join-Path $env:ProgramFiles 'AIsAtHome'
$exe = Join-Path $app 'aisathome-node.exe'
$data = Join-Path $env:ProgramData 'AIsAtHome'
$tray = Join-Path $app 'aisathome-tray.exe'
$shortcut = Join-Path $env:ProgramData 'Microsoft\Windows\Start Menu\Programs\AIs@home node.lnk'
$runKey = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Run'
$runValue = 'AIs@home tray'
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
  $trayLog = Join-Path $env:LOCALAPPDATA 'AIsAtHome\tray.log'
  if (Test-Path $trayLog) { Write-Host '----- tray.log'; Get-Content $trayLog -Tail 40 }
  throw $msg
}

# Tray-Check runs `aisathome-tray -check` (as $cred's user when given) and returns its output
# and exit code: the state the tray would show, and whether the agent's pipe gave it a
# sign-in link that signs in once.
function Tray-Check($cred) {
  $out = Join-Path $logs "tray-check-$([guid]::NewGuid()).txt"
  $a = @{ FilePath = $tray; ArgumentList = '-check'; Wait = $true; PassThru = $true; RedirectStandardOutput = $out
    WorkingDirectory = $app; NoNewWindow = $true }
  if ($cred) { $a.Credential = $cred; $a.Remove('NoNewWindow') }
  $p = Start-Process @a
  $text = if (Test-Path $out) { Get-Content -Raw $out } else { '' }
  Write-Host "tray -check (exit $($p.ExitCode)): $text"
  [pscustomobject]@{ Code = $p.ExitCode; Text = "$text" }
}

# Start-Tray starts the tray as this account, as the login autostart would, and checks that it
# keeps running.
function Start-Tray {
  $p = Start-Process -FilePath $tray -PassThru
  Start-Sleep -Seconds 3
  if ($p.HasExited) { Fail "the tray exited at start (exit code $($p.ExitCode))" }
  $p
}

function Check-TrayGone($p, [string]$when) {
  for ($i = 0; $i -lt 20 -and -not $p.HasExited; $i++) { Start-Sleep -Milliseconds 500 }
  if (-not $p.HasExited) { Fail "$when left the tray $($p.Id) running" }
  if (Get-Process -Name 'aisathome-tray' -ErrorAction SilentlyContinue) { Fail "$when left a tray running" }
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
  $lnk = (New-Object -ComObject WScript.Shell).CreateShortcut($shortcut)
  if ($lnk.TargetPath -ne $tray -or $lnk.Arguments -ne '-open') {
    Fail "the Start menu entry runs $($lnk.TargetPath) $($lnk.Arguments), want $tray -open"
  }
  if (Test-Path ($shortcut -replace '\.lnk$', '.url')) { Fail 'the old Internet shortcut is still there' }
  if (-not (Test-Path (Join-Path $app 'aisathome.ico'))) { Fail 'no aisathome.ico' }
  $run = (Get-ItemProperty $runKey -ErrorAction SilentlyContinue).$runValue
  if ($run -ne "`"$tray`"") { Fail "tray autostart is '$run', want `"$tray`"" }
  # The tray signs in over the agent's pipe (this account is an administrator) and sees a
  # configured node that is not connected.
  $c = Tray-Check $null
  if ($c.Code -ne 0 -or $c.Text -notmatch 'signin=ok' -or $c.Text -notmatch 'state=starting') {
    Fail "tray -check: want state=starting and signin=ok"
  }
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

Write-Host '== update (install over it), with a tray running'
$trayProc = Start-Tray
Install
Check-TrayGone $trayProc 'the update'
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

Write-Host '== tray sign-in for a standard user: refused until service install -ui-user names it'
# A standard (not administrator) account: the agent's pipe gives it no link while the password
# file is only for the installing user, and does once `service install -ui-user` grants it.
$ciUser = 'aisathome-ci'
# 14 characters at most: for a longer password net.exe asks "(Y/N)" about pre-Windows 2000
# computers, gets no answer here and fails (CI, 2026-11-12). Upper, lower, digit and symbol.
$ciPass = 'Ci1!a' + [guid]::NewGuid().ToString('N').Substring(0, 9)
$out = net.exe user $ciUser $ciPass /add 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) { Fail "could not add the local user ${ciUser}: $out" }
try {
  $cred = [pscredential]::new("$env:COMPUTERNAME\$ciUser", (ConvertTo-SecureString $ciPass -AsPlainText -Force))
  $ran = $true
  try { $c = Tray-Check $cred } catch { $ran = $false; Write-Warning "skipped: cannot run a process as $ciUser here: $($_.Exception.Message)" }
  if ($ran) {
    if ($c.Code -eq 0 -or $c.Text -notmatch 'signin=failed' -or $c.Text -notmatch 'state=starting') {
      Fail "tray -check as a standard user: want state=starting and a refused sign-in"
    }
    & $exe service install -ui-user $ciUser
    if ($LASTEXITCODE -ne 0) { Fail "service install -ui-user: exit $LASTEXITCODE" }
    $c = Tray-Check $cred
    if ($c.Code -ne 0 -or $c.Text -notmatch 'signin=ok') { Fail "tray -check as $ciUser after -ui-user: want signin=ok" }
  }
} finally {
  net.exe user $ciUser /delete | Out-Null
}
# Back to the installing user, for the checks below.
& $exe service install
if ($LASTEXITCODE -ne 0) { Fail "service install: exit $LASTEXITCODE" }

Write-Host '== uninstall with a tray running, keeping the data'
$trayProc = Start-Tray
Uninstall @()
Check-TrayGone $trayProc 'the uninstall'
if ((Get-ItemProperty $runKey -ErrorAction SilentlyContinue).$runValue) { Fail 'the tray autostart entry is still there' }
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
$c = Tray-Check $null
if ($c.Code -ne 0 -or $c.Text -notmatch 'state=setup' -or $c.Text -notmatch 'signin=ok') {
  Fail 'fresh install: tray -check, want state=setup and signin=ok'
}
Uninstall @('/PURGEDATA')
if (Test-Path $data) { Fail "/PURGEDATA left $data" }
Write-Host 'installer test passed'
# Exit 0 explicitly: the CI's pwsh shell ends with "exit $LASTEXITCODE", and the last native
# command here is Wait-Status's `status -json`, which exits 1 by design (answered, not connected).
exit 0
