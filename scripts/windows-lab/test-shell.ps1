# SSH runs in a service session. Exercise PSEC in the signed-in user's desktop.
$ErrorActionPreference = 'Stop'
$name = 'Wingman-Lab-Shell-' + [guid]::NewGuid()
$log = Join-Path $PSScriptRoot "$name.log"
$principal = New-ScheduledTaskPrincipal -UserId "$env:COMPUTERNAME\wingman" -LogonType Interactive -RunLevel Limited
$pattern = '^TestWindowsSandbox(LaunchesWithNetworkOff|PowerShellStartup|PreservesUserStderrAndExitCode)$'
$argsLine = "/d /c shell.test.exe -test.v -test.timeout 2m -test.run `"$pattern`" > `"$log`" 2>&1"
$action = New-ScheduledTaskAction -Execute $env:ComSpec -WorkingDirectory $PSScriptRoot -Argument $argsLine

# On a warm boot SSH can answer before Windows has signed in the desktop.
$deadline = (Get-Date).AddMinutes(2)
while (!(Get-Process explorer -ErrorAction SilentlyContinue)) {
    if ((Get-Date) -gt $deadline) { throw 'Sign in as wingman on the VNC console and retry.' }
    Start-Sleep -Seconds 1
}
Register-ScheduledTask $name -Action $action -Principal $principal | Out-Null
try {
    $previous = (Get-ScheduledTaskInfo $name).LastRunTime
    Start-ScheduledTask $name
    $deadline = (Get-Date).AddMinutes(3)
    do {
        Start-Sleep -Seconds 1
        $info = Get-ScheduledTaskInfo $name
        if ((Get-Date) -gt $deadline) {
            Stop-ScheduledTask $name
            throw 'Timed out running the desktop shell tests.'
        }
    } while ($info.LastRunTime -eq $previous -or (Get-ScheduledTask $name).State -eq 'Running')
    Get-Content $log
    exit $info.LastTaskResult
} finally {
    Unregister-ScheduledTask $name -Confirm:$false
    Remove-Item $log -ErrorAction SilentlyContinue
}
