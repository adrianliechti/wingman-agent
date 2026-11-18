"""Run Wingman's Windows Arm64 tests in a local QEMU guest on Apple Silicon."""

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import platform
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time


FIXTURES = Path(__file__).resolve().parent
REPO = FIXTURES.parent.parent
LAB = REPO / ".windows-lab"
VM = LAB / "vm"
BIN = LAB / "bin"
SSH_PORT = 2226
VNC_PORT = 5916
GUEST = "wingman@127.0.0.1"
SUITES = [
    ("cli", "./cmd/wingman", "^TestExecuteScript"),
    ("agent", "./pkg/code/agent", "^TestTurnReviewAndUndoSurviveJournalReload$"),
    ("terminal", "./pkg/terminal", "^TestWindows"),
    ("app", "./app", "."),
]


def run(*args, **kwargs):
    return subprocess.run(args, check=True, **kwargs)


@contextlib.contextmanager
def lock(name):
    with (LAB / name).open("a") as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def prepare():
    media = {}
    for device, var in [("install", "WINDOWS_ISO"), ("virtio", "VIRTIO_ISO")]:
        path = Path(os.environ.get(var, "")).expanduser()
        if not path.is_file():
            sys.exit(f"Set {var} to the path of the {device} ISO for the first boot.")
        media[device] = str(path.resolve())

    firmware = Path(subprocess.check_output(["brew", "--prefix", "qemu"], text=True).strip()) / "share/qemu"
    with tempfile.TemporaryDirectory(dir=LAB) as temp:
        staging = Path(temp) / "vm"
        setup = staging / "setup"
        setup.mkdir(parents=True)
        run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", staging / "key")
        password = "Wg-" + secrets.token_hex(16) + "9a"
        (staging / "password").write_text(password + "\n")
        template = (FIXTURES / "autounattend.xml").read_text()
        (setup / "autounattend.xml").write_text(template.replace("__ADMIN_PASSWORD__", password))
        shutil.copyfile(staging / "key.pub", setup / "authorized_keys")
        shutil.copyfile(FIXTURES / "setup-wingman.ps1", setup / "setup-wingman.ps1")
        run("hdiutil", "makehybrid", "-quiet", "-iso", "-joliet", "-o", staging / "setup.iso", setup)
        shutil.rmtree(setup)
        shutil.copyfile(firmware / "edk2-aarch64-code.fd", staging / "code.fd")
        shutil.copyfile(firmware / "edk2-arm-vars.fd", staging / "vars.fd")
        (staging / "media.json").write_text(json.dumps(media))
        run("qemu-img", "create", "-q", "-f", "qcow2", staging / "disk.qcow2", "80G")
        staging.rename(VM)


def monitor(command, arguments=None):
    with socket.socket(socket.AF_UNIX) as sock:
        sock.settimeout(5)
        sock.connect("monitor.sock")
        with sock.makefile("rb") as stream:
            if not stream.readline():  # QMP greeting
                raise ConnectionResetError("QEMU closed the monitor")
            for request in [{"execute": "qmp_capabilities"}, {"execute": command, "arguments": arguments or {}}]:
                sock.sendall(json.dumps(request).encode() + b"\n")
                while True:
                    line = stream.readline()
                    if not line:
                        raise ConnectionResetError("QEMU closed the monitor")
                    message = json.loads(line)
                    if "error" in message:
                        raise RuntimeError(message["error"]["desc"])
                    if "return" in message:
                        break
            return message["return"]


def running():
    try:
        monitor("query-status")
        return True
    except (FileNotFoundError, ConnectionRefusedError, ConnectionResetError, BrokenPipeError):
        return False


def start():
    media = []
    if not Path("ready").exists():
        for device, path in json.loads(Path("media.json").read_text()).items():
            if not Path(path).is_file():
                sys.exit(f"Installation media missing: {path}")
            drive = f"if=none,id={device},format=raw,media=cdrom,readonly=on,file={path.replace(',', ',,')}"
            usb = f"usb-storage,drive={device}" + (",bootindex=2" if device == "install" else "")
            media.extend(["-drive", drive, "-device", usb])
        media.extend(["-drive", "if=none,id=setup,format=raw,media=cdrom,readonly=on,file=setup.iso",
                      "-device", "usb-storage,drive=setup"])
    vnc_password = Path("vnc-password")
    if not vnc_password.exists():
        vnc_password.write_text(secrets.token_urlsafe(6))  # VNC accepts eight ASCII characters.
    run("qemu-system-aarch64", "-name", "wingman-win11", "-machine", "virt,highmem=on",
        "-accel", "hvf", "-cpu", "host", "-smp", "4", "-m", "8G", "-rtc", "base=utc",
        "-drive", "if=pflash,format=raw,readonly=on,file=code.fd", "-drive", "if=pflash,format=raw,file=vars.fd",
        "-device", "ramfb", "-device", "qemu-xhci,p2=8,p3=8", "-device", "usb-kbd", "-device", "usb-tablet",
        "-drive", "if=none,id=system,format=qcow2,file=disk.qcow2", "-device", "virtio-blk-pci,drive=system,bootindex=1",
        *media, "-nic", f"user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:{SSH_PORT}-:22",
        "-object", f"secret,id=vncpass,file={vnc_password}", "-vnc", f"127.0.0.1:{VNC_PORT - 5900},password-secret=vncpass",
        "-qmp", "unix:monitor.sock,server=on,wait=off", "-daemonize")


def ssh_options():
    return ["-F", "/dev/null", "-i", "key", "-o", f"Port={SSH_PORT}", "-o", "IdentitiesOnly=yes",
            "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=accept-new",
            "-o", "UserKnownHostsFile=known_hosts", "-o", "LogLevel=ERROR"]


def up():
    with lock("boot.lock"):
        if not VM.is_dir():
            prepare()
        os.chdir(VM)
        if not running():
            start()
        elif monitor("query-vnc").get("auth") != "vnc":
            sys.exit("Restart the lab with task lab:windows:stop and task lab:windows to enable the VNC password.")
        print(f"Windows console: vnc://127.0.0.1:{VNC_PORT}", flush=True)
        print("Copy the Screen Sharing password from the repo: pbcopy < .windows-lab/vm/vnc-password", flush=True)
        if not Path("booted").exists():
            for _ in range(90):
                drives = monitor("query-blockstats")
                if any(d.get("device") == "install" and d["stats"]["rd_bytes"] >= 32 * 1024**2 for d in drives):
                    Path("booted").touch()
                    break
                monitor("send-key", {"keys": [{"type": "qcode", "data": "ret"}]})
                time.sleep(1)
            else:
                sys.exit("Windows did not boot from the ISO. Inspect the VNC console.")
    print("Waiting for Windows OpenSSH. First boot installs Windows and its network driver.", flush=True)
    deadline = time.monotonic() + (600 if Path("ready").exists() else 5400)
    while running():
        probe = subprocess.run(["ssh", *ssh_options(), GUEST, "exit 0"], stdin=subprocess.DEVNULL,
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, text=True)
        if probe.returncode == 0:
            Path("ready").touch()
            return
        if time.monotonic() >= deadline:
            sys.exit(f"SSH timed out: {probe.stderr.strip()}\nInspect the VNC console and C:\\wingman-setup.log in the guest.")
        time.sleep(10)
    sys.exit("Windows stopped before OpenSSH became available.")


def stop():
    if running():
        monitor("system_powerdown")
        for _ in range(90):
            time.sleep(1)
            if not running():
                return
        sys.exit("Windows is still running. Shut down from its VNC console.")


def build():
    BIN.mkdir(exist_ok=True)
    target = dict(os.environ, GOOS="windows", GOARCH="arm64", CGO_ENABLED="0")
    run("go", "build", "-trimpath", "-o", BIN / "wingman.exe", "./cmd/wingman", env=target, cwd=REPO)
    run("go", "tool", "appbundle", "-os", "windows", "-arch", "arm64", "-name", "Wingman Agent",
        "-id", "com.adrianliechti.wingman-agent", "-out", BIN, "-package", ".", cwd=REPO / "app")
    for name, package, _ in SUITES:
        run("go", "test", "-c", "-o", BIN / f"{name}.test.exe", package, env=target, cwd=REPO)
    run("go", "test", "-c", "-o", BIN / "shell.test.exe", "./pkg/harness/tool/shell", env=target, cwd=REPO)


def deploy():
    run("ssh", *ssh_options(), GUEST, "$ErrorActionPreference='Stop'; New-Item -Force -Type Directory C:\\wingman-lab | Out-Null")
    files = ["wingman.exe", "Wingman Agent.exe", "shell.test.exe", *(f"{name}.test.exe" for name, _, _ in SUITES)]
    run("scp", *ssh_options(), *(BIN / name for name in files), FIXTURES / "test-shell.ps1", f"{GUEST}:C:/wingman-lab/")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("up", "test", "ssh", "stop"))
    command = parser.parse_args().command
    if platform.system() != "Darwin" or platform.machine() != "arm64":
        sys.exit("The Windows lab requires an Apple Silicon Mac.")
    os.umask(0o077)
    LAB.mkdir(exist_ok=True)
    if command in ("up", "test"):
        up()
        with lock("deploy.lock"):
            build()
            deploy()
            if command == "test":
                commands = ["wingman.exe --help"]
                # Windows PowerShell splits unquoted dotted flags at the first dot.
                for name, _, pattern in SUITES:
                    commands.append(f"{name}.test.exe '-test.v' '-test.timeout' 5m '-test.run' '{pattern}'")
                for cmd in commands:
                    run("ssh", *ssh_options(), GUEST, f"$ErrorActionPreference='Stop'; cd C:\\wingman-lab; .\\{cmd}; exit $LASTEXITCODE")
                run("ssh", *ssh_options(), GUEST, "powershell -NoProfile -ExecutionPolicy Bypass -File C:/wingman-lab/test-shell.ps1; exit $LASTEXITCODE")
        print("Guest binaries: C:\\wingman-lab", flush=True)
    elif VM.exists():
        os.chdir(VM)
        if command == "stop":
            stop()
        else:
            run("ssh", *ssh_options(), "-t", GUEST)
    elif command == "ssh":
        sys.exit("Run task lab:windows first.")


if __name__ == "__main__":
    try:
        main()
    except (OSError, RuntimeError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
