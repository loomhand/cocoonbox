# Provisions a cocoon Windows guest as a desk box: the agent's serve as a service under its virtual account, shuttle as a
# logon task in the console session with the agent's toolhost as its service plugin, a 1280x800 virtual display and a
# loopback VNC server in that session for the screen, Git for Windows for the shell, Chrome, the desk runtime and guide, and C:\workspace.
# Runs as SYSTEM inside the guest from an unpacked copy of image/windows; idempotent. The desktop user logs in automatically.
# The time zone is UTC: Windows reads the RTC as local time, and Cloud Hypervisor's RTC runs in UTC.
#   BOX_USER (default cocoon): the desktop user that logs in automatically
#   GW_BASE_URL, GW_API_KEY, MODEL_MAIN, MODEL_COMPUTER: the agent's model contract
#   SHUTTLE_DECIDE (default 1): 0 leaves shuttle without the gw, for a test box with no model access
#   AGENT_EXE: the cocoonbox-agent.exe to install (default vendor\cocoonbox-agent.exe)
#   NOW_UTC: the time to set the clock to, as the base image's clock runs hours off until its time zone is UTC
param([ValidateSet('all', 'fetch', 'system', 'display', 'vnc', 'git', 'chrome', 'shuttle', 'agent', 'check')][string]$Step = 'all')
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$Vendor = Join-Path $PSScriptRoot 'vendor'
$Pinned = @(
    @{ Name = 'cocoon-shuttle-v0.1.8-windows-x64.zip'; Sha = 'c47e591e433af4f356d254f53332fae13c78927e104210f972475fed17c08721'
        Url = 'https://github.com/loomhand/shuttle/releases/download/v0.1.8/cocoon-shuttle-v0.1.8-windows-x64.zip' },
    @{ Name = 'VirtualDisplayDriver-x86.Driver.Only.zip'; Sha = 'e24210692b442b39af763536330ce78b423f19342b7a7792c26de3944e418b3a'
        Url = 'https://github.com/VirtualDrivers/Virtual-Display-Driver/releases/download/25.7.23/VirtualDisplayDriver-x86.Driver.Only.zip' },
    @{ Name = 'nefcon_v1.21.0.zip'; Sha = '4c8e68f6b663598d77b7c3f76459b16fad00994e53ea8af0bd106a805dd5c18b'
        Url = 'https://github.com/nefarius/nefcon/releases/download/v1.21.0/nefcon_v1.21.0.zip' },
    @{ Name = 'tightvnc-2.8.90-gpl-setup-64bit.msi'; Sha = '9693cb4cdc2e7e482e8e3f5890415b57ef87de3369fa5c337db516218a1170ce'
        Url = 'https://www.tightvnc.com/download/2.8.90/tightvnc-2.8.90-gpl-setup-64bit.msi' },
    @{ Name = 'Git-2.56.0.2-64-bit.exe'; Sha = '52188f917b378f00c70ec136bcf090005f30d44fbc4eba0bce759cc6592d60f6'
        Url = 'https://github.com/git-for-windows/git/releases/download/v2.56.0.windows.2/Git-2.56.0.2-64-bit.exe' }
)
# Google publishes no checksum for its enterprise MSI; its Authenticode signature by Google LLC is checked instead.
$Chrome = @{ Name = 'googlechromestandaloneenterprise64.msi'; Url = 'https://dl.google.com/dl/chrome/install/googlechromestandaloneenterprise64.msi' }

$BoxUser = if ($env:BOX_USER) { $env:BOX_USER } else { 'cocoon' }
$Service = 'cocoonbox-agent'
$ServiceAccount = "NT SERVICE\$Service"
$Data = 'C:\ProgramData\cocoonbox'
$AgentDir = 'C:\Program Files\cocoonbox'
$ShuttleDir = 'C:\Program Files\cocoon-shuttle'
$Workspace = 'C:\workspace'
$DeskPipe = '\\.\pipe\cocoon-shuttle-desk'
$Task = 'cocoon-shuttle'
$VncTask = 'tvnserver'
$Display = 'C:\VirtualDisplayDriver'

function Log($m) { Write-Output "[windows] $m" }

function Sha($path) { (Get-FileHash -Algorithm SHA256 $path).Hash.ToLowerInvariant() }

function Fetch {
    New-Item -ItemType Directory -Force $Vendor | Out-Null
    foreach ($p in $Pinned) {
        $file = Join-Path $Vendor $p.Name
        if (-not (Test-Path $file) -or (Sha $file) -ne $p.Sha) {
            Log "download $($p.Name)"
            Invoke-WebRequest -UseBasicParsing -Uri $p.Url -OutFile $file
        }
        if ((Sha $file) -ne $p.Sha) { throw "$($p.Name): sha256 $(Sha $file), want $($p.Sha)" }
    }
    $file = Join-Path $Vendor $Chrome.Name
    if (-not (Test-Path $file)) {
        Log "download $($Chrome.Name)"
        Invoke-WebRequest -UseBasicParsing -Uri $Chrome.Url -OutFile $file
    }
    $sig = Get-AuthenticodeSignature $file
    if ($sig.Status -ne 'Valid' -or $sig.SignerCertificate.Subject -notmatch 'O=Google LLC') { throw "$($Chrome.Name): signature $($sig.Status) by $($sig.SignerCertificate.Subject)" }
    Log "$($Chrome.Name) sha256 $(Sha $file), signed by Google LLC"
}

function System {
    Log 'the clock in UTC, as the RTC is; no hibernation, boot repair, toasts or automatic updates; developer mode allows unprivileged symlinks'
    Set-TimeZone -Id UTC
    if ($env:NOW_UTC) { Set-Date -Date ([DateTime]::Parse($env:NOW_UTC).ToUniversalTime()) | Out-Null }
    # fast startup turns a shutdown into a hibernation, which a cold start under a new Cloud Hypervisor cannot resume;
    # a failed boot then lands in automatic repair, which does not load there
    & powercfg.exe /hibernate off
    & bcdedit.exe /set '{current}' bootstatuspolicy ignoreallfailures | Out-Null
    & bcdedit.exe /set '{current}' recoveryenabled no | Out-Null
    # a toast holds the foreground until it fades, so the desk cannot bring a window to the front meanwhile
    $toasts = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\CurrentVersion\PushNotifications'
    New-Item -Force -Path $toasts | Out-Null
    New-ItemProperty -Force -Path $toasts -Name NoToastApplicationNotification -PropertyType DWord -Value 1 | Out-Null
    $security = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows Defender Security Center\Notifications'
    New-Item -Force -Path $security | Out-Null
    New-ItemProperty -Force -Path $security -Name DisableNotifications -PropertyType DWord -Value 1 | Out-Null
    $updates = 'HKLM:\SOFTWARE\Policies\Microsoft\Windows\WindowsUpdate\AU'
    New-Item -Force -Path $updates | Out-Null
    New-ItemProperty -Force -Path $updates -Name NoAutoUpdate -PropertyType DWord -Value 1 | Out-Null
    New-Item -Force -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock' | Out-Null
    New-ItemProperty -Force -Path 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock' -Name AllowDevelopmentWithoutDevLicense -PropertyType DWord -Value 1 | Out-Null
    $winlogon = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
    if ((Get-ItemProperty $winlogon).DefaultUserName -ne $BoxUser -or (Get-ItemProperty $winlogon).AutoAdminLogon -ne '1') { throw "autologon is not set for $BoxUser" }
}

function Display {
    Log 'the IddCx virtual display at 1280x800, its signer trusted'
    $zip = Join-Path $Vendor 'VirtualDisplayDriver-x86.Driver.Only.zip'
    if (-not (Test-Path $Display)) { Expand-Archive -Force $zip $Display }
    $inf = Get-ChildItem -Recurse -Filter MttVDD.inf $Display | Select-Object -First 1
    $cat = Get-ChildItem -Recurse -Filter *.cat $inf.Directory | Select-Object -First 1
    $signer = (Get-AuthenticodeSignature $cat.FullName).SignerCertificate
    $store = New-Object System.Security.Cryptography.X509Certificates.X509Store('TrustedPublisher', 'LocalMachine')
    $store.Open('ReadWrite'); $store.Add($signer); $store.Close()
    $settings = Get-ChildItem -Recurse -Filter vdd_settings.xml $Display | Select-Object -First 1
    [xml]$x = Get-Content $settings.FullName
    $res = $x.vdd_settings.resolutions
    if (-not ($res.resolution | Where-Object { $_.width -eq '1280' -and $_.height -eq '800' })) {
        $r = $x.CreateElement('resolution')
        foreach ($kv in @(@('width', '1280'), @('height', '800'), @('refresh_rate', '60'))) { $e = $x.CreateElement($kv[0]); $e.InnerText = $kv[1]; [void]$r.AppendChild($e) }
        [void]$res.PrependChild($r)
        $x.Save($settings.FullName)
    }
    Copy-Item -Force $settings.FullName 'C:\VirtualDisplayDriver\vdd_settings.xml'
    if (-not (Get-PnpDevice -FriendlyName 'Virtual Display Driver' -ErrorAction SilentlyContinue)) {
        $nefcon = 'C:\ProgramData\nefcon'
        if (-not (Test-Path $nefcon)) { Expand-Archive -Force (Join-Path $Vendor 'nefcon_v1.21.0.zip') $nefcon }
        $exe = Get-ChildItem -Recurse -Filter nefconw.exe $nefcon | Where-Object { $_.FullName -match 'x64' } | Select-Object -First 1
        & $exe.FullName --create-device-node --hardware-id 'Root\MttVDD' --class-name Display --class-guid '4d36e968-e325-11ce-bfc1-08002be10318' | Out-Null
        & $exe.FullName --install-driver --inf-path $inf.FullName | Out-Null
    }
}

function Vnc {
    Log 'TightVNC 2.8.90 in the console session on 127.0.0.1:5900, security None'
    # as a service, TightVNC grabs a black screen from the IddCx display; in the user's session it grabs the desktop
    if (-not (Test-Path 'C:\Program Files\TightVNC\tvnserver.exe')) {
        $msi = Join-Path $Vendor 'tightvnc-2.8.90-gpl-setup-64bit.msi'
        $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList "/i `"$msi`" /quiet /norestart ADDLOCAL=Server SERVER_REGISTER_AS_SERVICE=0 SERVER_ADD_FIREWALL_EXCEPTION=0"
        if ($p.ExitCode -ne 0) { throw "tightvnc msiexec exit $($p.ExitCode)" }
    }
    if (Get-Service tvnserver -ErrorAction SilentlyContinue) {
        Stop-Service tvnserver
        Set-Service tvnserver -StartupType Disabled
    }
    $sid = (New-Object System.Security.Principal.NTAccount($BoxUser)).Translate([System.Security.Principal.SecurityIdentifier]).Value
    $hive = "Registry::HKEY_USERS\$sid"
    $loaded = Test-Path $hive
    if (-not $loaded) { & reg.exe load "HKU\$sid" "C:\Users\$BoxUser\NTUSER.DAT" | Out-Null }
    $key = "$hive\Software\TightVNC\Server"
    New-Item -Force -Path $key | Out-Null
    foreach ($kv in @(@('UseVncAuthentication', 0), @('UseControlAuthentication', 0), @('LoopbackOnly', 1), @('AllowLoopback', 1), @('RfbPort', 5900), @('AcceptHttpConnections', 0), @('RemoveWallpaper', 0))) {
        New-ItemProperty -Force -Path $key -Name $kv[0] -PropertyType DWord -Value $kv[1] | Out-Null
    }
    if (-not $loaded) { [GC]::Collect(); & reg.exe unload "HKU\$sid" | Out-Null }
    $a = New-ScheduledTaskAction -Execute 'C:\Program Files\TightVNC\tvnserver.exe' -Argument '-run'
    $t = New-ScheduledTaskTrigger -AtLogOn -User $BoxUser
    $p = New-ScheduledTaskPrincipal -UserId $BoxUser -LogonType Interactive -RunLevel Highest
    $s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
    Register-ScheduledTask -Force -TaskName $VncTask -Action $a -Trigger $t -Principal $p -Settings $s | Out-Null
}

function Git {
    Log 'Git for Windows 2.56.0.2, for the shell'
    if (-not (Test-Path 'C:\Program Files\Git\bin\bash.exe')) {
        $p = Start-Process (Join-Path $Vendor 'Git-2.56.0.2-64-bit.exe') -Wait -PassThru -ArgumentList '/VERYSILENT /NORESTART /NOCANCEL /SP- /SUPPRESSMSGBOXES'
        if ($p.ExitCode -ne 0) { throw "git setup exit $($p.ExitCode)" }
    }
}

function Chrome {
    Log 'Google Chrome'
    if (-not (Test-Path 'C:\Program Files\Google\Chrome\Application\chrome.exe')) {
        $p = Start-Process msiexec.exe -Wait -PassThru -ArgumentList "/i `"$(Join-Path $Vendor $Chrome.Name)`" /quiet /norestart"
        if ($p.ExitCode -ne 0) { throw "chrome msiexec exit $($p.ExitCode)" }
    }
}

function Acl($path, [string[]]$grants) {
    & icacls.exe $path /inheritance:r /grant:r @grants | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "icacls $path exit $LASTEXITCODE" }
}

function Shuttle {
    Log 'shuttle v0.1.8, its desk runtime, the guide and the logon task'
    $tmp = Join-Path $env:TEMP "shuttle-$PID"
    Expand-Archive -Force (Join-Path $Vendor 'cocoon-shuttle-v0.1.8-windows-x64.zip') $tmp
    Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue | Stop-ScheduledTask
    Get-Process cocoon-shuttle, cua-driver -ErrorAction SilentlyContinue | Stop-Process -Force
    if (Test-Path $ShuttleDir) { Remove-Item -Recurse -Force $ShuttleDir }
    Move-Item (Join-Path $tmp 'cocoon-shuttle') $ShuttleDir
    Remove-Item -Recurse -Force $tmp
    New-Item -ItemType Directory -Force "$Data\desk\lib", "$Data\tools.d", "$Data\routing.d" | Out-Null
    foreach ($f in 'desk', 'gui_plan', 'png') { Copy-Item -Force "$ShuttleDir\runtime\$f.mts" "$Data\desk\lib\$f.mts" }
    $guide = Get-Content -Raw (Join-Path $PSScriptRoot 'rootfs\ProgramData\cocoonbox\desk\AGENTS.md')
    $routing = (Get-Content -Raw (Join-Path $PSScriptRoot '..\desk\routing.md')).TrimEnd()
    [IO.File]::WriteAllText("$Data\desk\AGENTS.md", $guide.Replace('<!-- routing -->', $routing))
    $toml = ''
    if ($env:SHUTTLE_DECIDE -ne '0') {
        $toml += "base_url = `"$env:GW_BASE_URL`"`ndecisions_url = `"$env:GW_BASE_URL/decisions`"`napi_key = `"unprovisioned`"`ndecide_build = `"typesafe/jev-1.14-########`"`n`n"
    }
    $toml += "[safety]`nindicator = false`nconfirm = `"off`"`napps = []`nkill_hotkey = `"off`"`n`n[tools]`nscript_run = true`nplugins = '$Data\tools.d'`n`n[driver]`noverlay = false`n`n[daemon]`nsocket_group = '$ServiceAccount'`n"
    [IO.File]::WriteAllText("$Data\shuttle.toml", $toml)
    Copy-Item -Force (Join-Path $PSScriptRoot 'rootfs\ProgramData\cocoonbox\shuttle-session.ps1') "$Data\shuttle-session.ps1"
    $a = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument "-NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File `"$Data\shuttle-session.ps1`""
    $t = New-ScheduledTaskTrigger -AtLogOn -User $BoxUser
    $p = New-ScheduledTaskPrincipal -UserId $BoxUser -LogonType Interactive -RunLevel Highest
    $s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) -MultipleInstances IgnoreNew
    Register-ScheduledTask -Force -TaskName $Task -Action $a -Trigger $t -Principal $p -Settings $s | Out-Null
}

function Agent {
    Log 'the agent, its toolhost service spec and serve as a service under its virtual account'
    $exe = if ($env:AGENT_EXE) { $env:AGENT_EXE } else { Join-Path $Vendor 'cocoonbox-agent.exe' }
    if (Get-Service $Service -ErrorAction SilentlyContinue) { Stop-Service -Force $Service }
    Get-Process cocoonbox-agent -ErrorAction SilentlyContinue | Stop-Process -Force
    New-Item -ItemType Directory -Force $AgentDir, "$Data\state", "$Data\identity", $Workspace, "$Workspace\flows" | Out-Null
    Copy-Item -Force $exe "$AgentDir\cocoonbox-agent.exe"
    $spec = Get-Content -Raw (Join-Path $PSScriptRoot '..\desk\service\cocoonbox.json') | ConvertFrom-Json
    $spec.bin = "$AgentDir\cocoonbox-agent.exe"
    [IO.File]::WriteAllText("$Data\tools.d\cocoonbox.json", ($spec | ConvertTo-Json -Depth 20 -Compress))
    if (-not (Get-Service $Service -ErrorAction SilentlyContinue)) {
        New-Service -Name $Service -BinaryPathName "`"$AgentDir\cocoonbox-agent.exe`" serve" -DisplayName 'cocoonbox agent' -StartupType Automatic | Out-Null
    }
    & sc.exe config $Service obj= $ServiceAccount | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "sc config exit $LASTEXITCODE" }
    & sc.exe failure $Service reset= 0 actions= restart/3000/restart/3000/restart/3000 | Out-Null
    $env_ = @(
        'AGENT_LISTEN=127.0.0.1:7443',
        "GW_BASE_URL=$env:GW_BASE_URL", "GW_API_KEY=$env:GW_API_KEY", "MODEL_MAIN=$env:MODEL_MAIN", "MODEL_COMPUTER=$env:MODEL_COMPUTER",
        "SHUTTLE_SOCKET=$DeskPipe", "TOOLHOST_SOCKET=$DeskPipe",
        "CB_HOME=C:\Users\$BoxUser", "CB_WORKSPACE=$Workspace", "CB_LOG=$Data\agent.log"
    )
    foreach ($v in 'GW_BASE_URL', 'GW_API_KEY', 'MODEL_MAIN', 'MODEL_COMPUTER') { if (-not (Get-Item "env:$v" -ErrorAction SilentlyContinue)) { throw "missing $v" } }
    New-ItemProperty -Force -Path "HKLM:\SYSTEM\CurrentControlSet\Services\$Service" -Name Environment -PropertyType MultiString -Value $env_ | Out-Null
    Acl "$Data\state" @('SYSTEM:(OI)(CI)F', 'Administrators:(OI)(CI)F', "${ServiceAccount}:(OI)(CI)M")
    Acl "$Data\identity" @('SYSTEM:(OI)(CI)F', 'Administrators:(OI)(CI)F', "${ServiceAccount}:(OI)(CI)RX")
    if (-not (Test-Path "$Data\agent.log")) { New-Item -ItemType File "$Data\agent.log" | Out-Null }
    Acl "$Data\agent.log" @('SYSTEM:F', 'Administrators:F', "${ServiceAccount}:M")
    & icacls.exe $Workspace /setowner $BoxUser /T /C /Q | Out-Null
    & icacls.exe $Workspace /grant "${BoxUser}:(OI)(CI)F" /Q | Out-Null
    Start-Service $Service
}

function Check {
    if ((Get-Service $Service).Status -ne 'Running') { throw "$Service is $((Get-Service $Service).Status)" }
    $svc = Get-CimInstance Win32_Service -Filter "Name='$Service'"
    if ($svc.StartName -ne $ServiceAccount) { throw "$Service runs as $($svc.StartName)" }
    if (-not (Get-Process tvnserver -ErrorAction SilentlyContinue | Where-Object SessionId -ne 0)) { throw 'tvnserver does not run in the console session' }
    if (-not (Get-NetTCPConnection -State Listen -LocalAddress 127.0.0.1 -LocalPort 5900 -ErrorAction SilentlyContinue)) { throw 'nothing listens on 127.0.0.1:5900' }
    if (-not (Get-NetTCPConnection -State Listen -LocalAddress 127.0.0.1 -LocalPort 7443 -ErrorAction SilentlyContinue)) { throw 'serve does not listen on 127.0.0.1:7443' }
    if (-not (Test-Path 'C:\Program Files\Git\bin\bash.exe')) { throw 'no Git Bash' }
    if (-not (Test-Path 'C:\Program Files\Google\Chrome\Application\chrome.exe')) { throw 'no Chrome' }
    foreach ($f in 'desk', 'gui_plan', 'png') { if (-not (Test-Path "$Data\desk\lib\$f.mts")) { throw "no $f.mts" } }
    if ((Get-Content -Raw "$Data\desk\AGENTS.md") -match '<!-- routing -->') { throw 'the guide lacks the routing' }
    if (-not (Get-ScheduledTask -TaskName $Task -ErrorAction SilentlyContinue)) { throw "no $Task task" }
    & "$AgentDir\cocoonbox-agent.exe" version
    Write-Output 'check: ok'
}

switch ($Step) {
    'all' { Fetch; System; Display; Vnc; Git; Chrome; Shuttle; Agent }
    'fetch' { Fetch }
    'system' { System }
    'display' { Display }
    'vnc' { Vnc }
    'git' { Git }
    'chrome' { Chrome }
    'shuttle' { Shuttle }
    'agent' { Agent }
    'check' { Check }
}
