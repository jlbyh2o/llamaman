#!/bin/sh
# llamaman installer — DESIGN section 13.
#
# POSIX sh. No bashisms, no arrays, no `local`, no process substitution, and no
# dependency beyond coreutils plus one of curl/wget. It runs on whatever /bin/sh
# is on the host, which on Debian and Ubuntu is dash.
#
#   curl -fsSL https://raw.githubusercontent.com/jlbyh2o/llamaman/main/installer/install.sh | sudo sh
#
# D48 — THE WHOLE SCRIPT IS A FUNCTION, INVOKED ON THE LAST LINE. A `curl | sh`
# pipe truncated mid-transfer feeds the shell a prefix of this file. If the body
# ran at top level that prefix would EXECUTE: half an uninstall, a binary
# installed with no units, a state directory created and abandoned. Wrapped this
# way, a truncated copy defines some functions, reaches EOF without ever calling
# main, and does nothing at all. Nothing below may execute at parse time beyond
# the constant assignments, and nothing may be added after the final `main "$@"`.
#
# Every user-visible message is prefixed `llamaman:` so failures are greppable.

set -eu

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

LM_REPO='jlbyh2o/llamaman'
LM_SCRIPT_URL='https://raw.githubusercontent.com/jlbyh2o/llamaman/main/installer/install.sh'
LM_DEFAULT_PREFIX='/usr/local/bin'
LM_DEFAULT_STATE_DIR='/var/lib/llamaman'
LM_DEFAULT_PORT=5526
LM_DEDICATED_ACCOUNT='llamaman'

# The state-directory children of DESIGN section 6.1. This script creates them so
# the daemon's first boot finds the layout already correct and already owned.
LM_STATE_CHILDREN='versions src build logs db-backups update tmp'

# The five units of DESIGN section 5.2. `install-units` writes them; this script
# only ever removes them, and it must remove exactly the set that command writes
# — the two self-update actors included, since leaving
# llamaman-update-verify.service behind after an uninstall leaves an OnFailure=
# target for a service that no longer exists.
LM_UNITS='llamaman.service llamaman-instance@.service llamaman-instances.target llamaman-selfupdate.service llamaman-update-verify.service'

# The two polkit forms of DESIGN section 5.2. Only one is normally present —
# install-units writes whichever the detected polkit version supports, and both
# when detection is ambiguous — so the uninstall removes both unconditionally.
LM_POLKIT_RULES='/etc/polkit-1/rules.d/49-llamaman.rules'
LM_POLKIT_PKLA='/etc/polkit-1/localauthority/50-local.d/49-llamaman.pkla'

# The fixed release asset names of DESIGN section 16.2 step 3. checksums.txt has
# a version-free name on purpose: it is what `releases/latest/download/` can be
# asked for WITHOUT knowing the tag, and the asset names inside it are what tell
# this script which version it is about to install (D48, step 2 below).
LM_CHECKSUMS='checksums.txt'
LM_CHECKSUMS_SIG='checksums.txt.sig'

# Step 1's floor, in KiB: 500 MB free where the state directory will live.
LM_MIN_FREE_KB=512000

# ---------------------------------------------------------------------------
# Flags (DESIGN section 13) and everything derived from them
# ---------------------------------------------------------------------------

LM_VERSION=''
LM_USER=''
LM_DEDICATED=0
LM_USER_UNITS=0
LM_PORT=''
LM_PREFIX=''
LM_NO_AUTOSTART_GRANT=0
LM_NO_START=0
LM_REPAIR_POLKIT=0
LM_UNINSTALL=0
LM_PURGE=0
LM_PURGE_MODELS=0
LM_DRY_RUN=0
LM_ROOT=''

LM_ARCH=''
LM_UID=''
LM_GID=''
LM_GROUP=''
LM_HOME=''
LM_STATE_DIR=''
LM_UNIT_DIR=''
LM_BIN=''
LM_TOOL=''
LM_TMP=''
LM_ASSET=''
LM_RESOLVED_VERSION=''
LM_DOWNLOADER=''
LM_SHA=''
LM_UPGRADE=0
LM_ACTUAL_PORT=''
LM_STEP='starting up'
LM_LINE=0

# ---------------------------------------------------------------------------
# Output, failure reporting and the dry-run seam
# ---------------------------------------------------------------------------

lm_say() { printf 'llamaman: %s\n' "$*"; }
lm_warn() { printf 'llamaman: %s\n' "$*" >&2; }

lm_die() {
	printf 'llamaman: %s\n' "$*" >&2
	exit 1
}

# lm_at records where we are, so the exit trap can name the failing step and its
# line instead of leaving a bare non-zero status. POSIX sh has no ERR trap to
# hang this on, so the position is recorded rather than captured.
lm_at() {
	LM_LINE=$1
	LM_STEP=$2
}

# shellcheck disable=SC2329  # invoked indirectly, from main's EXIT trap
lm_exit_trap() {
	if [ -n "$LM_TMP" ] && [ -d "$LM_TMP" ]; then
		rm -rf "$LM_TMP"
	fi
	if [ "$1" -ne 0 ]; then
		printf 'llamaman: FAILED at line %s while %s (exit %s)\n' "$LM_LINE" "$LM_STEP" "$1" >&2
	fi
}

# lm_do runs one command that MUTATES THE HOST, or prints it under --dry-run.
# Writes into the temporary directory deliberately do not go through it: that
# directory is removed on exit and downloading into it changes nothing, so a
# --dry-run that fetches and verifies the real release is a far more useful
# preview than one that guesses.
lm_do() {
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		printf 'llamaman: [dry-run] %s\n' "$*"
		return 0
	fi
	"$@"
}

# lm_path maps an absolute host path through --root. Outside the test harness
# LM_ROOT is empty and this is the identity function.
lm_path() { printf '%s%s\n' "$LM_ROOT" "$1"; }

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------

lm_usage() {
	cat <<'EOF'
llamaman installer — manage llama.cpp servers on this host

  curl -fsSL https://raw.githubusercontent.com/jlbyh2o/llamaman/main/installer/install.sh | sudo sh

Install and upgrade:
  --version <tag>        install this release instead of the latest one
  --user <name>          the account the daemon runs as (default: $SUDO_USER)
  --dedicated-user       create and use a locked-down system account instead
  --user-units           install into the account's own systemd --user manager,
                         with no polkit rule at all (D2)
  --port <n>             the management port to seed (default 5526)
  --prefix <dir>         where the binary goes (default /usr/local/bin, or
                         ~<user>/.local/bin with --user-units)
  --no-autostart-grant   omit the polkit manage-unit-files grant; per-instance
                         autostart becomes read-only in the UI
  --no-start             install everything but do not start or restart anything
  --repair-polkit        rewrite the polkit files even when they already match
  --dry-run              print every change and write nothing

Removal:
  --uninstall            stop, disable and remove the units, the polkit files
                         and the binary; the state directory is KEPT
  --purge                also remove the state directory (with --uninstall)
  --purge-models         also remove a --dedicated-user install's Hugging Face
                         cache (with --purge; a second, explicit consent)

Other:
  --root <dir>           TESTING ONLY: treat <dir> as / for every path written,
                         and expect stubbed systemctl/curl on $PATH
  -h, --help             this text

Re-running installs the new binary, rewrites the units and restarts the daemon.
Instance units are never touched, so an upgrade does not interrupt inference.

The full specification is DESIGN section 13.
EOF
}

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------

# lm_need_value refuses a flag whose argument is missing rather than silently
# consuming the next flag as a value: `--version --no-start` must be an error,
# not an install of a release named "--no-start".
lm_need_value() {
	if [ "$#" -lt 2 ] || [ -z "$2" ]; then
		lm_die "$1 needs a value"
	fi
	case $2 in
	-*) lm_die "$1 needs a value (got the flag $2)" ;;
	esac
}

lm_parse_args() {
	while [ "$#" -gt 0 ]; do
		case $1 in
		--version)
			lm_need_value "$@"
			LM_VERSION=$2
			shift 2
			;;
		--user)
			lm_need_value "$@"
			LM_USER=$2
			shift 2
			;;
		--port)
			lm_need_value "$@"
			LM_PORT=$2
			shift 2
			;;
		--prefix)
			lm_need_value "$@"
			LM_PREFIX=$2
			shift 2
			;;
		--root)
			lm_need_value "$@"
			LM_ROOT=$2
			shift 2
			;;
		--dedicated-user)
			LM_DEDICATED=1
			shift
			;;
		--user-units)
			LM_USER_UNITS=1
			shift
			;;
		--no-autostart-grant)
			LM_NO_AUTOSTART_GRANT=1
			shift
			;;
		--no-start)
			LM_NO_START=1
			shift
			;;
		--repair-polkit)
			LM_REPAIR_POLKIT=1
			shift
			;;
		--uninstall)
			LM_UNINSTALL=1
			shift
			;;
		--purge)
			LM_PURGE=1
			shift
			;;
		--purge-models)
			LM_PURGE_MODELS=1
			shift
			;;
		--dry-run)
			LM_DRY_RUN=1
			shift
			;;
		-h | --help)
			lm_usage
			exit 0
			;;
		--)
			shift
			break
			;;
		*)
			lm_usage >&2
			lm_die "unknown argument: $1"
			;;
		esac
	done

	if [ "$#" -gt 0 ]; then
		lm_die "unexpected argument: $1"
	fi
}

lm_validate_args() {
	if [ -n "$LM_PORT" ]; then
		case $LM_PORT in
		'' | *[!0-9]*) lm_die "--port must be a number, got $LM_PORT" ;;
		esac
		if [ "$LM_PORT" -lt 1 ] || [ "$LM_PORT" -gt 65535 ]; then
			lm_die "--port must be between 1 and 65535, got $LM_PORT"
		fi
	fi

	case $LM_PREFIX in
	'' | /*) : ;;
	*) lm_die "--prefix must be an absolute path, got $LM_PREFIX" ;;
	esac

	if [ -n "$LM_ROOT" ]; then
		case $LM_ROOT in
		/) lm_die '--root / is not a test root; drop the flag to install for real' ;;
		/*) : ;;
		*) lm_die "--root must be an absolute path, got $LM_ROOT" ;;
		esac
		[ -d "$LM_ROOT" ] || lm_die "--root $LM_ROOT is not a directory"
		LM_ROOT=${LM_ROOT%/}
	fi

	# --purge and --purge-models are consent for an uninstall, not commands of
	# their own. Silently ignoring them on an install would be the worst kind of
	# no-op: the operator believes something was removed.
	if [ "$LM_UNINSTALL" -eq 0 ]; then
		[ "$LM_PURGE" -eq 0 ] || lm_die '--purge is only meaningful with --uninstall'
		[ "$LM_PURGE_MODELS" -eq 0 ] || lm_die '--purge-models is only meaningful with --uninstall --purge'
	fi
	if [ "$LM_PURGE_MODELS" -eq 1 ] && [ "$LM_PURGE" -eq 0 ]; then
		lm_die '--purge-models needs --purge'
	fi
}

# ---------------------------------------------------------------------------
# Step 1 — preconditions
# ---------------------------------------------------------------------------

lm_require_root() {
	if [ "$(id -u)" -eq 0 ]; then
		return 0
	fi

	# The script NEVER re-execs itself under sudo: under `curl | sh` there is no
	# file on disk to re-exec, and re-running the pipe from inside itself would
	# fetch the script a second time from a shell that has already been fed a
	# copy. Print the line the operator should have typed, with their own
	# arguments preserved, and stop.
	lm_warn 'must run as root.'
	if [ "$#" -gt 0 ]; then
		printf '  sudo sh -c "curl -fsSL %s | sh -s --%s"\n' \
			"$LM_SCRIPT_URL" "$(printf ' %s' "$@")" >&2
	else
		printf '  sudo sh -c "curl -fsSL %s | sh"\n' "$LM_SCRIPT_URL" >&2
	fi
	exit 1
}

lm_require_systemd() {
	if [ ! -d "$(lm_path /run/systemd/system)" ]; then
		lm_warn 'this host is not running systemd, which llamaman requires.'
		lm_warn '  /run/systemd/system does not exist, so there is no service manager to install units into.'
		exit 1
	fi
}

lm_detect_arch() {
	if [ "$(uname -s)" != Linux ]; then
		lm_die "llamaman runs on Linux only (this is $(uname -s))"
	fi
	case $(uname -m) in
	x86_64 | amd64) LM_ARCH=amd64 ;;
	aarch64 | arm64) LM_ARCH=arm64 ;;
	*) lm_die "unsupported architecture $(uname -m); llamaman publishes linux/amd64 and linux/arm64" ;;
	esac
}

lm_detect_tools() {
	if command -v curl >/dev/null 2>&1; then
		LM_DOWNLOADER=curl
	elif command -v wget >/dev/null 2>&1; then
		LM_DOWNLOADER=wget
	else
		lm_die 'neither curl nor wget is installed; one of them is needed to fetch the release'
	fi

	if command -v sha256sum >/dev/null 2>&1; then
		LM_SHA=sha256sum
	elif command -v shasum >/dev/null 2>&1; then
		LM_SHA=shasum
	else
		lm_die 'neither sha256sum nor shasum is installed; the download cannot be verified'
	fi
}

# lm_sha_check runs the chosen checker against a checksums file in the current
# directory. Two branches rather than one unquoted variable: a command held in a
# string and expanded unquoted is the shell hazard this script exists to avoid.
lm_sha_check() {
	if [ "$LM_SHA" = sha256sum ]; then
		sha256sum -c "$1"
	else
		shasum -a 256 -c "$1"
	fi
}

# lm_free_kb reports free space on the filesystem holding a path, walking up to
# the nearest existing ancestor: the state directory normally does not exist yet
# when this is asked.
lm_free_kb() {
	lm_free_dir=$1
	while [ ! -d "$lm_free_dir" ] && [ "$lm_free_dir" != / ]; do
		lm_free_dir=$(dirname "$lm_free_dir")
	done
	df -Pk "$lm_free_dir" 2>/dev/null | awk 'NR==2 {print $4}'
}

lm_check_space() {
	lm_space_free=$(lm_free_kb "$1")
	if [ -z "$lm_space_free" ]; then
		lm_warn "could not measure free space on $1; continuing"
		return 0
	fi
	if [ "$lm_space_free" -lt "$2" ]; then
		lm_die "$1 has $((lm_space_free / 1024)) MB free; llamaman needs at least $(($2 / 1024)) MB there"
	fi
}

# ---------------------------------------------------------------------------
# Step 4 — service identity
# ---------------------------------------------------------------------------

lm_passwd_home() {
	if command -v getent >/dev/null 2>&1; then
		getent passwd "$1" 2>/dev/null | cut -d: -f6
	else
		awk -F: -v u="$1" '$1 == u {print $6}' /etc/passwd 2>/dev/null
	fi
}

lm_user_exists() { id -u "$1" >/dev/null 2>&1; }

lm_create_dedicated_user() {
	if lm_user_exists "$LM_DEDICATED_ACCOUNT"; then
		lm_say "service account $LM_DEDICATED_ACCOUNT already exists"
		return 0
	fi

	# The shell is whichever nologin this distro ships. Passing one that does not
	# exist writes a passwd entry naming a missing binary — not fatal, but a
	# footgun in every later `su`.
	lm_nologin=/usr/sbin/nologin
	[ -x "$lm_nologin" ] || lm_nologin=/sbin/nologin
	[ -x "$lm_nologin" ] || lm_nologin=/bin/false

	command -v useradd >/dev/null 2>&1 ||
		lm_die 'useradd is not installed, so --dedicated-user cannot create the service account'

	lm_do useradd --system --home-dir "$LM_DEFAULT_STATE_DIR" \
		--shell "$lm_nologin" "$LM_DEDICATED_ACCOUNT" ||
		lm_die "could not create the $LM_DEDICATED_ACCOUNT service account"
	lm_say "created service account $LM_DEDICATED_ACCOUNT"
}

lm_resolve_identity() {
	if [ "$LM_DEDICATED" -eq 1 ]; then
		if [ -n "$LM_USER" ] && [ "$LM_USER" != "$LM_DEDICATED_ACCOUNT" ]; then
			lm_die "--dedicated-user and --user $LM_USER ask for two different identities; pick one"
		fi
		LM_USER=$LM_DEDICATED_ACCOUNT
		# An uninstall must never CREATE the account it is about to stop using.
		if [ "$LM_UNINSTALL" -eq 0 ]; then
			lm_create_dedicated_user
		fi
	fi

	if [ -z "$LM_USER" ]; then
		# SUDO_USER is the account that typed `sudo`, which is exactly the
		# identity SPEC section 5.1b wants the daemon to run as: it owns the
		# Hugging Face cache the daemon has to read and write.
		LM_USER=${SUDO_USER:-}
	fi

	if [ -z "$LM_USER" ] || [ "$LM_USER" = root ]; then
		if [ "$LM_UNINSTALL" -eq 1 ]; then
			# Removing units and a binary needs no identity. Fall back to the
			# defaults so the paths still resolve and say nothing about it.
			LM_USER=''
			LM_UID=''
			LM_GID=''
			LM_GROUP=''
			LM_HOME=''
			return 0
		fi
		lm_warn 'no service identity could be determined.'
		# shellcheck disable=SC2016  # $SUDO_USER is named for the reader, not expanded
		lm_warn '  $SUDO_USER is unset (or is root), which happens when this runs from a root shell'
		lm_warn '  rather than through sudo. Say which account the daemon should run as:'
		lm_warn '    --user <name>        run as an existing account and use its Hugging Face cache'
		lm_warn "    --dedicated-user     create a locked-down $LM_DEDICATED_ACCOUNT system account instead"
		exit 1
	fi

	if ! lm_user_exists "$LM_USER"; then
		if [ "$LM_UNINSTALL" -eq 1 ]; then
			lm_warn "the account $LM_USER no longer exists; removing files anyway"
			LM_UID=''
			LM_GID=''
			LM_GROUP=''
			LM_HOME=''
			return 0
		fi
		lm_die "no such account: $LM_USER"
	fi

	LM_UID=$(id -u "$LM_USER")
	LM_GID=$(id -g "$LM_USER")
	LM_GROUP=$(id -gn "$LM_USER")
	LM_HOME=$(lm_passwd_home "$LM_USER")
	if [ -z "$LM_HOME" ] && [ "$LM_UNINSTALL" -eq 0 ]; then
		lm_die "$LM_USER has no home directory in /etc/passwd"
	fi
}

# lm_dedicated_cache pre-creates the Hugging Face cache the dedicated account
# will use, so the daemon's section 7.2 rule-4 detection finds it already present
# on its first boot. No environment variable is set for it — SPEC section 3.9
# forbids requiring one, and rule 4 infers the topology from $HOME == state_dir
# instead.
lm_dedicated_cache() {
	[ "$LM_DEDICATED" -eq 1 ] || return 0
	lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0750 \
		"$(lm_path "$LM_DEFAULT_STATE_DIR/hf-cache/hub")"
	lm_say "hugging face cache: $LM_DEFAULT_STATE_DIR/hf-cache/hub"
}

# ---------------------------------------------------------------------------
# Topology — everything --user-units moves (D2, DESIGN section 5.2a)
# ---------------------------------------------------------------------------

lm_resolve_topology() {
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		# D72: StateDirectory=llamaman resolves to $XDG_STATE_HOME/llamaman under
		# a user manager, so /var/lib/llamaman is not a constant here and naming
		# it literally would create a directory the daemon never looks at.
		LM_UNIT_DIR=/etc/systemd/user
		if [ -n "$LM_HOME" ]; then
			LM_STATE_DIR="$LM_HOME/.local/state/llamaman"
			[ -n "$LM_PREFIX" ] || LM_PREFIX="$LM_HOME/.local/bin"
		else
			LM_STATE_DIR=''
			[ -n "$LM_PREFIX" ] || LM_PREFIX=$LM_DEFAULT_PREFIX
		fi
	else
		LM_STATE_DIR=$LM_DEFAULT_STATE_DIR
		LM_UNIT_DIR=/etc/systemd/system
		[ -n "$LM_PREFIX" ] || LM_PREFIX=$LM_DEFAULT_PREFIX
	fi
	LM_BIN="$(lm_path "$LM_PREFIX")/llamaman"
}

# ---------------------------------------------------------------------------
# Steps 2 and 3 — resolve, download, verify
# ---------------------------------------------------------------------------

# lm_release_base is D48's whole point: artifacts come through the
# `releases/latest/download/` redirect, never the GitHub API. The anonymous API
# allows 60 requests an hour per IP, which one office behind one NAT exhausts in
# an afternoon and which would make this installer fail intermittently for
# reasons no user could diagnose.
lm_release_base() {
	if [ -n "$LM_VERSION" ]; then
		printf 'https://github.com/%s/releases/download/%s\n' "$LM_REPO" "$LM_VERSION"
	else
		printf 'https://github.com/%s/releases/latest/download\n' "$LM_REPO"
	fi
}

lm_fetch() {
	if [ "$LM_DOWNLOADER" = curl ]; then
		curl -fsSL --retry 3 --connect-timeout 20 -o "$2" "$1"
	else
		wget -q -t 3 -T 20 -O "$2" "$1"
	fi
}

lm_http_get() {
	if [ "$LM_DOWNLOADER" = curl ]; then
		curl -fsS --max-time 2 "$1"
	else
		wget -q -T 2 -O - "$1"
	fi
}

# lm_stage_local is `--version local`: the developer path behind
# `make install-local`, which installs from dist/ instead of from a release.
#
# It is the ONE path where the release signature is not checked, because a local
# build has none — and it says so loudly. That is not the silent skip DESIGN
# section 13 step 3 forbids: the skip is announced on stderr, it is reachable
# only by typing `--version local`, and every other path refuses outright when
# the check cannot be made.
lm_stage_local() {
	lm_dist=${LLAMAMAN_DIST_DIR:-dist}
	[ -x "$lm_dist/llamaman" ] ||
		lm_die "--version local expects a built binary at $lm_dist/llamaman; run make build first"

	lm_warn 'WARNING: --version local installs an unsigned local build.'
	lm_warn '  The ed25519 release signature is NOT verified. Do not use this on a host you care about.'
	cp "$lm_dist/llamaman" "$LM_TMP/llamaman"
	chmod 0755 "$LM_TMP/llamaman"
	LM_RESOLVED_VERSION=local
}

lm_stage_release() {
	lm_base=$(lm_release_base)

	# checksums.txt first, and NOT because verification comes first — because it
	# is the only asset whose name is known before the version is. The tarball
	# names inside it resolve `latest` to a concrete tag with no API call and no
	# HTML redirect to parse.
	lm_say "fetching $LM_CHECKSUMS"
	lm_fetch "$lm_base/$LM_CHECKSUMS" "$LM_TMP/$LM_CHECKSUMS" ||
		lm_die "could not fetch $lm_base/$LM_CHECKSUMS (is ${LM_VERSION:-the latest release} published?)"
	lm_fetch "$lm_base/$LM_CHECKSUMS_SIG" "$LM_TMP/$LM_CHECKSUMS_SIG" ||
		lm_die "could not fetch $lm_base/$LM_CHECKSUMS_SIG; a release without a signature is not installable"

	LM_ASSET=$(awk -v suffix="_linux_${LM_ARCH}.tar.gz" '
		{ n = $2; sub(/^\*/, "", n) }
		n ~ /^llamaman_/ && substr(n, length(n) - length(suffix) + 1) == suffix { print n; exit }
	' "$LM_TMP/$LM_CHECKSUMS")
	[ -n "$LM_ASSET" ] || lm_die "this release publishes no linux/$LM_ARCH tarball"

	# llamaman_v1.2.3_linux_amd64.tar.gz -> v1.2.3
	LM_RESOLVED_VERSION=${LM_ASSET#llamaman_}
	LM_RESOLVED_VERSION=${LM_RESOLVED_VERSION%"_linux_${LM_ARCH}.tar.gz"}

	lm_say "installing llamaman $LM_RESOLVED_VERSION (linux/$LM_ARCH)"
	lm_fetch "$lm_base/$LM_ASSET" "$LM_TMP/$LM_ASSET" || lm_die "could not fetch $lm_base/$LM_ASSET"

	# sha256 in the shell, then the signature in the binary — DESIGN section 13
	# step 3's order. The single-entry file is what keeps the checker from
	# failing over the OTHER architecture's tarball, which this host correctly
	# never downloaded.
	awk -v want="$LM_ASSET" '
		{ n = $2; sub(/^\*/, "", n) }
		n == want { print; found = 1; exit }
		END { exit found ? 0 : 1 }
	' "$LM_TMP/$LM_CHECKSUMS" >"$LM_TMP/want.txt" ||
		lm_die "$LM_CHECKSUMS does not list $LM_ASSET"

	if ! (cd "$LM_TMP" && lm_sha_check want.txt >/dev/null 2>&1); then
		lm_die "$LM_ASSET failed its sha256 check; the download is corrupt or was tampered with"
	fi
	lm_say 'sha256 ok'

	# Hardened enough for a tarball we publish ourselves: refuse absolute paths
	# and traversal rather than trusting tar's own handling of them.
	if tar -tzf "$LM_TMP/$LM_ASSET" | grep -Eq '(^/|(^|/)\.\.(/|$))'; then
		lm_die "$LM_ASSET contains absolute or traversing paths; refusing to extract it"
	fi
	tar -xzf "$LM_TMP/$LM_ASSET" -C "$LM_TMP"
	[ -x "$LM_TMP/llamaman" ] || lm_die "$LM_ASSET does not contain an executable llamaman"

	# The ed25519 check, performed by the extracted binary itself. No gpg, no
	# minisign, no keyring — the trust root is compiled in (DESIGN sections 13
	# step 3 and 16.2 step 3), so this works on a host with nothing installed and
	# there is never a silent skip. --require names the tarball this host
	# actually downloaded, so a checksums file that does not cover it is a
	# refusal rather than a vacuous pass.
	lm_say 'verifying the release signature'
	"$LM_TMP/llamaman" verify-release --require "$LM_ASSET" "$LM_TMP" ||
		lm_die 'the release signature did not verify; nothing has been installed'
}

# ---------------------------------------------------------------------------
# Step 5 — the binary
# ---------------------------------------------------------------------------

lm_file_size() { wc -c <"$1" | tr -d ' '; }

lm_mode() {
	if command -v stat >/dev/null 2>&1; then
		stat -c '%a' "$1" 2>/dev/null || printf '755\n'
	else
		printf '755\n'
	fi
}

lm_install_binary() {
	lm_prefix_real=$(lm_path "$LM_PREFIX")

	# Created FIRST, and owned by the identity under --user-units, because
	# ~<user>/.local/bin routinely does not exist on a fresh account and the
	# install in the next breath would otherwise fail before the state tree was
	# ever created (DESIGN section 13 step 5).
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0755 "$(lm_path "$LM_HOME/.local")"
		lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0755 "$lm_prefix_real"
	else
		lm_do install -d -m 0755 "$lm_prefix_real"
	fi

	# Room for three copies: the installed binary, the retained previous one
	# (<prefix>/llamaman.prev) and the swap actor's llamaman.new.tmp all live
	# here (D89) — the retained binary is deliberately NOT under the state
	# directory, because every install must be a rename inside one filesystem.
	lm_bin_kb=$(($(lm_file_size "$LM_TMP/llamaman") / 1024 + 1))
	lm_check_space "$lm_prefix_real" "$((lm_bin_kb * 3))"

	# A group-writable <prefix> is a warning, not a refusal: `root:staff 2775
	# /usr/local/bin` is the common case on developer machines, the hazard
	# predates llamaman (anyone who can write llamaman.prev there can already
	# write llamaman), and `llamaman doctor` reports the same fact.
	case $(lm_mode "$lm_prefix_real") in
	*[2367]?) lm_warn "$LM_PREFIX is group-writable; anyone in its group can replace the llamaman binary" ;;
	esac

	if [ -x "$LM_BIN" ]; then
		LM_UPGRADE=1
	fi

	# Atomic, and safe while the old binary is running: write beside the target
	# and rename over it. An `install` straight onto a running executable is
	# ETXTBSY at best and a torn read at worst.
	lm_stage="$lm_prefix_real/.llamaman.install.$$"
	lm_do install -m 0755 "$LM_TMP/llamaman" "$lm_stage"
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		# The unprivileged daemon performs its own self-update here by renaming
		# over this very path (DESIGN section 5.2a item 2), so a root-owned
		# binary would be one it could never replace. D15's rationale — a
		# service-user-writable file on root's PATH is an escalation trap — does
		# not apply: this is not on root's PATH.
		lm_do chown "$LM_USER:$LM_GROUP" "$lm_stage"
	else
		lm_do chown 0:0 "$lm_stage"
	fi
	lm_do mv -f "$lm_stage" "$LM_BIN"
	lm_say "installed $LM_PREFIX/llamaman"

	# Under --dry-run the binary was never written, so the subcommands the rest
	# of this script runs have to come from the verified copy in the temporary
	# directory instead.
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		LM_TOOL="$LM_TMP/llamaman"
	else
		LM_TOOL=$LM_BIN
	fi
}

# ---------------------------------------------------------------------------
# Step 6 — the state directory
# ---------------------------------------------------------------------------

lm_create_state_dir() {
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0755 "$(lm_path "$LM_HOME/.local")"
		lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0755 "$(lm_path "$LM_HOME/.local/state")"
	fi

	lm_check_space "$(lm_path "$LM_STATE_DIR")" "$LM_MIN_FREE_KB"

	lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0750 "$(lm_path "$LM_STATE_DIR")"
	for lm_child in $LM_STATE_CHILDREN; do
		lm_do install -d -o "$LM_USER" -g "$LM_GROUP" -m 0750 "$(lm_path "$LM_STATE_DIR/$lm_child")"
	done
	lm_say "state directory: $LM_STATE_DIR"
}

# ---------------------------------------------------------------------------
# Step 7 — units and polkit
# ---------------------------------------------------------------------------

# THE BINARY WRITES THE UNIT AND POLKIT CONTENT, NOT THIS SCRIPT (D48): one
# source of truth, testable in Go, and the same command is the F16 repair path.
# Every unit it writes carries a `# llamaman-units: <N>` stamp naming the
# template version, which is what lets section 5.4a tell a hand-edit from a host
# whose units simply predate the binary now running (D95).
#
# It also adds the identity to the systemd-journal group (D77) — the one place
# that grant belongs, since it is a root-only idempotent change the F16 repair
# must re-apply — and it never touches the database or anything else under the
# state directory (section 11.3).
lm_install_units() {
	set -- --identity "$LM_USER" --prefix "$LM_PREFIX"
	[ -z "$LM_PORT" ] || set -- "$@" --port "$LM_PORT"
	[ "$LM_USER_UNITS" -eq 0 ] || set -- "$@" --user-units
	[ "$LM_NO_AUTOSTART_GRANT" -eq 0 ] || set -- "$@" --no-autostart-grant
	[ "$LM_REPAIR_POLKIT" -eq 0 ] || set -- "$@" --repair-polkit
	[ "$LM_DRY_RUN" -eq 0 ] || set -- "$@" --dry-run
	[ -z "$LM_ROOT" ] || set -- "$@" --root "$LM_ROOT"

	"$LM_TOOL" install-units "$@" || lm_die 'install-units failed; the units were not written'
}

# ---------------------------------------------------------------------------
# The two systemd managers (DESIGN section 5.2a item 3)
# ---------------------------------------------------------------------------

# lm_user_systemctl runs systemctl inside the identity's OWN manager. Root's own
# `systemctl --user` would talk to root's manager and silently do nothing useful,
# which is the failure this function exists to prevent.
lm_user_systemctl() {
	if command -v runuser >/dev/null 2>&1; then
		runuser -u "$LM_USER" -- env \
			XDG_RUNTIME_DIR="/run/user/$LM_UID" \
			DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$LM_UID/bus" \
			systemctl --user "$@"
	elif command -v setpriv >/dev/null 2>&1; then
		setpriv --reuid "$LM_UID" --regid "$LM_GID" --clear-groups env \
			XDG_RUNTIME_DIR="/run/user/$LM_UID" \
			DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$LM_UID/bus" \
			systemctl --user "$@"
	elif command -v su >/dev/null 2>&1; then
		su -s /bin/sh "$LM_USER" -c \
			"XDG_RUNTIME_DIR=/run/user/$LM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$LM_UID/bus systemctl --user $*"
	else
		lm_die 'none of runuser, setpriv or su is installed, so a user unit cannot be driven for another account'
	fi
}

# lm_scoped_systemctl is the one place the topology chooses a manager. Every
# start, stop, enable, disable and reload below goes through it, so no call site
# can forget that the two managers are different programs.
lm_scoped_systemctl() {
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_user_systemctl "$@"
	else
		systemctl "$@"
	fi
}

lm_now() { date +%s; }

# lm_wait_for_bus polls for the user bus. `loginctl enable-linger` starts the
# manager asynchronously, so the runuser calls that follow would otherwise race a
# socket that is about to exist.
lm_wait_for_bus() {
	lm_bus_deadline=$(($(lm_now) + 10))
	while [ "$(lm_now)" -lt "$lm_bus_deadline" ]; do
		if [ -S "$(lm_path "/run/user/$LM_UID/bus")" ]; then
			return 0
		fi
		sleep 1
	done
	lm_warn "/run/user/$LM_UID/bus did not appear within 10 s; the user manager may not have started"
	return 1
}

lm_reload_manager() {
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		lm_say '[dry-run] daemon-reload'
		return 0
	fi
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		command -v loginctl >/dev/null 2>&1 ||
			lm_die 'loginctl is not installed, so --user-units cannot enable linger'
		loginctl enable-linger "$LM_USER" || lm_warn 'loginctl enable-linger failed'
		lm_wait_for_bus || true
	fi
	lm_scoped_systemctl daemon-reload || lm_warn 'daemon-reload failed'
}

# ---------------------------------------------------------------------------
# Steps 8, 9, 10, 11
# ---------------------------------------------------------------------------

# Detection logic is never reimplemented in shell: `doctor` is the one place it
# lives. At this point the database does NOT exist, and that is a normal outcome
# — doctor reports its DB-dependent checks as skipped, opens nothing, and so
# cannot leave a root-owned llamaman.db or a -wal/-shm sidecar behind (DESIGN
# sections 11.3 and 13 step 8). No package manager is ever invoked.
lm_report_toolchain() {
	printf '\n'
	"$LM_TOOL" doctor --format=text || lm_warn 'doctor reported problems; see above'
	printf '\n'
}

lm_start_hint() {
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_say '  start it with: systemctl --user enable --now llamaman-instances.target llamaman.service'
	else
		lm_say '  start it with: sudo systemctl enable --now llamaman-instances.target llamaman.service'
	fi
}

# lm_start_service is step 9, IN THE SCOPE THIS INSTALL ACTUALLY USED. An
# unconditional system-scope command would enable nothing at all on a
# --user-units host.
lm_start_service() {
	if [ "$LM_NO_START" -eq 1 ]; then
		lm_say 'not starting anything (--no-start)'
		lm_start_hint
		return 0
	fi
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		lm_say '[dry-run] enable --now llamaman-instances.target llamaman.service'
		return 0
	fi
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_wait_for_bus || true
	fi
	lm_scoped_systemctl enable --now llamaman-instances.target llamaman.service ||
		lm_warn 'could not enable llamaman.service'
}

# lm_restart_service is step 11's upgrade path. INSTANCE UNITS ARE NEVER
# TOUCHED, so an upgrade does not interrupt inference; the public gateway ports
# survive the restart through the fd store (DESIGN section 9.4).
lm_restart_service() {
	if [ "$LM_NO_START" -eq 1 ]; then
		# This is what makes the command usable as step 2 of section 12.4's
		# downgrade procedure: the older binary has to be on disk before the
		# database is restored, and it must not be started in between, because
		# until then it would refuse the schema it finds and crash-loop.
		lm_say 'not restarting llamaman.service (--no-start)'
		return 0
	fi
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		lm_say '[dry-run] restart llamaman.service'
		return 0
	fi
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_wait_for_bus || true
	fi
	lm_scoped_systemctl restart llamaman.service || lm_warn 'could not restart llamaman.service'
	lm_say 'restarted llamaman.service; running instances were not touched'
}

# lm_wait_for_daemon is step 9's poll. The port range FOLLOWS the requested port
# rather than being hard-coded, and it is the same +20 walk the daemon performs
# in section 11.1 step 7 — so `--port 9000` polls 9000-9020 and prints a correct
# URL. The first port answering /api/v1/meta wins, and the returned ui_port is
# cross-checked so an unrelated service on that port cannot be mistaken for us.
lm_wait_for_daemon() {
	LM_ACTUAL_PORT=''
	if [ "$LM_NO_START" -eq 1 ] || [ "$LM_DRY_RUN" -eq 1 ]; then
		return 1
	fi

	lm_base_port=${LM_PORT:-$LM_DEFAULT_PORT}
	lm_last_port=$((lm_base_port + 20))
	lm_wait_deadline=$(($(lm_now) + 30))
	while [ "$(lm_now)" -lt "$lm_wait_deadline" ]; do
		lm_p=$lm_base_port
		while [ "$lm_p" -le "$lm_last_port" ]; do
			lm_meta=$(lm_http_get "http://127.0.0.1:$lm_p/api/v1/meta" 2>/dev/null || true)
			case $lm_meta in
			*'"ui_port"'*)
				LM_ACTUAL_PORT=$lm_p
				return 0
				;;
			esac
			lm_p=$((lm_p + 1))
		done
		sleep 1
	done
	return 1
}

# lm_print_setup is step 10, and it is deliberately a plain `llamaman status`
# with its Setup block echoed verbatim. NO --json, AND THEREFORE NO jq: step 1's
# preconditions require only curl or wget, and hand-parsing JSON in POSIX sh is
# exactly the fragility this script avoids everywhere else. The plain-text block
# already carries both the URL and the token, which by then is a plain file read
# of <state_dir>/setup-token (section 2.2a) — the daemon minted it during its own
# boot, wrote it to disk and logged it to journald, so this neither scrapes
# journald nor asks the database for something the database does not hold.
#
# It runs AFTER the daemon started, so the database and its WAL sidecars already
# exist owned by the service identity and root's read-only open creates nothing
# (section 11.3).
lm_print_setup() {
	if [ "$LM_DRY_RUN" -eq 1 ] || [ "$LM_NO_START" -eq 1 ]; then
		return 0
	fi

	lm_status_out=$("$LM_TOOL" status 2>/dev/null || true)
	if [ -z "$lm_status_out" ]; then
		lm_warn 'llamaman status printed nothing; the daemon may not have finished starting'
		return 0
	fi

	lm_url=''
	if [ -n "$LM_ACTUAL_PORT" ]; then
		lm_url="http://127.0.0.1:$LM_ACTUAL_PORT"
	fi

	printf '\n'
	case $lm_status_out in
	*'Setup         complete'* | *'run as root or'*)
		# Already claimed, or the token file is not readable by this uid. Print
		# the URL alone and say so, rather than a Setup block promising a token
		# it cannot show.
		[ -z "$lm_url" ] || lm_say "open $lm_url"
		lm_say 'the one-time setup token was already used; sign in with the admin password'
		;;
	*)
		printf '%s\n' "$lm_status_out" | sed -n '/^  Setup /,$p'
		;;
	esac
	printf '\n'
}

lm_print_next_steps() {
	if [ -n "$LM_ACTUAL_PORT" ]; then
		lm_say "llamaman is running: http://127.0.0.1:$LM_ACTUAL_PORT"
		return 0
	fi
	if [ "$LM_NO_START" -eq 1 ] || [ "$LM_DRY_RUN" -eq 1 ]; then
		return 0
	fi

	# A URL this script could not verify is worse than none: it sends the
	# operator to a browser tab instead of to the journal that has the answer.
	lm_warn 'the daemon did not answer within 30 s.'
	if [ "$LM_USER_UNITS" -eq 1 ]; then
		lm_warn '  journalctl --user -u llamaman -n 50'
	else
		lm_warn '  journalctl -u llamaman -n 50'
	fi
}

# ---------------------------------------------------------------------------
# Step 12 — uninstall
# ---------------------------------------------------------------------------

lm_instance_units() {
	lm_scoped_systemctl list-units --no-legend --no-pager --all --plain \
		'llamaman-instance@*.service' 2>/dev/null |
		awk '{print $1}' | grep '^llamaman-instance@' || true
}

lm_uninstall_stop() {
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		lm_say '[dry-run] stop and disable llamaman.service, llamaman-instances.target and every instance unit'
		return 0
	fi

	lm_scoped_systemctl disable --now llamaman.service llamaman-instances.target 2>/dev/null || true
	for lm_unit in $(lm_instance_units); do
		lm_scoped_systemctl stop "$lm_unit" 2>/dev/null || true
	done
}

lm_uninstall_files() {
	for lm_unit in $LM_UNITS; do
		lm_target=$(lm_path "$LM_UNIT_DIR/$lm_unit")
		if [ -e "$lm_target" ]; then
			lm_do rm -f "$lm_target"
			lm_say "removed $LM_UNIT_DIR/$lm_unit"
		fi
	done

	for lm_pk in "$LM_POLKIT_RULES" "$LM_POLKIT_PKLA"; do
		lm_target=$(lm_path "$lm_pk")
		if [ -e "$lm_target" ]; then
			lm_do rm -f "$lm_target"
			lm_say "removed $lm_pk"
		fi
	done

	# <prefix>/llamaman.prev is OURS and lives in a directory that is not
	# (DESIGN section 6.1, D89), so leaving it behind would leave a stray
	# root-owned file on root's PATH. llamaman.new.tmp is the swap actor's
	# transient and goes for the same reason.
	for lm_f in llamaman llamaman.prev llamaman.new.tmp; do
		lm_target="$(lm_path "$LM_PREFIX")/$lm_f"
		if [ -e "$lm_target" ]; then
			lm_do rm -f "$lm_target"
			lm_say "removed $LM_PREFIX/$lm_f"
		fi
	done
}

lm_uninstall_reload() {
	if [ "$LM_DRY_RUN" -eq 1 ]; then
		lm_say '[dry-run] daemon-reload'
		return 0
	fi
	lm_scoped_systemctl daemon-reload || lm_warn 'daemon-reload failed'
	if [ "$LM_USER_UNITS" -eq 1 ] && command -v loginctl >/dev/null 2>&1; then
		# The mirror of section 5.2a item 3: linger was enabled for these units,
		# so it goes away with them.
		loginctl disable-linger "$LM_USER" 2>/dev/null || true
	fi
}

# lm_uninstall_state is the one place this script can destroy something the user
# cares about, so it is the one place with a second consent gate.
lm_uninstall_state() {
	if [ -z "$LM_STATE_DIR" ]; then
		lm_warn 'the state directory could not be resolved (the service account is gone); nothing was removed'
		return 0
	fi
	lm_state_real=$(lm_path "$LM_STATE_DIR")

	if [ "$LM_PURGE" -eq 0 ]; then
		lm_say "kept $LM_STATE_DIR (the database, downloads and instance configuration)"
		lm_say '  remove it with: --uninstall --purge'
		return 0
	fi

	# It NEVER touches the Hugging Face cache: those models belong to the user,
	# not to us. The one exception is a --dedicated-user install, where the cache
	# lives UNDER the state path and an ordinary --purge would sweep it up — so
	# that case requires a second explicit consent, and prints what it is about
	# to delete first.
	lm_cache="$lm_state_real/hf-cache"
	if [ -d "$lm_cache" ] && [ "$LM_PURGE_MODELS" -eq 0 ]; then
		lm_size=$(du -sh "$lm_cache" 2>/dev/null | awk '{print $1}')
		lm_warn "$LM_STATE_DIR/hf-cache holds ${lm_size:-an unknown amount} of downloaded models."
		lm_warn '  That is your model library, not llamaman data, so --purge will not remove it.'
		lm_warn '  Re-run with --purge-models as well to delete it, or move it somewhere else first.'
		lm_warn "  Nothing under $LM_STATE_DIR was removed."
		return 0
	fi

	if [ -d "$lm_state_real" ]; then
		lm_do rm -rf "$lm_state_real"
		lm_say "removed $LM_STATE_DIR"
	fi
}

lm_uninstall() {
	lm_at "$LINENO" 'stopping the units'
	lm_uninstall_stop

	lm_at "$LINENO" 'removing the units, the polkit files and the binary'
	lm_uninstall_files

	lm_at "$LINENO" 'reloading the service manager'
	lm_uninstall_reload

	lm_at "$LINENO" 'deciding what to do with the state directory'
	lm_uninstall_state

	lm_say 'uninstalled'
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------

main() {
	trap 'lm_exit_trap $?' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM

	lm_at "$LINENO" 'parsing arguments'
	lm_parse_args "$@"
	lm_validate_args

	lm_at "$LINENO" 'checking preconditions'
	lm_require_root "$@"
	lm_require_systemd

	lm_at "$LINENO" 'resolving the service identity'
	lm_resolve_identity
	lm_resolve_topology

	if [ "$LM_UNINSTALL" -eq 1 ]; then
		lm_uninstall
		exit 0
	fi

	lm_detect_arch
	lm_detect_tools

	lm_at "$LINENO" 'preparing a temporary directory'
	LM_TMP=$(mktemp -d 2>/dev/null || mktemp -d -t llamaman.XXXXXX)
	[ -n "$LM_TMP" ] || lm_die 'could not create a temporary directory'
	chmod 0700 "$LM_TMP"

	lm_at "$LINENO" 'downloading and verifying the release'
	if [ "$LM_VERSION" = local ]; then
		lm_stage_local
	else
		lm_stage_release
	fi

	lm_at "$LINENO" 'installing the binary'
	lm_install_binary

	lm_at "$LINENO" 'creating the state directory'
	lm_create_state_dir
	lm_dedicated_cache

	lm_at "$LINENO" 'writing the units and polkit rules'
	lm_install_units
	lm_reload_manager

	lm_at "$LINENO" 'reporting the toolchain'
	lm_report_toolchain

	# Step 11's upgrade path and step 9's first start are one decision made once:
	# a host that already had a binary is restarted, a fresh one is enabled.
	lm_at "$LINENO" 'starting the daemon'
	if [ "$LM_UPGRADE" -eq 1 ]; then
		lm_restart_service
	else
		lm_start_service
	fi

	lm_at "$LINENO" 'waiting for the daemon'
	lm_wait_for_daemon || true

	lm_at "$LINENO" 'printing the setup token'
	lm_print_setup
	lm_print_next_steps

	exit 0
}

main "$@"
