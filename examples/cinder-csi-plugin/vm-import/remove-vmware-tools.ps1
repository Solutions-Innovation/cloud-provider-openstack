<#
.SYNOPSIS
    Force-removes VMware Tools from a Windows guest that has been migrated
    off a VMware hypervisor (e.g. converted to run on KVM via this
    project's virt-v2v-in-place-based tooling).

.DESCRIPTION
    VMware Tools' own MSI uninstaller cannot complete once the guest is no
    longer running on a VMware hypervisor: one of its first actions is a
    hardware/hypervisor validation check (seen as VM_LogStart or
    VM_CheckRequirements depending on the VMware Tools version) that
    deliberately fails with MSI error 1603 outside VMware. This is a known,
    documented limitation of VMware Tools itself, not something fixable by
    retrying the standard uninstall or passing different msiexec options --
    see Broadcom/VMware KB 414772 and Red Hat KB 7080999 for independent
    confirmation of the same failure in other migration tooling.

    This script sidesteps that broken uninstaller entirely. Instead of
    calling msiexec against the VMware Tools MSI, it:
      1. Looks up the VMware Tools product registration directly in the
         Windows Installer registry data (no assumption about which
         VMware Tools version is installed, or which GUID it uses).
      2. Removes the registry entries that make Windows consider VMware
         Tools "installed" (Installer\Products, Installer\Features, the
         Uninstall key used by Apps & Features, and the UserData product
         key).
      3. Stops and deletes the known VMware services.
      4. Removes the VMware Tools program directories.

    This is a direct adaptation of a widely used community script
    (https://gist.github.com/broestls/f872872a00acee2fca02017160840624,
    143 stars at the time of writing) for post-migration VMware Tools
    removal, adjusted to run non-interactively from this project's
    conversion tooling context.

    KNOWN LIMITATION (documented upstream in the same community script's
    discussion thread): this removes VMware Tools from Windows' own
    bookkeeping (Apps & Features, services, Program Files) but does not
    necessarily remove every trace from Device Manager -- hidden/ghost
    VMware virtual devices (e.g. vmxnet3) can remain listed even after a
    successful uninstall via any method, standard or forced. If you need a
    fully clean Device Manager, a dedicated cleanup tool (e.g. Uwe Sieber's
    DeviceCleanup) or a fresh OS install is the only way to guarantee that;
    this script does not attempt it.

.NOTES
    This script is placed into the guest's filesystem (not executed) by
    convert-and-import.sh during CONVERT_MODE=direct-v2v conversion, for
    the guest's administrator to run manually after the guest has actually
    booted on its new (KVM) hardware -- see OPERATIONS-GUIDE.md. It is not
    run automatically as part of conversion or as a firstboot script,
    because removing files/registry/services is an irreversible guest-side
    change that should happen under an administrator's direct control,
    with the guest already up and reachable to verify the result.
#>

[CmdletBinding()]
param(
    # Skip the interactive y/n confirmation prompt (e.g. for scripted use).
    [switch]$Force
)

function Get-VMwareToolsInstallerID {
    foreach ($item in $(Get-ChildItem Registry::HKEY_CLASSES_ROOT\Installer\Products -ErrorAction SilentlyContinue)) {
        if ($item.GetValue('ProductName') -eq 'VMware Tools') {
            return @{
                reg_id = $item.PSChildName
                msi_id = [Regex]::Match($item.GetValue('ProductIcon'), '(?<={)(.*?)(?=})') | Select-Object -ExpandProperty Value
            }
        }
    }
}

Write-Host "Looking up VMware Tools product registration..."
$vmware_tools_ids = Get-VMwareToolsInstallerID

if (-not $vmware_tools_ids) {
    Write-Host "VMware Tools product registration not found -- nothing to remove (it may already be uninstalled, or was never present under this name)."
    exit 0
}

Write-Host "Found VMware Tools: reg_id=$($vmware_tools_ids.reg_id) msi_id=$($vmware_tools_ids.msi_id)"

# Registry locations keyed by the Windows-Installer-internal compressed
# GUID (reg_id). Covers both the classic HKCR location and its HKLM mirror
# (Windows maintains both; removing only one can leave the other pointing
# at a now-broken product reference).
$reg_targets = @(
    "Registry::HKEY_CLASSES_ROOT\Installer\Features\",
    "Registry::HKEY_CLASSES_ROOT\Installer\Products\",
    "HKLM:\SOFTWARE\Classes\Installer\Features\",
    "HKLM:\SOFTWARE\Classes\Installer\Products\",
    "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Installer\UserData\S-1-5-18\Products\"
)

$VMware_Tools_Directory = "C:\Program Files\VMware"
$VMware_Common_Directory = "C:\Program Files\Common Files\VMware"

$targets = @()
foreach ($item in $reg_targets) {
    $targets += $item + $vmware_tools_ids.reg_id
}
# The key Apps & Features / Programs and Features actually reads from,
# keyed by the real Product Code GUID (msi_id), not the compressed reg_id.
$targets += "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\{$($vmware_tools_ids.msi_id)}"

if (Test-Path "HKLM:\SOFTWARE\VMware, Inc.") {
    $targets += "HKLM:\SOFTWARE\VMware, Inc."
}
if (Test-Path $VMware_Tools_Directory) {
    $targets += $VMware_Tools_Directory
}
if (Test-Path $VMware_Common_Directory) {
    $targets += $VMware_Common_Directory
}

$services = Get-Service -DisplayName "VMware*" -ErrorAction SilentlyContinue
$services += Get-Service -DisplayName "GISvc" -ErrorAction SilentlyContinue

Write-Host ""
Write-Host "The following registry keys, filesystem folders, and services will be removed:"
if (-not $targets -and -not $services) {
    Write-Host "Nothing to do!"
    exit 0
}
$targets | ForEach-Object { Write-Host "  $_" }
$services | ForEach-Object { Write-Host "  service: $($_.Name)" }
Write-Host ""

if (-not $Force) {
    $user_confirmed = Read-Host "Continue (y/n)"
    if ($user_confirmed -ne "y") {
        Write-Host "Aborted -- no changes made."
        exit 1
    }
}

if ($services) {
    $services | Stop-Service -Confirm:$false -ErrorAction SilentlyContinue
    if (Get-Command Remove-Service -ErrorAction SilentlyContinue) {
        $services | Remove-Service -Confirm:$false -ErrorAction SilentlyContinue
    } else {
        foreach ($s in $services) {
            sc.exe DELETE $($s.Name) | Out-Null
        }
    }
}

foreach ($item in $targets) {
    if (Test-Path $item) {
        Remove-Item -Path $item -Recurse -Force -ErrorAction SilentlyContinue
    }
}

Write-Host ""
Write-Host "Done. VMware Tools has been removed from Apps & Features, its services, and Program Files."
Write-Host "Note: Device Manager may still list hidden/ghost VMware devices (e.g. vmxnet3) -- this is a"
Write-Host "known limitation of removing VMware Tools after a cross-hypervisor migration by any method,"
Write-Host "not specific to this script. A reboot is recommended to complete the removal."
