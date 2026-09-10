#!/bin/sh
# Verify a sercond deployment on a jump host.
#
# Run this from the machine you will actually use as a client. It copies the
# Linux binary to the target, walks the whole chain, and prints a pass/fail
# summary. It is also the thing to run when something stops working: every step
# is a smaller version of the one an operator would otherwise debug by hand.
#
#   sh hack/verify-deployment.sh user@host [path/to/sercond-linux-amd64]
#
# The binary is placed in a temporary directory, not in PATH, and removed on
# exit unless KEEP=1 is set.

set -u

TARGET="${1:-}"
BIN="${2:-dist/sercond-linux-amd64}"
REMOTE_DIR="${REMOTE_DIR:-/tmp/sercond-verify}"
KEEP="${KEEP:-0}"

if [ -z "$TARGET" ]; then
	echo "usage: sh hack/verify-deployment.sh user@host [binary]" >&2
	exit 2
fi
if [ ! -f "$BIN" ]; then
	echo "verify: $BIN not found (build it first: make build)" >&2
	exit 2
fi

PASS=0
FAIL=0

ok()   { PASS=$((PASS + 1)); printf '  \033[32mPASS\033[0m  %s\n' "$1"; }
bad()  { FAIL=$((FAIL + 1)); printf '  \033[31mFAIL\033[0m  %s\n' "$1"; }
note() { printf '        %s\n' "$1"; }
step() { printf '\n%s\n' "$1"; }

# ssh_run runs a command on the target with the remote binary first on PATH.
ssh_run() {
	ssh -o BatchMode=yes -o ConnectTimeout=8 "$TARGET" \
		"PATH=$REMOTE_DIR:\$PATH; export PATH; $1" 2>&1
}

cleanup() {
	if [ "$KEEP" = "0" ]; then
		ssh -o BatchMode=yes -o ConnectTimeout=8 "$TARGET" "rm -rf $REMOTE_DIR" >/dev/null 2>&1
	fi
}
trap cleanup EXIT INT TERM

printf 'verifying sercond on %s\n' "$TARGET"
printf 'binary: %s (%s)\n' "$BIN" "$(wc -c <"$BIN" | tr -d ' ') bytes"

step "1. transport"
if out=$(ssh_run 'uname -s -m; id -un' | tr '\n' ' '); then
	ok "ssh works: $out"
else
	bad "ssh failed: $out"
	echo
	echo "cannot continue without a working ssh connection"
	exit 1
fi

if ssh_run 'command -v setsid >/dev/null 2>&1' | grep -q .; then
	note "setsid present (needed for the daemon to outlive this session)"
fi

step "2. deploy"
if ssh -o BatchMode=yes -o ConnectTimeout=8 "$TARGET" "mkdir -p $REMOTE_DIR" 2>&1; then
	ok "created $REMOTE_DIR"
else
	bad "cannot create $REMOTE_DIR"
	exit 1
fi

if scp -q -o BatchMode=yes -o ConnectTimeout=8 "$BIN" "$TARGET:$REMOTE_DIR/sercond" 2>&1; then
	ok "copied binary"
else
	bad "scp failed"
	exit 1
fi

if ssh_run "chmod +x $REMOTE_DIR/sercond" 2>&1; then
	ok "marked executable"
fi

step "3. binary runs on the target"
out=$(ssh_run 'sercond version')
case "$out" in
*protocol*) ok "$out" ;;
*)
	bad "unexpected output: $out"
	note "a noexec mount or a wrong architecture shows up here"
	exit 1
	;;
esac

step "4. port enumeration"
out=$(ssh_run 'sercond list --json')
if echo "$out" | grep -q '^\['; then
	n=$(echo "$out" | grep -c '"ref"')
	ok "enumerated $n port(s)"
	echo "$out" | grep '"ref"' | sed 's/^ */        /'
else
	bad "list did not return JSON: $out"
fi

step "5. daemon outlives the ssh session"
# This is the step the whole design rests on. "session" ensures a daemon exists
# and then pipes stdio; with stdin closed it exits immediately, so if the daemon
# is still running afterwards it genuinely detached rather than being carried by
# the session that started it.
ssh_run 'sercond session < /dev/null' >/dev/null 2>&1

sleep 2
out=$(ssh "$TARGET" "PATH=$REMOTE_DIR:\$PATH; export PATH; sercond status" 2>&1)
case "$out" in
*"not running"*)
	bad "daemon did not survive"
	note "$out"
	note "on Linux this means setsid did not take effect; check that the binary"
	note "was not started inside a wrapper that re-parents it"
	;;
*"daemon   running"*)
	ok "daemon survived the session that created it"
	;;
*)
	bad "unexpected status output: $out"
	;;
esac

step "6. transient socket path"
# The socket lives in a per-user runtime directory. If the target is a shared
# machine, two users must not collide.
out=$(ssh_run 'ls -l "$XDG_RUNTIME_DIR/sercon/run/" 2>/dev/null || ls -l ~/.cache/sercon/run/ 2>/dev/null')
if echo "$out" | grep -q 's\.sock'; then
	ok "socket present"
	note "$out"
else
	bad "no socket found: $out"
fi

step "7. logs are being written"
out=$(ssh_run 'find ~/.local/state/sercon/ports -name "*.log" 2>/dev/null | head -5')
if [ -n "$out" ]; then
	ok "log files exist"
	echo "$out" | sed 's/^ */        /'
	first=$(echo "$out" | head -1)
	ssh_run "head -2 '$first'" | sed 's/^ */        /'
else
	bad "no log files"
	note "a daemon with no ports attached writes nothing, which is expected"
	note "on a machine with no serial adapters"
fi

step "8. audit trail"
out=$(ssh_run 'tail -5 ~/.local/state/sercon/audit/*.jsonl 2>/dev/null')
if [ -n "$out" ]; then
	ok "audit records present"
	echo "$out" | sed 's/^ */        /'
else
	bad "no audit records"
fi

step "9. shutdown"
out=$(ssh_run 'sercond stop')
case "$out" in
*stopped*) ok "$out" ;;
*) bad "stop did not confirm: $out" ;;
esac

printf '\n----------------------------------------\n'
printf '%d passed, %d failed\n' "$PASS" "$FAIL"
[ "$KEEP" = "0" ] && printf 'cleaned up %s on the target\n' "$REMOTE_DIR"
[ "$FAIL" -eq 0 ] || exit 1
