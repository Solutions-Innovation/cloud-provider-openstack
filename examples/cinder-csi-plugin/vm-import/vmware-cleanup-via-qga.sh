#!/usr/bin/env bash
#
# vmware-cleanup-via-qga.sh
#
# Post-boot VMware Tools cleanup for a Windows guest that was converted by
# this project's CONVERT_MODE=direct-v2v tooling (convert-and-import.sh +
# virt-v2v-in-place) and has since been booted on KVM/QEMU. Automates the
# full workflow documented in WINDOWS-POST-BOOT-CLEANUP.md:
#
#   1. Confirm the libvirt domain is running and QEMU Guest Agent (QGA) is
#      reachable.
#   2. Inspect the guest's current state (OS version, network, VMware
#      Tools/service status, Windows Installer registration) as a
#      "before" baseline.
#   3. Confirm the cleanup script (remove-vmware-tools.ps1) is actually
#      present in the guest's filesystem -- it is uploaded, but not run,
#      by convert-and-import.sh during conversion, and that upload can
#      fail without failing the overall conversion (see
#      OPERATIONS-GUIDE.md's troubleshooting table).
#   4. Run the cleanup script inside the guest via QGA, with -Force to
#      skip its interactive y/n prompt (guest-exec has no interactive
#      stdin channel for a human to answer it).
#   5. Collect and decode its output.
#   6. Re-run the same checks from step 2 to confirm VMware Tools, its
#      services, and its Program Files directories are actually gone.
#
# Run this ON THE HYPERVISOR HOST where the guest's libvirt domain is
# running (not over SSH to some other machine) -- it shells out to
# `sudo virsh` directly. sudo is assumed to be already configured to run
# virsh without an interactive password prompt in this context, per this
# script's intended use.
#
# WARNING: Step 4 is IRREVERSIBLE. It stops/deletes VMware services,
# deletes C:\Program Files\VMware and C:\Program Files\Common Files\VMware
# inside the guest, and removes the Windows Installer registry entries
# that make Apps & Features consider VMware Tools installed. There is no
# undo beyond re-converting the guest from its original source image.
# This script will refuse to proceed to Step 4 if any earlier step's
# checks did not pass (domain not running, agent unreachable, cleanup
# script missing) -- but once Step 4 starts, it runs to completion.
#
# Usage:
#   ./vmware-cleanup-via-qga.sh <domain-name> [--yes] [--skip-run]
#
#   <domain-name>   Required. The libvirt domain name (see `virsh list`).
#   --yes           Skip the interactic confirmation prompt before Step 4
#                    (the destructive step). Without this flag, the
#                    script pauses and asks for explicit "yes" input
#                    after showing the baseline state, before running
#                    the cleanup script inside the guest.
#   --skip-run       Run steps 1-3 and 6 only (connectivity, baseline
#                    inspection, script-presence check, then re-check
#                    the same state) without actually running the
#                    cleanup script in step 4 -- useful to audit a
#                    guest's current state without changing anything.
#
set -euo pipefail

GREEN="\033[0;32m"; RED="\033[0;31m"; YELLOW="\033[0;33m"; CYAN="\033[0;36m"; BOLD="\033[1m"; NC="\033[0m"
log_info()  { echo -e "${GREEN}INFO:${NC}  $1"; }
log_warn()  { echo -e "${YELLOW}WARN:${NC}  $1" >&2; }
log_error() { echo -e "${RED}ERROR:${NC} $1" >&2; }
log_step()  { echo -e "\n${BOLD}==> $1${NC}"; }

fail() {
  log_error "$1"
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "required command '$1' not found on this host. Install it before running this script."
}

require_cmd virsh
require_cmd iconv
require_cmd base64
require_cmd python3

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
DOM=""
AUTO_YES="false"
SKIP_RUN="false"

for arg in "$@"; do
  case "$arg" in
    --yes) AUTO_YES="true" ;;
    --skip-run) SKIP_RUN="true" ;;
    -h|--help)
      sed -n '2,45p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      if [ -z "$DOM" ]; then
        DOM="$arg"
      else
        fail "unexpected extra argument: $arg"
      fi
      ;;
  esac
done

[ -n "$DOM" ] || fail "usage: $0 <domain-name> [--yes] [--skip-run]  (run \`virsh list\` to find the domain name)"

# ---------------------------------------------------------------------------
# QGA helpers
# ---------------------------------------------------------------------------

# Encodes a PowerShell command string as UTF-16LE base64, suitable for
# powershell.exe -EncodedCommand. This avoids multiple layers of shell/JSON
# quoting breaking on anything beyond a trivial command.
ps_encode() {
  printf '%s' "$1" | iconv -f UTF-8 -t UTF-16LE | base64 -w0
}

# Submits a PowerShell command to the guest via guest-exec. Prints the pid
# on success, or fails the script if guest-exec itself could not be
# dispatched (e.g. agent unreachable).
qga_exec_ps() {
  local encoded="$1"
  local raw
  raw="$(sudo virsh qemu-agent-command "${DOM}" \
    "{\"execute\":\"guest-exec\",\"arguments\":{\"path\":\"C:\\\\Windows\\\\System32\\\\WindowsPowerShell\\\\v1.0\\\\powershell.exe\",\"arg\":[\"-NoLogo\",\"-NonInteractive\",\"-NoProfile\",\"-EncodedCommand\",\"${encoded}\"],\"capture-output\":true}}" 2>&1)" \
    || fail "guest-exec dispatch failed against domain '${DOM}': ${raw}"

  local pid
  pid="$(echo "${raw}" | grep -oP '"pid":\s*\K[0-9]+' || true)"
  [ -n "${pid}" ] || fail "guest-exec did not return a pid (raw response: ${raw})"
  echo "${pid}"
}

# Polls guest-exec-status for a pid until it reports exited, or until
# QGA_POLL_TIMEOUT_SECONDS elapses. Writes the final JSON response to the
# path given as $2. Exits non-zero (without failing the whole script) on
# timeout, so callers can decide how to handle a stuck command.
QGA_POLL_INTERVAL_SECONDS="${QGA_POLL_INTERVAL_SECONDS:-5}"
QGA_POLL_TIMEOUT_SECONDS="${QGA_POLL_TIMEOUT_SECONDS:-120}"

qga_wait_exec() {
  local pid="$1"
  local out_file="$2"
  local waited=0

  while true; do
    sudo virsh qemu-agent-command "${DOM}" \
      "{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":${pid}}}" \
      > "${out_file}" 2>&1 || true

    if grep -q '"exited":\s*true' "${out_file}"; then
      return 0
    fi

    if [ "${waited}" -ge "${QGA_POLL_TIMEOUT_SECONDS}" ]; then
      log_warn "guest-exec pid ${pid} did not report exited after ${QGA_POLL_TIMEOUT_SECONDS}s"
      return 1
    fi

    sleep "${QGA_POLL_INTERVAL_SECONDS}"
    waited=$((waited + QGA_POLL_INTERVAL_SECONDS))
  done
}

# Extracts exitcode/out-data/err-data from a saved guest-exec-status
# response and decodes the base64 streams. Prints a small summary to
# stdout; the decoded bodies are written to the two file paths given.
qga_decode_result() {
  local raw_file="$1"
  local out_file="$2"
  local err_file="$3"

  python3 - "${raw_file}" "${out_file}" "${err_file}" <<'PYEOF'
import json, re, base64, sys

raw_path, out_path, err_path = sys.argv[1:4]
content = open(raw_path, "r", errors="replace").read()
m = re.search(r'\{"return":.*\}', content)
if not m:
    print("exitcode: UNKNOWN (could not parse guest-exec-status response)")
    open(out_path, "w").close()
    open(err_path, "w").close()
    sys.exit(0)

data = json.loads(m.group(0))
ret = data.get("return", {})
print(f"exited: {ret.get('exited')}  exitcode: {ret.get('exitcode')}")

out_bytes = base64.b64decode(ret["out-data"]) if "out-data" in ret else b""
err_bytes = base64.b64decode(ret["err-data"]) if "err-data" in ret else b""

# PowerShell's own stdout/stderr over guest-exec is CRLF-terminated
# (Windows line endings). Normalize to LF so Bash string comparisons
# against the captured output (e.g. [ "$x" = "True" ]) work reliably --
# otherwise a trailing \r survives Bash's command-substitution trailing
# newline stripping and silently breaks exact-match comparisons.
out_bytes = out_bytes.replace(b"\r\n", b"\n")
err_bytes = err_bytes.replace(b"\r\n", b"\n")

with open(out_path, "wb") as f:
    f.write(out_bytes)
with open(err_path, "wb") as f:
    f.write(err_bytes)
PYEOF
}

# Runs a one-off PowerShell command end-to-end: encode, submit, poll,
# decode. Prints the decoded stdout to stdout. Fails the script if the
# command does not complete within the timeout, or if its own exit code
# is non-zero (callers that want to tolerate a non-zero exit should pass
# allow_nonzero=1 as the second argument).
run_ps() {
  local ps_cmd="$1"
  local allow_nonzero="${2:-0}"
  local encoded pid raw_file out_file err_file

  encoded="$(ps_encode "${ps_cmd}")"
  pid="$(qga_exec_ps "${encoded}")"

  raw_file="$(mktemp)"
  out_file="$(mktemp)"
  err_file="$(mktemp)"
  # shellcheck disable=SC2064
  trap "rm -f '${raw_file}' '${out_file}' '${err_file}'" RETURN

  if ! qga_wait_exec "${pid}" "${raw_file}"; then
    fail "command did not complete in time (pid ${pid}): ${ps_cmd}"
  fi

  local summary
  summary="$(qga_decode_result "${raw_file}" "${out_file}" "${err_file}")"
  local exitcode
  exitcode="$(echo "${summary}" | grep -oP 'exitcode:\s*\K[0-9A-Za-z]+' || echo "UNKNOWN")"

  if [ "${exitcode}" != "0" ] && [ "${allow_nonzero}" != "1" ]; then
    log_error "PowerShell command exited ${exitcode}: ${ps_cmd}"
    log_error "stdout: $(cat "${out_file}")"
    log_error "stderr: $(cat "${err_file}")"
    fail "aborting due to non-zero exit from a required check"
  fi

  cat "${out_file}"
}

# ---------------------------------------------------------------------------
# Step 1: domain running + QGA reachable
# ---------------------------------------------------------------------------
log_step "Step 1: Confirm domain '${DOM}' is running and QEMU Guest Agent is reachable"

STATE="$(sudo virsh domstate "${DOM}" 2>&1)" || fail "could not query domain '${DOM}' (is the name correct? try: virsh list --all)"
log_info "Domain state: ${STATE}"
[ "${STATE}" = "running" ] || fail "domain '${DOM}' is not running (state: ${STATE}) -- start it before running this script"

PING_RAW="$(sudo virsh qemu-agent-command "${DOM}" '{"execute":"guest-ping"}' 2>&1)" \
  || fail "guest-ping failed against domain '${DOM}': ${PING_RAW}. QEMU Guest Agent may not be running inside the guest."
log_info "guest-ping response: ${PING_RAW}"

# ---------------------------------------------------------------------------
# Step 2: baseline inspection
# ---------------------------------------------------------------------------
log_step "Step 2: Inspect guest state (OS version, network, VMware Tools/services)"

log_info "OS version:"
OSINFO_RAW="$(sudo virsh qemu-agent-command "${DOM}" '{"execute":"guest-get-osinfo"}' 2>&1)" || fail "guest-get-osinfo failed: ${OSINFO_RAW}"
echo "  ${OSINFO_RAW}"

log_info "Network interfaces:"
NETINFO_RAW="$(sudo virsh qemu-agent-command "${DOM}" '{"execute":"guest-network-get-interfaces"}' 2>&1)" || log_warn "guest-network-get-interfaces failed: ${NETINFO_RAW}"
echo "  ${NETINFO_RAW}"

log_info "VMware service status (before):"
SERVICES_BEFORE="$(run_ps 'Get-Service | Where-Object {$_.DisplayName -like "VMware*"} | Select-Object Name,Status | ConvertTo-Json -Compress')"
echo "  ${SERVICES_BEFORE:-<none found>}"

log_info "Windows Installer registration for VMware Tools (before):"
INSTALLER_BEFORE="$(run_ps 'Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue | Where-Object {$_.DisplayName -like "*VMware*"} | Select-Object DisplayName | ConvertTo-Json -Compress')"
echo "  ${INSTALLER_BEFORE:-<none found -- VMware Tools not registered, nothing to clean up>}"

if [ -z "${INSTALLER_BEFORE}" ]; then
  log_warn "VMware Tools is not currently registered in Apps & Features. The cleanup script (if run) will detect this and exit 0 with 'nothing to remove' -- this is not an error, but there may be nothing left to do here."
fi

# ---------------------------------------------------------------------------
# Step 3: confirm the cleanup script is present
# ---------------------------------------------------------------------------
log_step "Step 3: Confirm remove-vmware-tools.ps1 is present in the guest"

SCRIPT_PRESENT="$(run_ps 'Test-Path "C:\Program Files\Guestfs\remove-vmware-tools.ps1"')"
log_info "Test-Path result: ${SCRIPT_PRESENT}"

if [ "${SCRIPT_PRESENT}" != "True" ]; then
  fail "remove-vmware-tools.ps1 was not found at C:\\Program Files\\Guestfs\\ inside the guest. This is normally uploaded (not executed) by convert-and-import.sh during conversion -- the upload can fail without failing the overall conversion (common cause: the guest's NTFS partition had a 'hibernated' flag set at conversion time; see OPERATIONS-GUIDE.md). Copy the script into the guest by some other means (console/RDP, or an offline guestfish upload) before re-running this script."
fi

if [ "${SKIP_RUN}" = "true" ]; then
  log_info "--skip-run was passed; skipping Step 4 (actually running the cleanup script) and Step 5. Re-running Step 2's checks as Step 6 for a consistency check against the same baseline just printed above."
  log_step "Step 6: Re-check state (--skip-run mode: expect this to match the 'before' baseline above)"
  SERVICES_AFTER="$(run_ps 'Get-Service | Where-Object {$_.DisplayName -like "VMware*"} | Select-Object Name,Status | ConvertTo-Json -Compress')"
  echo "  Services: ${SERVICES_AFTER:-<none found>}"
  exit 0
fi

# ---------------------------------------------------------------------------
# Confirmation gate before the destructive step
# ---------------------------------------------------------------------------
if [ "${AUTO_YES}" != "true" ]; then
  echo ""
  echo -e "${YELLOW}${BOLD}About to run remove-vmware-tools.ps1 inside domain '${DOM}'.${NC}"
  echo -e "${YELLOW}This will stop/delete VMware services, delete C:\\Program Files\\VMware${NC}"
  echo -e "${YELLOW}and C:\\Program Files\\Common Files\\VMware, and remove the Windows Installer${NC}"
  echo -e "${YELLOW}registry entries for VMware Tools. This is IRREVERSIBLE.${NC}"
  echo ""
  read -r -p "Type 'yes' to continue: " CONFIRM
  [ "${CONFIRM}" = "yes" ] || fail "aborted by user (did not type 'yes')"
fi

# ---------------------------------------------------------------------------
# Step 4: run the cleanup script
# ---------------------------------------------------------------------------
log_step "Step 4: Running remove-vmware-tools.ps1 -Force inside the guest (this can take close to a minute)"

RUN_ENC="$(ps_encode '& "C:\Program Files\Guestfs\remove-vmware-tools.ps1" -Force')"
RUN_PID="$(qga_exec_ps "${RUN_ENC}")"
log_info "Submitted as guest-exec pid ${RUN_PID}; polling every ${QGA_POLL_INTERVAL_SECONDS}s (timeout ${QGA_POLL_TIMEOUT_SECONDS}s)..."

RUN_RAW_FILE="$(mktemp)"
RUN_OUT_FILE="$(mktemp)"
RUN_ERR_FILE="$(mktemp)"
trap 'rm -f "${RUN_RAW_FILE}" "${RUN_OUT_FILE}" "${RUN_ERR_FILE}"' EXIT

if ! qga_wait_exec "${RUN_PID}" "${RUN_RAW_FILE}"; then
  fail "remove-vmware-tools.ps1 did not report completion within ${QGA_POLL_TIMEOUT_SECONDS}s (pid ${RUN_PID}). It may still be running inside the guest -- check manually with: virsh qemu-agent-command ${DOM} '{\"execute\":\"guest-exec-status\",\"arguments\":{\"pid\":${RUN_PID}}}'"
fi

# ---------------------------------------------------------------------------
# Step 5: collect and show the result
# ---------------------------------------------------------------------------
log_step "Step 5: Cleanup script output"

RUN_SUMMARY="$(qga_decode_result "${RUN_RAW_FILE}" "${RUN_OUT_FILE}" "${RUN_ERR_FILE}")"
log_info "${RUN_SUMMARY}"
echo ""
echo "--- stdout from remove-vmware-tools.ps1 ---"
cat "${RUN_OUT_FILE}"
echo "--------------------------------------------"

RUN_EXITCODE="$(echo "${RUN_SUMMARY}" | grep -oP 'exitcode:\s*\K[0-9A-Za-z]+' || echo "UNKNOWN")"
if [ "${RUN_EXITCODE}" != "0" ]; then
  log_error "remove-vmware-tools.ps1 exited with code ${RUN_EXITCODE} (expected 0)."
  log_error "Raw stderr (PowerShell wraps Write-Host output as CLIXML over guest-exec; a non-empty value here is not necessarily an error by itself -- inspect it alongside the exit code and stdout above):"
  cat "${RUN_ERR_FILE}" >&2
  fail "cleanup script did not report success -- investigate before assuming the guest is in a known state"
fi

# ---------------------------------------------------------------------------
# Step 6: confirm the result
# ---------------------------------------------------------------------------
log_step "Step 6: Confirm VMware Tools, its services, and its directories are gone"

SERVICES_AFTER="$(run_ps 'Get-Service -DisplayName "VMware*" -ErrorAction SilentlyContinue | Select-Object Name,Status | ConvertTo-Json -Compress')"
INSTALLER_AFTER="$(run_ps 'Get-ItemProperty "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\*" -ErrorAction SilentlyContinue | Where-Object {$_.DisplayName -like "*VMware*"} | Select-Object DisplayName | ConvertTo-Json -Compress')"
DIRS_AFTER="$(run_ps '$r = @{}; $r.dir1 = Test-Path "C:\Program Files\VMware"; $r.dir2 = Test-Path "C:\Program Files\Common Files\VMware"; $r | ConvertTo-Json -Compress')"

log_info "Services (expect empty):          ${SERVICES_AFTER:-<empty, as expected>}"
log_info "Installer registration (expect empty): ${INSTALLER_AFTER:-<empty, as expected>}"
log_info "Program Files directories:        ${DIRS_AFTER}"

PASS="true"
if [ -n "${SERVICES_AFTER}" ]; then
  log_warn "VMware services still present after cleanup -- expected an empty result"
  PASS="false"
fi
if [ -n "${INSTALLER_AFTER}" ]; then
  log_warn "VMware Tools is still registered in Apps & Features after cleanup -- expected an empty result"
  PASS="false"
fi
if echo "${DIRS_AFTER}" | grep -q '"dir1":true\|"dir2":true'; then
  log_warn "One or both VMware Program Files directories still exist after cleanup"
  PASS="false"
fi

echo ""
if [ "${PASS}" = "true" ]; then
  log_info "Cleanup confirmed complete: no VMware services, no Apps & Features registration, no Program Files directories."
  log_info "A reboot of the guest is recommended to let Windows finish settling the removed service/driver state."
  log_info "Note: Device Manager may still list hidden/ghost VMware devices (e.g. vmxnet3) -- this is a known limitation of removing VMware Tools after a cross-hypervisor migration by any method, not specific to this script."
else
  log_error "One or more post-cleanup checks did not pass -- see warnings above. Do not assume the guest is in a fully clean state."
  exit 1
fi
