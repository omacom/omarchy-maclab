#!/bin/bash
# Install lab-macos-runner on a lab Mac, in macOS: sudo ./install.sh
#
# Checks the runner and its LaunchDaemon against their signatures by the
# release key pinned below (ssh-keygen -Y verify) before anything else, then
# puts the runner in /usr/local/lab/bin and its LaunchDaemon in
# /Library/LaunchDaemons, which runs it at every boot. Then enroll it once
# with the line `lab macos enroll <mac>` or "Set up macOS" on My Macs gives,
# and run `sudo lab-macos-runner setup`. ./install.sh --check only checks the
# signatures; ./install.sh --uninstall removes the runner, its LaunchDaemon
# and its enrolment.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
label=org.omarchy.maclab.macos-runner
plist=/Library/LaunchDaemons/$label.plist
signer="release@omarchy-m-testing.org"
namespace="omarchy-m-test-release"
# The release signing public key, the same one every file of a Lab release is checked against.
allowed_signer='release@omarchy-m-testing.org namespaces="omarchy-m-test-release" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIKHA9ywzf4hxAd5Ohzw8tRg5i2lxHAxp2mDV0PWi5o0u'

if [[ ${1:-} == --uninstall ]]; then
  [[ $(uname -s) == Darwin ]] || { echo "install.sh: for macOS" >&2; exit 1; }
  [[ $EUID -eq 0 ]] || { echo "install.sh: run with sudo" >&2; exit 1; }
  launchctl bootout system "$plist" 2>/dev/null || true
  rm -f "$plist" /usr/local/lab/bin/lab-macos-runner
  rm -rf "/Library/Application Support/Omarchy Mac Lab"
  echo "lab-macos-runner removed"
  exit 0
fi

command -v ssh-keygen >/dev/null 2>&1 || { echo "install.sh: needs ssh-keygen to check the signatures: nothing was installed" >&2; exit 1; }
signers="$(mktemp)"
trap 'rm -f "$signers"' EXIT
printf '%s\n' "$allowed_signer" > "$signers"
for f in bin/lab-macos-runner "$label.plist"; do
  if [[ ! -f $here/$f || ! -f $here/$f.sig ]] ||
    ! ssh-keygen -Y verify -f "$signers" -I "$signer" -n "$namespace" -s "$here/$f.sig" < "$here/$f" >/dev/null 2>&1; then
    echo "install.sh: $f is not signed by the release key of Omarchy Mac Lab: nothing was installed" >&2
    exit 1
  fi
done
if [[ ${1:-} == --check ]]; then
  echo "the runner and its LaunchDaemon are signed by the release key"
  exit 0
fi

[[ $(uname -s) == Darwin ]] || { echo "install.sh: for macOS" >&2; exit 1; }
[[ $EUID -eq 0 ]] || { echo "install.sh: run with sudo" >&2; exit 1; }
install -d -m 0755 /usr/local/lab/bin
install -m 0755 "$here/bin/lab-macos-runner" /usr/local/lab/bin/lab-macos-runner
launchctl bootout system "$plist" 2>/dev/null || true
install -m 0644 -o root -g wheel "$here/$label.plist" "$plist"
launchctl bootstrap system "$plist"
echo "lab-macos-runner installed. Now run the line the lab gave (lab macos enroll <mac>, or Set up macOS on My Macs), then: sudo lab-macos-runner setup"
