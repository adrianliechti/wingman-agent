# Runs at first logon in the guest.
$ErrorActionPreference = 'Stop'
Start-Transcript -Path C:\wingman-setup.log -Append
try {
    foreach ($drive in Get-PSDrive -PSProvider FileSystem) {
        $driver = Join-Path $drive.Root 'NetKVM\w11\ARM64\netkvm.inf'
        if (Test-Path $driver) {
            pnputil /add-driver $driver /install
            # PnPUtil can return nonzero when the current driver is already installed.
            if ($LASTEXITCODE -ne 0 -and !(Get-NetAdapter | Where-Object InterfaceDescription -Like '*VirtIO*')) {
                throw "Could not install the VirtIO network driver (PnPUtil exit $LASTEXITCODE)"
            }
            break
        }
    }

    # The feature can require Windows Update while the network is coming up.
    for ($attempt = 1; ; $attempt++) {
        try {
            Add-WindowsCapability -Online -Name OpenSSH.Server~~~~0.0.1.0 | Out-Null
            break
        } catch {
            if ($attempt -ge 20) { throw }
            Start-Sleep -Seconds 30
        }
    }
    $keys = "$env:ProgramData\ssh\administrators_authorized_keys"
    New-Item -Type Directory -Force (Split-Path $keys) | Out-Null
    Copy-Item "$PSScriptRoot\authorized_keys" $keys -Force
    icacls $keys /inheritance:r /grant:r '*S-1-5-32-544:F' '*S-1-5-18:F'
    if ($LASTEXITCODE -ne 0) { throw "Could not secure the SSH authorized keys" }
    New-ItemProperty HKLM:\SOFTWARE\OpenSSH -Name DefaultShell -Force -PropertyType String -Value "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe" | Out-Null
    # Windows can classify the QEMU NIC as Public. The inbox SSH rule is Private.
    if (!(Get-NetFirewallRule -Name 'Wingman-Lab-SSH' -ErrorAction SilentlyContinue)) {
        New-NetFirewallRule -Name 'Wingman-Lab-SSH' -DisplayName 'Wingman lab SSH' -Direction Inbound -Profile Any -RemoteAddress 10.0.2.2 -Protocol TCP -LocalPort 22 -Action Allow | Out-Null
    }
    Set-Service sshd -StartupType Automatic
    Start-Service sshd

    # Keep the dedicated lab user signed in across boots for desktop-session tests.
    Remove-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon' -Name AutoLogonCount -ErrorAction SilentlyContinue
} finally {
    Stop-Transcript
}
