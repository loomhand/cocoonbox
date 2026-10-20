# Runs shuttle in the desktop user's console session: the box config and the flows link are renewed first. A logon task
# starts it with a hidden window and starts it again when it exits; the log of the run before is kept as shuttle.log.1.
$ErrorActionPreference = 'Stop'
$dir = Join-Path $env:APPDATA 'cocoon-shuttle'
$log = Join-Path $dir 'shuttle.log'
New-Item -ItemType Directory -Force $dir, 'C:\workspace\flows' | Out-Null
Copy-Item -Force 'C:\ProgramData\cocoonbox\shuttle.toml' (Join-Path $dir 'config.toml')
if (-not (Test-Path (Join-Path $dir 'flows'))) { & cmd.exe /c mklink /J (Join-Path $dir 'flows') 'C:\workspace\flows' | Out-Null }
if (Test-Path $log) { Move-Item -Force $log "$log.1" }
$env:SHUTTLE_SOCKET = '\\.\pipe\cocoon-shuttle-desk'
$env:CUA_DRIVER_RS_TELEMETRY_ENABLED = 'false'
$env:CUA_DRIVER_RS_UPDATE_CHECK = 'false'
$shuttle = Start-Process -FilePath 'C:\Program Files\cocoon-shuttle\cocoon-shuttle.exe' -ArgumentList 'serve' -NoNewWindow -Wait -PassThru `
    -RedirectStandardOutput (Join-Path $dir 'shuttle.out.log') -RedirectStandardError $log
exit $shuttle.ExitCode
