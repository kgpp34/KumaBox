#!/usr/bin/env bash
# doctor/check_test.sh — tests for doctor/check.sh.
#
# Runs the checker against a throwaway root and a PATH full of fake tools, so it
# needs no root, no KVM and no Cloud Hypervisor. Every external command the
# checker calls is replaced, which also means the checks themselves are asserted
# rather than merely exercised.

set -uo pipefail

repo_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
checker="${repo_dir}/doctor/check.sh"

ESC=$(printf '\033')
strip_ansi() { sed "s/${ESC}\[[0-9;]*m//g"; }

PASSED=0
FAILED=0

ok()      { PASSED=$((PASSED + 1)); printf "  \033[32mok\033[0m   %s\n" "$1"; }
not_ok()  { FAILED=$((FAILED + 1)); printf "  \033[31mFAIL\033[0m %s\n" "$1"; }

assert_contains() {
    local haystack="$1" needle="$2" what="$3"
    if grep -qF -- "$needle" <<<"$haystack"; then
        ok "$what"
    else
        not_ok "$what (missing: $needle)"
        printf '%s\n' "$haystack" | sed 's/^/       | /'
    fi
}

assert_not_contains() {
    local haystack="$1" needle="$2" what="$3"
    if grep -qF -- "$needle" <<<"$haystack"; then
        not_ok "$what (unexpected: $needle)"
        printf '%s\n' "$haystack" | sed 's/^/       | /'
    else
        ok "$what"
    fi
}

assert_dir() {
    if [ -d "$1" ]; then ok "created $1"; else not_ok "$1 was not created"; fi
}

assert_missing() {
    if [ -e "$1" ]; then not_ok "$1 should not exist"; else ok "$1 not created"; fi
}

sandbox=$(mktemp -d)
trap 'rm -rf "$sandbox"' EXIT

root="${sandbox}/root"
fake_bin="${sandbox}/bin"
cni_conf="${sandbox}/cni-conf"
cni_bin="${sandbox}/cni-bin"
netns_dir="${sandbox}/netns"
kvm_device="${sandbox}/kvm"
mkdir -p "$fake_bin" "$cni_bin" "$netns_dir"

# --- fake tools -------------------------------------------------------------
cat > "${fake_bin}/cloud-hypervisor" <<'EOF'
#!/usr/bin/env bash
echo "Cloud Hypervisor v${FAKE_CH_VERSION:-53.0}"
EOF

cat > "${fake_bin}/mkfs.erofs" <<'EOF'
#!/usr/bin/env bash
echo "mkfs.erofs ${FAKE_EROFS_VERSION:-1.8.1}"
EOF

cat > "${fake_bin}/mkfs.ext4" <<'EOF'
#!/usr/bin/env bash
[ "${1:-}" = "-V" ] && echo "mke2fs 1.47.0"
EOF

# The checker only asks whether a rule exists (or adds it); both succeed here.
cat > "${fake_bin}/iptables" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

# Report the expected values for the two keys the checker inspects.
cat > "${fake_bin}/sysctl" <<'EOF'
#!/usr/bin/env bash
case "${1:-}" in
    -n) case "${2:-}" in
            net.ipv4.ip_forward|net.bridge.bridge-nf-call-iptables) echo 1 ;;
            *) echo "" ;;
        esac ;;
    -w) exit 0 ;;
esac
exit 0
EOF

cat > "${fake_bin}/ip" <<'EOF'
#!/usr/bin/env bash
case "$*" in
    "route show default") echo "default via 10.0.0.1 dev eth0" ;;
    "link show eth0")     echo "2: eth0: <UP> mtu 1460 state UP" ;;
esac
exit 0
EOF

chmod 0755 "${fake_bin}"/*

for plugin in bridge host-local loopback; do
    printf '#!/usr/bin/env bash\n' > "${cni_bin}/${plugin}"
    chmod 0755 "${cni_bin}/${plugin}"
done
printf '{}\n' > "${cni_conf}/10-kumabox.conflist"
printf 'device\n' > "$kvm_device"
chmod 0666 "$kvm_device"

# run_checker keeps both the output and the status of one invocation in
# CHECK_OUTPUT and CHECK_STATUS.
CHECK_OUTPUT=""
CHECK_STATUS=0
run_checker() {
    CHECK_OUTPUT=$(env -i \
        PATH="${fake_bin}:/usr/bin:/bin" \
        HOME="$sandbox" \
        KUMABOX_ROOT_DIR="$root" \
        KUMABOX_RUN_DIR="${sandbox}/run" \
        KUMABOX_LOG_DIR="${sandbox}/log" \
        KUMABOX_META_DIR="${root}/metadata" \
        KUMABOX_BLOBS_DIR="${root}/blobs" \
        KUMABOX_TMP_DIR="${root}/tmp" \
        KUMABOX_CNI_CONF_DIR="$cni_conf" \
        KUMABOX_CNI_BIN_DIR="$cni_bin" \
        KUMABOX_NETNS_DIR="$netns_dir" \
        KUMABOX_KVM_DEVICE="$kvm_device" \
        FAKE_CH_VERSION="${FAKE_CH_VERSION:-53.0}" \
        FAKE_EROFS_VERSION="${FAKE_EROFS_VERSION:-1.8.1}" \
        bash "$checker" "$@" 2>&1)
    CHECK_STATUS=$?
    CHECK_OUTPUT=$(printf '%s' "$CHECK_OUTPUT" | strip_ansi)
}

printf '\n\033[1mdoctor/check.sh\033[0m\n'

# --- 1. help ----------------------------------------------------------------
run_checker --help
if [ "$CHECK_STATUS" -eq 0 ]; then ok "help exits 0"; else not_ok "help exit code = $CHECK_STATUS"; fi
assert_contains "$CHECK_OUTPUT" "Usage:" "help prints usage"
assert_contains "$CHECK_OUTPUT" "KUMABOX_KVM_DEVICE" "help documents the KVM override"

# --- 2. a fresh root fails and changes nothing ------------------------------
run_checker
if [ "$CHECK_STATUS" -eq 1 ]; then ok "a fresh root fails with exit 1"; else not_ok "exit code = $CHECK_STATUS, want 1"; fi
assert_contains "$CHECK_OUTPUT" "[FAIL] ${root} does not exist" "reports the missing root"
assert_missing "$root"

# --- 3. --fix creates the managed directories -------------------------------
# --fix repairs what it finds, but the failures it found still count: the
# summary reports them and the exit code stays 1 until a clean re-run.
run_checker --fix
if [ "$CHECK_STATUS" -eq 1 ]; then ok "--fix reports the failures it repaired"; else
    not_ok "--fix exit code = $CHECK_STATUS, want 1 (failures were found)"
    printf '%s\n' "$CHECK_OUTPUT" | sed 's/^/       | /'
fi
assert_contains "$CHECK_OUTPUT" "[FIXED] created ${root}/blobs" "reports what it created"
for dir in "$root" "${root}/metadata" "${root}/blobs" "${root}/tmp" \
           "${root}/firmware" "${sandbox}/run" "${sandbox}/log" "$netns_dir"; do
    assert_dir "$dir"
done

# --- 4. the second run passes the checks it just fixed ----------------------
run_checker
if [ "$CHECK_STATUS" -eq 0 ]; then ok "a repaired root passes"; else
    not_ok "exit code = $CHECK_STATUS after --fix"
    printf '%s\n' "$CHECK_OUTPUT" | sed 's/^/       | /'
fi
assert_contains "$CHECK_OUTPUT" "[PASS] ${root}/blobs" "root directories are reported as present"
assert_contains "$CHECK_OUTPUT" "[PASS] cloud-hypervisor (Cloud Hypervisor v53.0)" "reads the Cloud Hypervisor version"
assert_contains "$CHECK_OUTPUT" "[PASS] mkfs.erofs (mkfs.erofs 1.8.1)" "reads the erofs-utils version"
assert_contains "$CHECK_OUTPUT" "[PASS] ${kvm_device} accessible" "reports the KVM device as usable"
assert_contains "$CHECK_OUTPUT" "[PASS] bridge" "finds the CNI plugins"
assert_contains "$CHECK_OUTPUT" "Pass:" "prints a summary"

# --- 5. the erofs floor is enforced ----------------------------------------
FAKE_EROFS_VERSION=1.7.4 run_checker
if [ "$CHECK_STATUS" -eq 1 ]; then ok "an old mkfs.erofs fails the check"; else not_ok "exit code = $CHECK_STATUS, want 1"; fi
assert_contains "$CHECK_OUTPUT" "[FAIL] mkfs.erofs (mkfs.erofs 1.7.4) is older than 1.8" "explains the erofs floor"

# --- 6. the Cloud Hypervisor floor is enforced ------------------------------
FAKE_CH_VERSION=42.0 run_checker
if [ "$CHECK_STATUS" -eq 1 ]; then ok "an old cloud-hypervisor fails the check"; else not_ok "exit code = $CHECK_STATUS, want 1"; fi
assert_contains "$CHECK_OUTPUT" "is older than v43" "explains the Cloud Hypervisor floor"

# --- 7. stray staging directories are reported ------------------------------
mkdir -p "${root}/tmp/import-1234"
run_checker
assert_contains "$CHECK_OUTPUT" "staging entr" "warns about staging leftovers"
assert_contains "$CHECK_OUTPUT" "kumabox gc" "points at the command that reclaims them"

printf "\n  %d passed, %d failed\n\n" "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
