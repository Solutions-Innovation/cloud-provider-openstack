# Windows Post-Boot Cleanup via `virsh qemu-agent-command`

This guide covers the post-boot workflow for removing VMware Tools from a
Windows guest that was converted by this project's `CONVERT_MODE=direct-v2v`
tooling (`convert-and-import.sh` + `virt-v2v-in-place`) and has since been
booted on KVM/QEMU (e.g. as an OpenStack Nova instance, or directly as a
libvirt domain during testing).

## Why this is a separate, manual, post-boot step

VMware Tools' own MSI uninstaller cannot complete once the guest is no
longer running on a VMware hypervisor — its first custom action
(`VM_LogStart` or `VM_CheckRequirements`, depending on the VMware Tools
version) validates it is still running on VMware and deliberately fails
with MSI error 1603 otherwise. This is a documented limitation of VMware
Tools itself (see Broadcom/VMware KB 414772 and Red Hat KB 7080999 for
independent confirmation of the same failure in other migration tooling),
not something fixable from this project's side.

To work around it, `convert-and-import.sh` uploads (but does not execute)
`remove-vmware-tools.ps1` into the converted guest's filesystem, at
`C:\Program Files\Guestfs\remove-vmware-tools.ps1`, during conversion (see
`UPLOAD_VMWARE_CLEANUP_SCRIPT` in `convert-and-import.sh`'s header comment).
That script sidesteps the broken uninstaller entirely: instead of calling
`msiexec`, it removes the Windows Installer registry records that make
`Apps & Features` consider VMware Tools "installed", stops and deletes the
VMware services, and deletes `C:\Program Files\VMware` and
`C:\Program Files\Common Files\VMware`.

Running it is deliberately **not** automated as part of conversion or as a
firstboot script: removing registry/service/file state is an irreversible
guest-side change, and should happen under an administrator's direct
control, once the guest has actually booted and can be verified — not
unattended during an offline conversion run where there is no way to
confirm the guest is in a healthy state first.

## Prerequisites

- The guest has been converted with `CONVERT_MODE=direct-v2v` and booted
  successfully on its new (KVM) hardware.
- QEMU Guest Agent (`qemu-ga`) is installed and running inside the guest —
  this is installed by one of `virt-v2v`'s own firstboot scripts
  (`install-qemu-ga.bat`) during conversion, so it should already be
  present without any extra steps.
- You have `virsh` access to the hypervisor host the guest is running on,
  with permission to run `qemu-agent-command` against the domain (this
  typically requires root/sudo, since it talks to libvirt's system
  instance).
- You know the guest's libvirt domain name (`virsh list` to find it).

Throughout this guide, `<dom>` is a placeholder for the domain name (e.g.
`instance-0000000f`).

## Overview of the workflow

1. Confirm the domain is running and reachable via the guest agent.
2. Inspect the guest's current state: OS version, VMware Tools/service
   status, QEMU Guest Agent connectivity, network configuration.
3. Confirm `remove-vmware-tools.ps1` is actually present in the guest's
   filesystem (it's uploaded during conversion; verify it wasn't skipped
   — see `OPERATIONS-GUIDE.md`'s troubleshooting entry on
   `UPLOAD_VMWARE_CLEANUP_SCRIPT` failures, most commonly caused by the
   guest's NTFS partition having a "hibernated" flag set).
4. Run the script.
5. Collect and inspect its output.
6. Re-verify the guest's state to confirm VMware Tools is actually gone.

Every step below uses the same two primitives:

- `guest-exec` — submit a command to run inside the guest; returns a `pid`.
- `guest-exec-status` — poll that `pid` for completion, exit code, and
  captured stdout/stderr (base64-encoded).

Because `qemu-agent-command`'s JSON argument has to survive both shell
quoting on the host and JSON parsing by QEMU, the most reliable way to run
anything beyond a trivial command is to base64-encode the PowerShell
command itself (UTF-16LE, which is what PowerShell's own
`-EncodedCommand` flag expects) and pass it via `-EncodedCommand`, rather
than trying to escape a raw command string through multiple layers of
quoting. All examples below follow this pattern.

## Step 1: Confirm the domain is running and the agent is reachable

```bash
virsh list --all
```

Look for `<dom>` with state `running`. Then ping the guest agent:

```bash
virsh qemu-agent-command <dom> '{"execute":"guest-ping"}'
```

Expected output: `{"return":{}}`. An empty `{}` return (not an error) means
the agent responded — this is QGA's standard liveness check. If this
times out or errors, QEMU Guest Agent is not running/reachable inside the
guest; stop here and investigate that first (check
`Get-Service QEMU-GA` inside the guest via console/RDP if you have another
way in) before attempting anything below.

## Step 2: Inspect the guest's current state

### 2.1 OS version

```bash
virsh qemu-agent-command <dom> '{"execute":"guest-get-osinfo"}'
```

Example real output from a converted Windows Server 2019 guest:

```json
{"return":{"name":"Microsoft Windows","kernel-release":"17763","version":"Microsoft Windows Server 2019","variant":"server","pretty-name":"Windows Server 2019 Standard Evaluation","version-id":"2019","variant-id":"server","kernel-version":"10.0","machine":"x86_64","id":"mswindows"}}
```

### 2.2 Network configuration (confirms VirtIO NIC is actually up)

```bash
virsh qemu-agent-command <dom> '{"execute":"guest-network-get-interfaces"}'
```

A real converted guest should show a working `Ethernet` interface with an
assigned IPv4 address and a valid MAC — if this comes back empty or only
shows a loopback interface, the VirtIO network driver did not get
injected/bound correctly during conversion; that is a separate problem
from VMware Tools cleanup and should be resolved first.

### 2.3 VMware Tools / service status (the "before" baseline)

This is where the helper pattern (base64-encoded `-EncodedCommand`) is
introduced. From here on, every PowerShell command is run the same way:

```bash
# 1. Prepare the base64-encoded command (UTF-16LE, no BOM issues with iconv)
PS_CMD='Get-Service | Where-Object {$_.DisplayName -like "VMware*"} | Select-Object Name,Status | ConvertTo-Json -Compress'
ENC=$(echo -n "$PS_CMD" | iconv -f UTF-8 -t UTF-16LE | base64 -w0)

# 2. Submit it
virsh qemu-agent-command <dom> "{\"execute\":\"guest-exec\",\"arguments\":{\"path\":\"C:\\\\Windows\\\\System32\\\\WindowsPowerShell\\\\v1.0\\\\powershell.exe\",\"arg\":[\"-NoLogo\",\"-NonInteractive\",\"-NoProfile\",\"-EncodedCommand\",\"$ENC\"],\"capture-output\":true}}"
# -> {"return":{"pid":1432}}

# 3. Poll for completion (commands are asynchronous; re-run until "exited":true)
virsh qemu-agent-command <dom> '{"execute":"guest-exec-status","arguments":{"pid":1432}}'
```

A completed response looks like:

```json
{"return":{"exitcode":0,"out-data":"<base64>","out-truncated":false,"exited":true}}
```

Decode `out-data` with `base64 -d` (it's UTF-8 text from `ConvertTo-Json`,
not UTF-16, since that's PowerShell's own stdout encoding for redirected
output — only the *input* `-EncodedCommand` needs to be UTF-16LE).

Real "before" output from a freshly converted guest, prior to any
cleanup, for the same command above:

```json
[{"Name":"VGAuthService","Status":4},{"Name":"vm3dservice","Status":4},{"Name":"VMTools","Status":4},{"Name":"vmvss","Status":1}]
```

`Status` is the raw `ServiceControllerStatus` enum value:
`1=Stopped, 2=StartPending, 3=StopPending, 4=Running, 5=ContinuePending,
6=PausePending, 7=Paused`. In this baseline, `VGAuthService`,
`vm3dservice`, and `VMTools` are `Running` (4); `vmvss` is `Stopped` (1),
which is expected — it's a VSS-related service that only runs on demand.

Also check the Windows Installer registration that `Apps & Features`
reads from — this is the actual target of the cleanup script, not just
the services:

```bash
PS_CMD='Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue | Where-Object {$_.DisplayName -like "*VMware*"} | Select-Object DisplayName | ConvertTo-Json -Compress'
```

Run through the same submit/poll pattern. Before cleanup, this returns a
single entry (`DisplayName: "VMware Tools"`); an empty/missing `out-data`
field in the response means no matching entries were found.

## Step 3: Confirm the cleanup script is present

`convert-and-import.sh` uploads `remove-vmware-tools.ps1` automatically
during conversion (controlled by `UPLOAD_VMWARE_CLEANUP_SCRIPT`, default
`true`), but the upload can fail without failing the overall conversion
(see `OPERATIONS-GUIDE.md`'s troubleshooting table) — most commonly
because the guest's NTFS partition had a "hibernated" or "not cleanly
unmounted" flag set at the time of conversion, which makes `libguestfs`
refuse to mount it read-write. Always verify the script actually made it
into the guest before trying to run it:

```bash
PS_CMD='Test-Path "C:\Program Files\Guestfs\remove-vmware-tools.ps1"'
```

Submit/poll as above. Expected output: `True`. If you get `False`, the
upload did not happen (or the file was removed) — copy the script in by
some other means (e.g. via an RDP/console session, or by mounting the
guest disk offline with `guestfish` from a maintenance context and
re-running the upload step from `convert-and-import.sh`) before
continuing.

## Step 4: Run the script

`guest-exec` has no interactive stdin channel, so the script must be run
with `-Force` to skip its interactive `y/n` confirmation prompt (the
prompt is there for a human running it directly in an RDP/console
session; it would otherwise hang forever waiting for input that can never
arrive over `guest-exec`):

```bash
PS_CMD='& "C:\Program Files\Guestfs\remove-vmware-tools.ps1" -Force'
ENC=$(echo -n "$PS_CMD" | iconv -f UTF-8 -t UTF-16LE | base64 -w0)

virsh qemu-agent-command <dom> "{\"execute\":\"guest-exec\",\"arguments\":{\"path\":\"C:\\\\Windows\\\\System32\\\\WindowsPowerShell\\\\v1.0\\\\powershell.exe\",\"arg\":[\"-NoLogo\",\"-NonInteractive\",\"-NoProfile\",\"-EncodedCommand\",\"$ENC\"],\"capture-output\":true}}"
# -> {"return":{"pid":4032}}
```

**This step is irreversible.** It stops and deletes VMware services,
deletes `C:\Program Files\VMware` and
`C:\Program Files\Common Files\VMware`, and removes the registry entries
that make Windows consider VMware Tools installed. There is no undo
beyond re-converting the guest from the original source image. Only
proceed once Steps 1-3 above have confirmed the guest is in the expected
state.

Poll `guest-exec-status` with the returned `pid` until `"exited":true`.
**Verified in testing: this took close to a minute** (the script's own
registry enumeration over `HKEY_CLASSES_ROOT\Installer\Products` is the
slow part) — do not assume a quick `"exited":false` after a few seconds
means something is wrong; keep polling every 10-15 seconds.

## Step 5: Collect and inspect the result

A successful completion looks like:

```json
{"return":{"exitcode":0,"out-data":"<base64>", ...}}
```

Decode `out-data`. Real output from a verified run:

```
Looking up VMware Tools product registration...
Found VMware Tools: reg_id=46E471FAFC2268349ACE372F58379989 msi_id=AF174E64-22CF-4386-A9EC-73F285739998

The following registry keys, filesystem folders, and services will be removed:
  Registry::HKEY_CLASSES_ROOT\Installer\Features\46E471FAFC2268349ACE372F58379989
  Registry::HKEY_CLASSES_ROOT\Installer\Products\46E471FAFC2268349ACE372F58379989
  HKLM:\SOFTWARE\Classes\Installer\Features\46E471FAFC2268349ACE372F58379989
  HKLM:\SOFTWARE\Classes\Installer\Products\46E471FAFC2268349ACE372F58379989
  HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Installer\UserData\S-1-5-18\Products\46E471FAFC2268349ACE372F58379989
  HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{AF174E64-22CF-4386-A9EC-73F285739998}
  HKLM:\SOFTWARE\VMware, Inc.
  C:\Program Files\VMware
  C:\Program Files\Common Files\VMware
  service: VGAuthService
  service: vm3dservice
  service: VMTools
  service: vmvss

Done. VMware Tools has been removed from Apps & Features, its services, and Program Files.
Note: Device Manager may still list hidden/ghost VMware devices (e.g. vmxnet3) -- this is a
known limitation of removing VMware Tools after a cross-hypervisor migration by any method,
not specific to this script. A reboot is recommended to complete the removal.
```

Also check `err-data`, if present. In testing this field was populated
with PowerShell's CLIXML-wrapped `Information`-stream serialization of
the same `Write-Host` output seen above (`#< CLIXML ...`) — this is **not**
an error; it is how PowerShell serializes `Write-Host` output when there
is no interactive host attached (as is always the case over `guest-exec`).
Confirm `exitcode: 0` and cross-reference the human-readable text already
decoded from `out-data` rather than treating a non-empty `err-data` as a
failure signal by itself.

If `exitcode` is non-zero, something genuinely failed — re-run Step 2's
VMware Tools lookup to see whether it still finds the product
registration (the script exits `0` immediately with "nothing to remove"
if it can't find VMware Tools at all, so a non-zero exit here means an
unexpected error partway through, not simply "already clean").

## Step 6: Confirm the result

Re-run the same checks from Step 2.3 and compare against the baseline.

Services — should now return an empty array:

```bash
PS_CMD='Get-Service -DisplayName "VMware*" -ErrorAction SilentlyContinue | Select-Object Name,Status | ConvertTo-Json -Compress'
```
Verified result after cleanup: no `out-data` field at all in the response
(empty result set).

Windows Installer registration — should also be empty:

```bash
PS_CMD='Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue | Where-Object {$_.DisplayName -like "*VMware*"} | Select-Object DisplayName | ConvertTo-Json -Compress'
```
Verified result after cleanup: no `out-data` field (empty result set) —
VMware Tools no longer appears in `Apps & Features`.

Program directories — should both be `false`:

```bash
PS_CMD='$r = @{}; $r.services = @(Get-Service -DisplayName "VMware*" -ErrorAction SilentlyContinue | Select-Object Name,Status); $r.dir1 = Test-Path "C:\Program Files\VMware"; $r.dir2 = Test-Path "C:\Program Files\Common Files\VMware"; $r | ConvertTo-Json -Compress'
```
Verified result after cleanup:
```json
{"dir1":false,"services":[],"dir2":false}
```

If all four checks (services empty, Installer registration empty, both
directories `false`) come back as above, cleanup is confirmed complete.

## Known limitation

As the script's own output notes, Device Manager may still list hidden
("ghost") VMware virtual devices (e.g. `vmxnet3`) even after a fully
successful cleanup by this or any other method — this is a general,
documented limitation of removing VMware Tools after a cross-hypervisor
migration, not something this script (or any uninstall approach) can
fully resolve short of a clean OS reinstall. If a completely clean Device
Manager is a hard requirement, a dedicated cleanup utility (e.g. Uwe
Sieber's DeviceCleanup) is the next step beyond what is covered here.

A reboot of the guest is recommended after running the cleanup script, to
let Windows finish settling the removed service/driver state.
