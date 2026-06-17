#!/bin/sh
# schedule-agent-retry.sh is invoked by viam-server-detached.service as an
# ExecStartPre= command. Each time we enter detached mode it schedules a future
# attempt to *leave* detached mode by starting viam-agent again -- which, via
# Conflicts=viam-agent.service, stops this detached service. If the agent is
# healthy again (e.g. the original failure was transient or a false positive) it
# stays up and we have recovered automatically; if it is still "bad" it triggers
# OnFailure= back into detached mode and the next, longer-delayed retry is scheduled.
#
# The delay grows with each consecutive attempt (exponential-ish backoff, capped)
# so a genuinely bad agent binary settles into a near-stable detached state
# instead of flapping viam-server, while a transient/false-positive failure still
# recovers quickly.
#
# This runs on every (re)start of viam-server-detached, INCLUDING viam-server
# crash-restarts within the same detached episode (Restart=always). To avoid
# bumping the backoff or stacking timers on those restarts, it is a no-op while a
# retry is already pending. The attempt counter decays with time: if the previous
# attempt was longer ago than RESET_AFTER (i.e. the agent recovered and ran for a
# good while), the next failure starts the backoff over from the beginning.

set -u

RETRY_NAME=viam-agent-retry
RETRY_TIMER="${RETRY_NAME}.timer"
STATE_FILE=/opt/viam/tmp/detached-retry-state # line 1: attempt count, line 2: epoch of last attempt
RESET_AFTER=43200                             # 12h; longer than the max backoff so flapping keeps escalating

# Already waiting to retry? Then this is a viam-server restart within the same
# detached episode; leave the existing schedule and counter untouched.
if [ "$(systemctl is-active "$RETRY_TIMER" 2>/dev/null)" = "active" ]; then
	exit 0
fi

# Drop any spent transient unit from a previous attempt so we can reuse the name.
systemctl reset-failed "$RETRY_TIMER" "${RETRY_NAME}.service" 2>/dev/null || true

now=$(date +%s)
count=0
last=0
if [ -r "$STATE_FILE" ]; then
	count=$(sed -n 1p "$STATE_FILE")
	last=$(sed -n 2p "$STATE_FILE")
fi
case "$count" in '' | *[!0-9]*) count=0 ;; esac
case "$last" in '' | *[!0-9]*) last=0 ;; esac

# Fresh episode if the previous attempt was long ago (agent recovered in between).
if [ "$((now - last))" -gt "$RESET_AFTER" ]; then
	count=0
fi
count=$((count + 1))

mkdir -p "$(dirname "$STATE_FILE")"
printf '%s\n%s\n' "$count" "$now" >"$STATE_FILE"

# Backoff schedule in seconds, capped at 6h.
case "$count" in
1) delay=60 ;;
2) delay=120 ;;
3) delay=300 ;;
4) delay=900 ;;
5) delay=1800 ;;
6) delay=3600 ;;
*) delay=21600 ;;
esac

echo "detached mode: scheduling viam-agent retry #${count} in ${delay}s"
# Transient one-shot timer that starts viam-agent (which Conflicts= us away).
exec systemd-run \
	--unit="$RETRY_NAME" \
	--on-active="$delay" \
	--timer-property=AccuracySec=10s \
	systemctl start viam-agent.service
