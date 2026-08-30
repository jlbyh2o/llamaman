#!/bin/sh
# installer/tests/run.sh — the mechanical test suite for installer/install.sh.
#
# Plain POSIX sh, no bats, no new dependency (DESIGN section 14 names every
# third-party package this project may use and a test framework is not among
# them). Run it directly:
#
#   sh installer/tests/run.sh            # everything
#   sh installer/tests/run.sh uninstall  # only tests whose name matches
#
# WHAT IT DOES NOT DO: touch the real host. Every test runs install.sh against a
# fresh fake root with --root, with systemctl, loginctl, runuser, curl, useradd,
# id, getent, chown, install, date and sleep replaced by stubs that come first on
# $PATH and only RECORD what they were asked to do. No systemd call, no network
# request and no write outside the sandbox happens at any point.
#
# What is exercised for real, and deliberately so: argument parsing, sha256
# verification (the host's own sha256sum against a genuine tarball), tar
# extraction, directory creation, the arguments handed to `llamaman
# install-units`, the upgrade-versus-first-install branch, and the whole
# uninstall path including both consent gates.
#
# Three file-wide shellcheck exemptions, each for a false positive:
#   SC2329 — every assertion and every t_* case IS invoked, by name, from the
#            table in main(). Dynamic dispatch is the whole design of a test
#            table and shellcheck cannot see through it.
#   SC2089/SC2090 — the fixture JSON contains double quotes. It is exported to a
#            stub as one environment variable and printed verbatim; it is never
#            re-parsed as shell, so there is nothing for the quotes to do.
# shellcheck disable=SC2329,SC2089,SC2090

set -eu

LM_TEST_SELF=$(cd "$(dirname "$0")" && pwd)
LM_TEST_SCRIPT=$(cd "$LM_TEST_SELF/.." && pwd)/install.sh
[ -f "$LM_TEST_SCRIPT" ] || {
	printf 'cannot find install.sh beside %s\n' "$LM_TEST_SELF" >&2
	exit 1
}

LM_TEST_FILTER=${1:-}
LM_TEST_PASS=0
LM_TEST_FAIL=0
LM_TEST_NAME=''
LM_TEST_FAILED_NAMES=''

SB=''
OUT=''
STATUS=0
ARCH=''

case $(uname -m) in
x86_64 | amd64) ARCH=amd64 ;;
aarch64 | arm64) ARCH=arm64 ;;
*)
	printf 'unsupported test host architecture %s\n' "$(uname -m)" >&2
	exit 1
	;;
esac

VERSION=v1.4.2
ASSET="llamaman_${VERSION}_linux_${ARCH}.tar.gz"
OTHER_ARCH=arm64
[ "$ARCH" = amd64 ] || OTHER_ARCH=amd64
OTHER_ASSET="llamaman_${VERSION}_linux_${OTHER_ARCH}.tar.gz"

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

fail() {
	LM_TEST_FAIL=$((LM_TEST_FAIL + 1))
	LM_TEST_FAILED_NAMES="$LM_TEST_FAILED_NAMES
  $LM_TEST_NAME: $*"
	printf 'not ok - %s\n' "$LM_TEST_NAME"
	printf '  # %s\n' "$*"
	if [ -n "$OUT" ] && [ -f "$OUT" ]; then
		sed 's/^/  # out: /' "$OUT"
	fi
}

assert_status() {
	if [ "$STATUS" -ne "$1" ]; then
		fail "exit status $STATUS, want $1"
		return 1
	fi
}

assert_status_nonzero() {
	if [ "$STATUS" -eq 0 ]; then
		fail 'exit status 0, want non-zero'
		return 1
	fi
}

assert_out() {
	if ! grep -qF -- "$1" "$OUT"; then
		fail "output does not contain: $1"
		return 1
	fi
}

assert_not_out() {
	if grep -qF -- "$1" "$OUT"; then
		fail "output unexpectedly contains: $1"
		return 1
	fi
}

assert_file() {
	if [ ! -f "$SB/root$1" ]; then
		fail "expected file: $1"
		return 1
	fi
}

assert_no_file() {
	if [ -e "$SB/root$1" ]; then
		fail "unexpected file: $1"
		return 1
	fi
}

assert_dir() {
	if [ ! -d "$SB/root$1" ]; then
		fail "expected directory: $1"
		return 1
	fi
}

assert_no_dir() {
	if [ -d "$SB/root$1" ]; then
		fail "unexpected directory: $1"
		return 1
	fi
}

# assert_log asserts one recorded stub invocation. The log holds one line per
# call, verbatim, so these read as "this command was run with these arguments".
assert_log() {
	if [ ! -f "$SB/log/$1" ] || ! grep -qF -- "$2" "$SB/log/$1"; then
		fail "$1 was not called with: $2"
		if [ -f "$SB/log/$1" ]; then sed 's/^/  # log: /' "$SB/log/$1"; fi
		return 1
	fi
}

assert_no_log() {
	if [ -f "$SB/log/$1" ] && grep -qF -- "$2" "$SB/log/$1"; then
		fail "$1 was unexpectedly called with: $2"
		sed 's/^/  # log: /' "$SB/log/$1"
		return 1
	fi
}

assert_log_absent() {
	if [ -s "$SB/log/$1" ]; then
		fail "$1 was called, and should not have been"
		sed 's/^/  # log: /' "$SB/log/$1"
		return 1
	fi
}

# ---------------------------------------------------------------------------
# The sandbox: a fake root, a stub $PATH and a fake release
# ---------------------------------------------------------------------------

sandbox_new() {
	SB=$(mktemp -d)
	OUT="$SB/out"
	STATUS=0

	mkdir -p \
		"$SB/root/run/systemd/system" \
		"$SB/root/etc/systemd/system" \
		"$SB/root/etc/systemd/user" \
		"$SB/root/etc/polkit-1/rules.d" \
		"$SB/root/usr/local/bin" \
		"$SB/root/var/lib" \
		"$SB/root/home/alice" \
		"$SB/bin" "$SB/log" "$SB/art" "$SB/pkg" "$SB/tmp"

	# The fake /etc/passwd the id, getent and useradd stubs all read, so account
	# creation is genuinely observable rather than asserted from a flag.
	cat >"$SB/root/etc/passwd" <<'EOF'
root:x:0:0:root:/root:/bin/sh
alice:x:1000:1000::/home/alice:/bin/bash
EOF

	write_stubs
	write_release
}

sandbox_free() {
	[ -z "$SB" ] || rm -rf "$SB"
	SB=''
	OUT=''
}

write_stubs() {
	# id — answers from the fake passwd file. `id -u` with no operand is the
	# euid the script sees, which is how the non-root refusal is exercised.
	cat >"$SB/bin/id" <<'EOF'
#!/bin/sh
mode=${1:-}
who=${2:-}
if [ -z "$who" ]; then
	case $mode in
	-u) printf '%s\n' "${LM_TEST_EUID:-0}"; exit 0 ;;
	-g) printf '0\n'; exit 0 ;;
	esac
fi
line=$(awk -F: -v u="$who" '$1 == u {print; exit}' "$LM_TEST_ROOT/etc/passwd")
[ -n "$line" ] || exit 1
case $mode in
-u) printf '%s\n' "$(printf '%s' "$line" | cut -d: -f3)" ;;
-g) printf '%s\n' "$(printf '%s' "$line" | cut -d: -f4)" ;;
-gn) printf '%s\n' "$who" ;;
*) printf '%s\n' "$line" ;;
esac
EOF

	cat >"$SB/bin/getent" <<'EOF'
#!/bin/sh
[ "${1:-}" = passwd ] || exit 2
awk -F: -v u="${2:-}" '$1 == u {print; found=1} END {exit found ? 0 : 2}' "$LM_TEST_ROOT/etc/passwd"
EOF

	# useradd — records, and actually appends to the fake passwd file, so the
	# idempotent re-run has something real to find.
	cat >"$SB/bin/useradd" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/useradd.log"
home=/var/lib/llamaman
shell=/usr/sbin/nologin
name=''
while [ "$#" -gt 0 ]; do
	case $1 in
	--home-dir) home=$2; shift 2 ;;
	--shell) shell=$2; shift 2 ;;
	--system) shift ;;
	*) name=$1; shift ;;
	esac
done
printf '%s:x:986:986::%s:%s\n' "$name" "$home" "$shell" >>"$LM_TEST_ROOT/etc/passwd"
EOF

	# curl — two shapes. `-o DEST URL` serves an artifact out of the fake
	# release directory (and 404s like the real thing when it is absent); no
	# `-o` is the /api/v1/meta readiness probe.
	cat >"$SB/bin/curl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/curl.log"
dest=''
url=''
while [ "$#" -gt 0 ]; do
	case $1 in
	-o) dest=$2; shift 2 ;;
	--retry|--connect-timeout|--max-time) shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
if [ -n "$dest" ]; then
	name=${url##*/}
	[ -f "$LM_TEST_ART/$name" ] || exit 22
	cp "$LM_TEST_ART/$name" "$dest"
	exit 0
fi
case $url in
*":${LM_TEST_META_PORT:-0}/api/v1/meta") printf '%s\n' "${LM_TEST_META:-}" ;;
*) exit 22 ;;
esac
EOF

	# systemctl — records every call and answers list-units from a fixture, so a
	# test can put instance units on the host without a service manager.
	cat >"$SB/bin/systemctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/systemctl.log"
case ${1:-} in
list-units)
	if [ -f "$LM_TEST_LOG/instances.txt" ]; then cat "$LM_TEST_LOG/instances.txt"; fi
	;;
esac
exit 0
EOF

	cat >"$SB/bin/loginctl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/loginctl.log"
exit 0
EOF

	# runuser — records the whole invocation. Everything the user-scope path
	# does lands HERE and not in systemctl.log, which is exactly what lets a
	# test assert that no system-scope enable was issued.
	cat >"$SB/bin/runuser" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/runuser.log"
exit 0
EOF

	# install — the real thing with the ownership flags dropped, because the
	# suite runs unprivileged and `install -o` needs root. Directory creation,
	# modes and the atomic staged copy are therefore all genuine.
	cat >"$SB/bin/install" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/install.log"
args=''
set -- "$@"
out=''
while [ "$#" -gt 0 ]; do
	case $1 in
	-o|-g) shift 2 ;;
	*) out="$out
$1"; shift ;;
	esac
done
# Rebuild the argument list without -o/-g, one per line so spaces survive.
IFS='
'
# shellcheck disable=SC2086
set -f
# shellcheck disable=SC2046
set -- $out
set +f
unset IFS
exec /usr/bin/install "$@"
EOF

	cat >"$SB/bin/chown" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/chown.log"
exit 0
EOF

	# date — a counter, so the two wall-clock deadlines in install.sh (the 10 s
	# user-bus poll and the 30 s readiness poll) elapse in logical time instead
	# of holding the suite for forty real seconds.
	cat >"$SB/bin/date" <<'EOF'
#!/bin/sh
if [ "${1:-}" = '+%s' ]; then
	n=0
	[ ! -f "$LM_TEST_LOG/clock" ] || n=$(cat "$LM_TEST_LOG/clock")
	n=$((n + 1))
	printf '%s\n' "$n" >"$LM_TEST_LOG/clock"
	printf '%s\n' "$n"
	exit 0
fi
exec /usr/bin/date "$@"
EOF

	cat >"$SB/bin/sleep" <<'EOF'
#!/bin/sh
exit 0
EOF

	# The llamaman binary itself, as shipped inside the fake tarball. It records
	# every subcommand the installer invokes and, for install-units, writes the
	# five units and the polkit rule into the fake root so the uninstall has
	# real files to remove.
	cat >"$SB/bin/llamaman-stub" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$LM_TEST_LOG/llamaman.log"
cmd=${1:-}
shift 2>/dev/null || true
case $cmd in
verify-release)
	exit "${LM_TEST_VERIFY_RC:-0}"
	;;
install-units)
	rc=${LM_TEST_INSTALL_UNITS_RC:-0}
	[ "$rc" -eq 0 ] || exit "$rc"
	root=''
	dir=/etc/systemd/system
	polkit=1
	while [ "$#" -gt 0 ]; do
		case $1 in
		--root) root=$2; shift 2 ;;
		--user-units) dir=/etc/systemd/user; polkit=0; shift ;;
		--dry-run) exit 0 ;;
		*) shift ;;
		esac
	done
	mkdir -p "$root$dir"
	for u in llamaman.service llamaman-instance@.service llamaman-instances.target \
		llamaman-selfupdate.service llamaman-update-verify.service; do
		[ "$dir" = /etc/systemd/user ] && [ "$u" = llamaman-selfupdate.service ] && continue
		printf '# llamaman-units: 1\n' >"$root$dir/$u"
	done
	if [ "$polkit" -eq 1 ]; then
		mkdir -p "$root/etc/polkit-1/rules.d"
		printf '// llamaman\n' >"$root/etc/polkit-1/rules.d/49-llamaman.rules"
	fi
	exit 0
	;;
doctor)
	printf 'llamaman doctor: systemd ok, database skipped (not yet created)\n'
	exit 0
	;;
status)
	printf '%s\n' "${LM_TEST_STATUS:-}"
	exit 0
	;;
esac
exit 0
EOF

	chmod 0755 "$SB"/bin/*
}

# write_release builds a GENUINE tarball and a GENUINE checksums.txt, so the
# sha256 step of install.sh is verified by the host's own sha256sum rather than
# stubbed. The other architecture's tarball is listed but never produced, which
# is the shape a real release has and the case the asset-selection awk must get
# right.
write_release() {
	cp "$SB/bin/llamaman-stub" "$SB/pkg/llamaman"
	chmod 0755 "$SB/pkg/llamaman"
	printf 'LICENSE\n' >"$SB/pkg/LICENSE"
	printf 'README\n' >"$SB/pkg/README.md"
	tar -czf "$SB/art/$ASSET" -C "$SB/pkg" llamaman LICENSE README.md

	(
		cd "$SB/art" || exit 1
		sha256sum "$ASSET" >checksums.txt
		printf '%s  %s\n' \
			'0000000000000000000000000000000000000000000000000000000000000000' \
			"$OTHER_ASSET" >>checksums.txt
	)
	printf 'not-a-real-signature\n' >"$SB/art/checksums.txt.sig"
}

# ---------------------------------------------------------------------------
# Running the installer
# ---------------------------------------------------------------------------

run_installer() {
	STATUS=0
	env -i \
		PATH="$SB/bin:/usr/bin:/bin" \
		HOME=/root \
		SUDO_USER="${LM_SUDO_USER-alice}" \
		TMPDIR="$SB/tmp" \
		LM_TEST_ROOT="$SB/root" \
		LM_TEST_LOG="$SB/log" \
		LM_TEST_ART="$SB/art" \
		LM_TEST_EUID="${LM_TEST_EUID:-0}" \
		LM_TEST_VERIFY_RC="${LM_TEST_VERIFY_RC:-0}" \
		LM_TEST_INSTALL_UNITS_RC="${LM_TEST_INSTALL_UNITS_RC:-0}" \
		LM_TEST_META="${LM_TEST_META:-}" \
		LM_TEST_META_PORT="${LM_TEST_META_PORT:-0}" \
		LM_TEST_STATUS="${LM_TEST_STATUS:-}" \
		sh "$LM_TEST_SCRIPT" --root "$SB/root" "$@" >"$OUT" 2>&1 || STATUS=$?
}

# ready_daemon makes the readiness probe succeed on one port and gives `status`
# a Setup block to print, which together are step 9 and step 10.
ready_daemon() {
	LM_TEST_META_PORT=${1:-5526}
	LM_TEST_META='{"version":"v1.4.2","ui_port":'"$LM_TEST_META_PORT"'}'
	LM_TEST_STATUS='llamaman v1.4.2 (commit abc1234) — running, pid 42
  UI            http://127.0.0.1:'"$LM_TEST_META_PORT"'   (desired 5526, actual '"$LM_TEST_META_PORT"')
  Setup         NOT COMPLETE — open http://127.0.0.1:'"$LM_TEST_META_PORT"'
                setup token  7Fq2xxxxnR4d   (not needed from this machine)'
	export LM_TEST_META_PORT LM_TEST_META LM_TEST_STATUS
}

reset_env() {
	LM_TEST_EUID=0
	LM_TEST_VERIFY_RC=0
	LM_TEST_INSTALL_UNITS_RC=0
	LM_TEST_META=''
	LM_TEST_META_PORT=0
	LM_TEST_STATUS=''
	LM_SUDO_USER=alice
	export LM_TEST_EUID LM_TEST_VERIFY_RC LM_TEST_INSTALL_UNITS_RC \
		LM_TEST_META LM_TEST_META_PORT LM_TEST_STATUS LM_SUDO_USER
}

# ---------------------------------------------------------------------------
# The test table
# ---------------------------------------------------------------------------

# Every test is a function named t_<something>. run_test sets up a fresh
# sandbox, runs one, and tears it down; a failing assertion reports and returns,
# so one broken branch does not hide the others.
run_test() {
	LM_TEST_NAME=$1
	case $LM_TEST_NAME in
	*"$LM_TEST_FILTER"*) : ;;
	*) return 0 ;;
	esac

	reset_env
	sandbox_new
	if "t_$1"; then
		LM_TEST_PASS=$((LM_TEST_PASS + 1))
		printf 'ok - %s\n' "$LM_TEST_NAME"
	fi
	sandbox_free
}

# --- argument parsing ------------------------------------------------------

t_help_exits_zero() {
	run_installer --help
	assert_status 0 || return 1
	assert_out '--dedicated-user' || return 1
	assert_out '--user-units' || return 1
	assert_out '--purge-models' || return 1
	# --help must not have installed anything.
	assert_no_file /usr/local/bin/llamaman || return 1
}

t_unknown_flag_is_refused() {
	run_installer --frobnicate
	assert_status_nonzero || return 1
	assert_out 'unknown argument: --frobnicate' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
}

t_positional_argument_is_refused() {
	run_installer v1.2.3
	assert_status_nonzero || return 1
	assert_out 'unknown argument: v1.2.3' || return 1
}

t_flag_without_value_is_refused() {
	run_installer --version
	assert_status_nonzero || return 1
	assert_out '--version needs a value' || return 1
}

t_flag_swallowing_the_next_flag_is_refused() {
	run_installer --version --no-start
	assert_status_nonzero || return 1
	assert_out 'got the flag --no-start' || return 1
}

t_port_must_be_numeric() {
	run_installer --port http
	assert_status_nonzero || return 1
	assert_out '--port must be a number' || return 1
}

t_port_must_be_in_range() {
	run_installer --port 70000
	assert_status_nonzero || return 1
	assert_out '--port must be between 1 and 65535' || return 1
}

t_prefix_must_be_absolute() {
	run_installer --prefix bin
	assert_status_nonzero || return 1
	assert_out '--prefix must be an absolute path' || return 1
}

t_purge_without_uninstall_is_refused() {
	run_installer --purge
	assert_status_nonzero || return 1
	assert_out '--purge is only meaningful with --uninstall' || return 1
}

t_purge_models_needs_purge() {
	run_installer --uninstall --purge-models
	assert_status_nonzero || return 1
	assert_out '--purge-models needs --purge' || return 1
}

t_dedicated_user_and_user_conflict() {
	run_installer --dedicated-user --user alice
	assert_status_nonzero || return 1
	assert_out 'ask for two different identities' || return 1
}

# --- preconditions ---------------------------------------------------------

t_non_root_prints_the_sudo_line() {
	LM_TEST_EUID=1000
	export LM_TEST_EUID
	run_installer --port 9000
	assert_status_nonzero || return 1
	assert_out 'must run as root' || return 1
	assert_out 'sudo sh -c "curl -fsSL' || return 1
	# The operator's own arguments are preserved in the line they are told to
	# retype, or the advice is wrong for their invocation.
	assert_out '--port 9000' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
}

t_missing_systemd_is_refused() {
	rmdir "$SB/root/run/systemd/system" "$SB/root/run/systemd" "$SB/root/run"
	run_installer
	assert_status_nonzero || return 1
	assert_out 'not running systemd' || return 1
}

t_no_identity_asks_for_one() {
	LM_SUDO_USER=''
	export LM_SUDO_USER
	run_installer
	assert_status_nonzero || return 1
	assert_out 'no service identity could be determined' || return 1
	assert_out '--dedicated-user' || return 1
}

t_unknown_user_is_refused() {
	run_installer --user nobody-here
	assert_status_nonzero || return 1
	assert_out 'no such account: nobody-here' || return 1
}

# --- download and verification --------------------------------------------

t_install_verifies_and_lands() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1

	assert_out 'sha256 ok' || return 1
	assert_out 'verifying the release signature' || return 1
	assert_out "installing llamaman $VERSION" || return 1
	assert_file /usr/local/bin/llamaman || return 1

	# The state tree of DESIGN section 6.1, created by the installer so the
	# daemon's first boot finds it already correct.
	assert_dir /var/lib/llamaman || return 1
	for child in versions src build logs db-backups update tmp; do
		assert_dir "/var/lib/llamaman/$child" || return 1
	done
	assert_out 'state directory: /var/lib/llamaman' || return 1

	# The binary writes the units, not the shell script (D48).
	assert_log llamaman.log 'install-units --identity alice --prefix /usr/local/bin' || return 1
	assert_file /etc/systemd/system/llamaman.service || return 1
	assert_file /etc/systemd/system/llamaman-update-verify.service || return 1
	assert_file /etc/polkit-1/rules.d/49-llamaman.rules || return 1

	# Step 8's toolchain report and step 9's start, in system scope.
	assert_log llamaman.log 'doctor --format=text' || return 1
	assert_log systemctl.log 'daemon-reload' || return 1
	assert_log systemctl.log 'enable --now llamaman-instances.target llamaman.service' || return 1

	# Step 10: the plain-text Setup block, verbatim, and no jq anywhere.
	assert_out 'setup token  7Fq2xxxxnR4d' || return 1
	assert_out 'llamaman is running: http://127.0.0.1:5526' || return 1
	assert_not_out 'jq' || return 1
}

t_verify_release_is_handed_the_downloaded_asset() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	assert_log llamaman.log "verify-release --require $ASSET" || return 1
}

t_only_this_architecture_is_downloaded() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	assert_log curl.log "$ASSET" || return 1
	assert_no_log curl.log "$OTHER_ASSET" || return 1
}

t_corrupt_download_aborts_before_installing() {
	printf 'corrupted\n' >"$SB/art/$ASSET"
	run_installer
	assert_status_nonzero || return 1
	assert_out 'failed its sha256 check' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
	assert_no_dir /var/lib/llamaman || return 1
}

t_bad_signature_aborts_before_installing() {
	LM_TEST_VERIFY_RC=1
	export LM_TEST_VERIFY_RC
	run_installer
	assert_status_nonzero || return 1
	assert_out 'signature did not verify' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
	assert_no_dir /var/lib/llamaman || return 1
}

t_missing_release_aborts() {
	rm -f "$SB/art/checksums.txt"
	run_installer
	assert_status_nonzero || return 1
	assert_out 'could not fetch' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
}

t_missing_signature_aborts() {
	rm -f "$SB/art/checksums.txt.sig"
	run_installer
	assert_status_nonzero || return 1
	assert_out 'a release without a signature is not installable' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
}

t_no_tarball_for_this_arch_aborts() {
	printf '%s  %s\n' \
		'0000000000000000000000000000000000000000000000000000000000000000' \
		"$OTHER_ASSET" >"$SB/art/checksums.txt"
	run_installer
	assert_status_nonzero || return 1
	assert_out "publishes no linux/$ARCH tarball" || return 1
}

t_pinned_version_uses_the_tag_url() {
	ready_daemon 5526
	run_installer --version "$VERSION"
	assert_status 0 || return 1
	assert_log curl.log "releases/download/$VERSION/checksums.txt" || return 1
	assert_no_log curl.log 'releases/latest/download' || return 1
}

t_latest_uses_the_redirect_not_the_api() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	assert_log curl.log 'releases/latest/download/checksums.txt' || return 1
	assert_no_log curl.log 'api.github.com' || return 1
}

# --- flags that change the install ----------------------------------------

t_prefix_is_threaded_everywhere() {
	ready_daemon 5526
	mkdir -p "$SB/root/opt/lm-bin"
	run_installer --prefix /opt/lm-bin
	assert_status 0 || return 1
	assert_file /opt/lm-bin/llamaman || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
	assert_log llamaman.log '--prefix /opt/lm-bin' || return 1
}

t_port_is_threaded_and_polled() {
	ready_daemon 9000
	run_installer --port 9000
	assert_status 0 || return 1
	assert_log llamaman.log '--port 9000' || return 1
	assert_log curl.log '127.0.0.1:9000/api/v1/meta' || return 1
	assert_out 'llamaman is running: http://127.0.0.1:9000' || return 1
	# The +20 walk follows the requested port rather than 5526.
	assert_no_log curl.log '127.0.0.1:5526/api/v1/meta' || return 1
}

t_port_walk_finds_a_moved_daemon() {
	ready_daemon 5531
	LM_TEST_META_PORT=5531
	export LM_TEST_META_PORT
	run_installer
	assert_status 0 || return 1
	assert_out 'llamaman is running: http://127.0.0.1:5531' || return 1
}

t_no_autostart_grant_is_passed_through() {
	ready_daemon 5526
	run_installer --no-autostart-grant
	assert_status 0 || return 1
	assert_log llamaman.log '--no-autostart-grant' || return 1
}

t_repair_polkit_is_passed_through() {
	ready_daemon 5526
	run_installer --repair-polkit
	assert_status 0 || return 1
	assert_log llamaman.log '--repair-polkit' || return 1
}

t_dedicated_user_is_created_once() {
	ready_daemon 5526
	run_installer --dedicated-user
	assert_status 0 || return 1
	assert_log useradd.log '--system --home-dir /var/lib/llamaman' || return 1
	assert_log llamaman.log '--identity llamaman' || return 1
	# Section 7.2 rule 4 needs this present before the daemon's first boot.
	assert_dir /var/lib/llamaman/hf-cache/hub || return 1

	# Idempotent: the second run must not try to create the account again.
	rm -f "$SB/log/useradd.log"
	run_installer --dedicated-user
	assert_status 0 || return 1
	assert_out 'already exists' || return 1
	assert_log_absent useradd.log || return 1
}

t_no_start_suppresses_the_start() {
	run_installer --no-start
	assert_status 0 || return 1
	assert_file /usr/local/bin/llamaman || return 1
	assert_out 'not starting anything (--no-start)' || return 1
	assert_no_log systemctl.log 'enable --now' || return 1
	assert_no_log llamaman.log 'status' || return 1
}

t_dry_run_writes_nothing() {
	run_installer --dry-run
	assert_status 0 || return 1
	assert_out '[dry-run]' || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
	assert_no_dir /var/lib/llamaman || return 1
	assert_no_log systemctl.log 'enable --now' || return 1
	# It still fetched and verified for real, which is what makes the preview
	# worth reading.
	assert_out 'sha256 ok' || return 1
}

# --- the upgrade path (step 11) -------------------------------------------

t_rerun_restarts_instead_of_enabling() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1

	rm -f "$SB/log/systemctl.log"
	run_installer
	assert_status 0 || return 1
	assert_log systemctl.log 'restart llamaman.service' || return 1
	assert_out 'running instances were not touched' || return 1
	assert_no_log systemctl.log 'enable --now' || return 1

	# Instance units are never touched by an upgrade.
	assert_no_log systemctl.log 'llamaman-instance@' || return 1
}

t_upgrade_with_no_start_does_not_restart() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1

	rm -f "$SB/log/systemctl.log"
	run_installer --no-start
	assert_status 0 || return 1
	assert_out 'not restarting llamaman.service (--no-start)' || return 1
	assert_no_log systemctl.log 'restart llamaman.service' || return 1
	# Section 12.4 step 2: the older binary is on disk and nothing started it.
	assert_file /usr/local/bin/llamaman || return 1
}

t_upgrade_preserves_the_state_directory() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	printf 'db\n' >"$SB/root/var/lib/llamaman/llamaman.db"

	run_installer
	assert_status 0 || return 1
	assert_file /var/lib/llamaman/llamaman.db || return 1
}

# --- the user-scope topology (D2) -----------------------------------------

t_user_units_moves_everything() {
	ready_daemon 5526
	run_installer --user-units
	assert_status 0 || return 1

	# The binary is under the account's own ~/.local/bin, in a directory the
	# installer created, and the state tree is under $XDG_STATE_HOME (D72) —
	# never /var/lib/llamaman.
	assert_file /home/alice/.local/bin/llamaman || return 1
	assert_dir /home/alice/.local/state/llamaman/versions || return 1
	assert_no_dir /var/lib/llamaman || return 1
	assert_no_file /usr/local/bin/llamaman || return 1
	assert_out 'state directory: /home/alice/.local/state/llamaman' || return 1

	# Units go to /etc/systemd/user and there is no polkit rule at all.
	assert_file /etc/systemd/user/llamaman.service || return 1
	assert_no_file /etc/systemd/system/llamaman.service || return 1
	assert_no_file /etc/polkit-1/rules.d/49-llamaman.rules || return 1
	assert_log llamaman.log '--user-units' || return 1
	assert_log llamaman.log '--prefix /home/alice/.local/bin' || return 1

	# Section 5.2a item 3: linger first, then the account's OWN manager through
	# runuser. Root's system manager must never be asked to enable these.
	assert_log loginctl.log 'enable-linger alice' || return 1
	assert_log runuser.log 'systemctl --user daemon-reload' || return 1
	assert_log runuser.log 'systemctl --user enable --now llamaman-instances.target llamaman.service' || return 1
	assert_log runuser.log 'XDG_RUNTIME_DIR=/run/user/1000' || return 1
	assert_no_log systemctl.log 'enable --now' || return 1
	assert_no_log systemctl.log 'daemon-reload' || return 1
}

t_user_units_with_explicit_prefix() {
	ready_daemon 5526
	mkdir -p "$SB/root/opt/lm-bin"
	run_installer --user-units --prefix /opt/lm-bin
	assert_status 0 || return 1
	assert_file /opt/lm-bin/llamaman || return 1
	assert_log chown.log 'alice:alice' || return 1
}

t_system_scope_binary_is_root_owned() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	# D15: a service-user-writable file on root's PATH is an escalation trap.
	assert_log chown.log '0:0' || return 1
	assert_no_log chown.log 'alice:alice' || return 1
}

# --- uninstall (step 12) ---------------------------------------------------

t_uninstall_removes_units_and_keeps_state() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	printf 'db\n' >"$SB/root/var/lib/llamaman/llamaman.db"
	printf 'prev\n' >"$SB/root/usr/local/bin/llamaman.prev"
	printf 'llamaman-instance@qwen.service loaded active running\n' >"$SB/log/instances.txt"

	rm -f "$SB/log/systemctl.log"
	run_installer --uninstall
	assert_status 0 || return 1

	assert_log systemctl.log 'disable --now llamaman.service llamaman-instances.target' || return 1
	assert_log systemctl.log 'stop llamaman-instance@qwen.service' || return 1
	assert_log systemctl.log 'daemon-reload' || return 1

	assert_no_file /etc/systemd/system/llamaman.service || return 1
	assert_no_file /etc/systemd/system/llamaman-update-verify.service || return 1
	assert_no_file /etc/polkit-1/rules.d/49-llamaman.rules || return 1
	assert_no_file /usr/local/bin/llamaman || return 1

	# D89: the retained previous binary is ours and lives in a directory that
	# is not, so leaving it behind leaves a stray root-owned file on root's PATH.
	assert_no_file /usr/local/bin/llamaman.prev || return 1

	# The state directory is KEPT, and its path is printed.
	assert_file /var/lib/llamaman/llamaman.db || return 1
	assert_out 'kept /var/lib/llamaman' || return 1
}

t_uninstall_purge_removes_state() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1
	printf 'db\n' >"$SB/root/var/lib/llamaman/llamaman.db"

	run_installer --uninstall --purge
	assert_status 0 || return 1
	assert_no_dir /var/lib/llamaman || return 1
	assert_out 'removed /var/lib/llamaman' || return 1
}

t_uninstall_purge_refuses_to_delete_models() {
	ready_daemon 5526
	run_installer --dedicated-user
	assert_status 0 || return 1
	printf 'gguf\n' >"$SB/root/var/lib/llamaman/hf-cache/hub/model.gguf"

	run_installer --uninstall --dedicated-user --purge
	assert_status 0 || return 1
	assert_out 'That is your model library' || return 1
	assert_out 'Re-run with --purge-models' || return 1
	# Nothing under the state directory was removed.
	assert_file /var/lib/llamaman/hf-cache/hub/model.gguf || return 1
	assert_dir /var/lib/llamaman || return 1
}

t_uninstall_purge_models_removes_them() {
	ready_daemon 5526
	run_installer --dedicated-user
	assert_status 0 || return 1
	printf 'gguf\n' >"$SB/root/var/lib/llamaman/hf-cache/hub/model.gguf"

	run_installer --uninstall --dedicated-user --purge --purge-models
	assert_status 0 || return 1
	assert_no_dir /var/lib/llamaman || return 1
}

t_uninstall_user_units_uses_the_user_manager() {
	ready_daemon 5526
	run_installer --user-units
	assert_status 0 || return 1

	rm -f "$SB/log/systemctl.log" "$SB/log/runuser.log" "$SB/log/loginctl.log"
	run_installer --user-units --uninstall
	assert_status 0 || return 1

	assert_log runuser.log 'systemctl --user disable --now llamaman.service' || return 1
	assert_log loginctl.log 'disable-linger alice' || return 1
	assert_no_file /etc/systemd/user/llamaman.service || return 1
	assert_no_file /home/alice/.local/bin/llamaman || return 1
	assert_dir /home/alice/.local/state/llamaman || return 1
	assert_no_log systemctl.log 'disable --now' || return 1
}

t_uninstall_never_downloads() {
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1

	rm -f "$SB/log/curl.log"
	run_installer --uninstall
	assert_status 0 || return 1
	assert_log_absent curl.log || return 1
}

t_uninstall_on_a_clean_host_is_harmless() {
	run_installer --uninstall
	assert_status 0 || return 1
	assert_out 'uninstalled' || return 1
}

# --- D48: a truncated download must execute nothing ------------------------

t_truncated_script_executes_nothing() {
	# The classic curl-to-shell hazard. Feed the shell prefixes of the script at
	# many lengths and assert that not one of them touches the host: no file
	# created or removed, and no stub invoked.
	ready_daemon 5526
	run_installer
	assert_status 0 || return 1

	before="$SB/before.txt"
	after="$SB/after.txt"
	(cd "$SB/root" && find . | sort) >"$before"
	rm -f "$SB"/log/*.log

	total=$(wc -c <"$LM_TEST_SCRIPT")
	n=1
	while [ "$n" -le 20 ]; do
		cut=$((total * n / 21))
		head -c "$cut" "$LM_TEST_SCRIPT" >"$SB/trunc.sh"
		env -i PATH="$SB/bin:/usr/bin:/bin" HOME=/root SUDO_USER=alice \
			LM_TEST_ROOT="$SB/root" LM_TEST_LOG="$SB/log" LM_TEST_ART="$SB/art" \
			sh "$SB/trunc.sh" --uninstall --purge >/dev/null 2>&1 || true
		n=$((n + 1))
	done

	(cd "$SB/root" && find . | sort) >"$after"
	if ! cmp -s "$before" "$after"; then
		fail 'a truncated copy of install.sh changed the filesystem'
		diff "$before" "$after" | sed 's/^/  # /' || true
		return 1
	fi
	for log in systemctl loginctl runuser curl useradd llamaman chown; do
		if [ -s "$SB/log/$log.log" ]; then
			fail "a truncated copy of install.sh invoked $log"
			return 1
		fi
	done
}

# --- failure reporting -----------------------------------------------------

t_install_units_failure_is_fatal_and_located() {
	LM_TEST_INSTALL_UNITS_RC=1
	export LM_TEST_INSTALL_UNITS_RC
	run_installer
	assert_status_nonzero || return 1
	assert_out 'install-units failed' || return 1
	assert_out 'FAILED while' || return 1
	assert_out 'writing the units and polkit rules' || return 1
	assert_no_log systemctl.log 'enable --now' || return 1
}

t_every_message_is_prefixed() {
	run_installer --no-start
	assert_status 0 || return 1
	# DESIGN section 13: every user-visible message is prefixed `llamaman:` so
	# failures are greppable. Blank lines and the doctor report are the daemon's
	# own output and are exempt.
	bad=$(grep -v '^llamaman:' "$OUT" | grep -v '^$' | grep -v '^llamaman doctor:' | grep -v '^  ' || true)
	if [ -n "$bad" ]; then
		fail "unprefixed output: $bad"
		return 1
	fi
}

t_setup_block_collapses_when_already_claimed() {
	LM_TEST_META_PORT=5526
	LM_TEST_META='{"ui_port":5526}'
	LM_TEST_STATUS='llamaman v1.4.2 (commit abc1234) — running, pid 42
  Setup         complete'
	export LM_TEST_META_PORT LM_TEST_META LM_TEST_STATUS
	run_installer
	assert_status 0 || return 1
	assert_out 'setup token was already used' || return 1
	assert_not_out 'setup token  7Fq' || return 1
}

t_unreachable_daemon_prints_the_journal_line() {
	LM_TEST_META=''
	LM_TEST_META_PORT=0
	export LM_TEST_META LM_TEST_META_PORT
	run_installer
	assert_status 0 || return 1
	assert_out 'did not answer within 30 s' || return 1
	assert_out 'journalctl -u llamaman -n 50' || return 1
	# It must NOT print a URL it could not verify.
	assert_not_out 'llamaman is running:' || return 1
}

# ---------------------------------------------------------------------------
# Runner
# ---------------------------------------------------------------------------

main() {
	printf '# install.sh: %s\n' "$LM_TEST_SCRIPT"

	for name in \
		help_exits_zero \
		unknown_flag_is_refused \
		positional_argument_is_refused \
		flag_without_value_is_refused \
		flag_swallowing_the_next_flag_is_refused \
		port_must_be_numeric \
		port_must_be_in_range \
		prefix_must_be_absolute \
		purge_without_uninstall_is_refused \
		purge_models_needs_purge \
		dedicated_user_and_user_conflict \
		non_root_prints_the_sudo_line \
		missing_systemd_is_refused \
		no_identity_asks_for_one \
		unknown_user_is_refused \
		install_verifies_and_lands \
		verify_release_is_handed_the_downloaded_asset \
		only_this_architecture_is_downloaded \
		corrupt_download_aborts_before_installing \
		bad_signature_aborts_before_installing \
		missing_release_aborts \
		missing_signature_aborts \
		no_tarball_for_this_arch_aborts \
		pinned_version_uses_the_tag_url \
		latest_uses_the_redirect_not_the_api \
		prefix_is_threaded_everywhere \
		port_is_threaded_and_polled \
		port_walk_finds_a_moved_daemon \
		no_autostart_grant_is_passed_through \
		repair_polkit_is_passed_through \
		dedicated_user_is_created_once \
		no_start_suppresses_the_start \
		dry_run_writes_nothing \
		rerun_restarts_instead_of_enabling \
		upgrade_with_no_start_does_not_restart \
		upgrade_preserves_the_state_directory \
		user_units_moves_everything \
		user_units_with_explicit_prefix \
		system_scope_binary_is_root_owned \
		uninstall_removes_units_and_keeps_state \
		uninstall_purge_removes_state \
		uninstall_purge_refuses_to_delete_models \
		uninstall_purge_models_removes_them \
		uninstall_user_units_uses_the_user_manager \
		uninstall_never_downloads \
		uninstall_on_a_clean_host_is_harmless \
		truncated_script_executes_nothing \
		install_units_failure_is_fatal_and_located \
		every_message_is_prefixed \
		setup_block_collapses_when_already_claimed \
		unreachable_daemon_prints_the_journal_line; do
		run_test "$name"
	done

	printf '\n1..%s\n' "$((LM_TEST_PASS + LM_TEST_FAIL))"
	printf '# passed %s, failed %s\n' "$LM_TEST_PASS" "$LM_TEST_FAIL"
	if [ "$LM_TEST_FAIL" -ne 0 ]; then
		printf '# failures:%s\n' "$LM_TEST_FAILED_NAMES"
		exit 1
	fi
	exit 0
}

main "$@"
