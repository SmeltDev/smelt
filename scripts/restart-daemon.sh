#!/usr/bin/env bash
# Restart the smelt daemon so the next smelt command starts it from the freshly
# built binary. A running daemon keeps the code it started with.
#
# Without --force the daemon is drained: it takes no new factory runs, lets the
# active ones finish, then exits (`smelt daemon restart --when-idle`). With
# --force it is stopped at once, which cancels active runs
# (`smelt daemon stop --force`).
#
# A smelt without the `daemon restart` command, or a daemon too old to drain,
# falls back to the logic below: stop only when no factory run is active
# (otherwise leave it running and print a hint, exiting 0), or always with --force.
set -euo pipefail

force=false
[ "${1:-}" = "--force" ] && force=true

# Prefer the binary just built, then the one on PATH.
here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
smelt=""
if [ -x "${here}/dist/smelt" ]; then
	smelt="${here}/dist/smelt"
elif command -v smelt >/dev/null 2>&1; then
	smelt="$(command -v smelt)"
fi

if [ -n "${smelt}" ] && "${smelt}" daemon restart --help 2>&1 | grep -q -- '--when-idle'; then
	if [ "${force}" = true ]; then
		exec "${smelt}" daemon stop --force
	fi
	# A daemon too old to drain falls through to the fallback below, so a build
	# never fails because of it; any other failure is the command's own.
	status=0
	out=$("${smelt}" daemon restart --when-idle 2>&1) || status=$?
	if [ "${status}" -eq 0 ]; then
		printf '%s\n' "${out}"
		exit 0
	fi
	if ! printf '%s' "${out}" | grep -q 'too old to drain'; then
		printf '%s\n' "${out}" >&2
		exit "${status}"
	fi
fi

dir="${SMELT_DAEMON_DIR:-${HOME}/.smelt/daemon}"
sock="${dir}/daemon.sock"
pidfile="${dir}/daemon.pid"

if ! pgrep -f 'smelt _daemon' >/dev/null 2>&1; then
	exit 0 # not running: nothing to restart
fi

if [ "${force}" = false ]; then
	# Stopping the daemon cancels its runs, so only stop it when it says, in
	# time, that none is running. No answer, or one that can't be read, means
	# "don't know", and the daemon is left alone.
	active=unknown
	if [ -S "${sock}" ] && command -v curl >/dev/null && command -v python3 >/dev/null; then
		active=$(curl -s --max-time 10 --unix-socket "${sock}" http://smeltd/factory 2>/dev/null |
			python3 -c 'import json,sys
try: print(sum(1 for s in json.load(sys.stdin).get("sessions") or [] if s.get("state") == "running"))
except Exception: print("unknown")')
	fi
	if [ "${active}" != 0 ]; then
		if [ "${active}" = unknown ]; then
			echo "smelt daemon left running: could not tell whether a factory run is active." >&2
		else
			echo "smelt daemon left running: ${active} factory run(s) active." >&2
		fi
		echo "It still runs the old binary; restart it when it's idle with: task daemon:restart" >&2
		exit 0
	fi
fi

if [ -f "${pidfile}" ] && kill "$(cat "${pidfile}")" 2>/dev/null; then
	:
else
	pkill -f 'smelt _daemon' || true
fi
echo "smelt daemon stopped; the next smelt command starts it from the new build." >&2
