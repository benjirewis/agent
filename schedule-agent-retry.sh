#!/bin/sh
# schedule-agent-retry.sh is invoked by viam-server-detached.service as an
# ExecStartPre= command. Each time we enter detached mode it schedules a future
# attempt to *leave* detached mode by starting viam-agent again -- which, via
# Conflicts=viam-agent.service, stops this detached service. If the agent is
# healthy again (e.g. the original failure was transient or a false positive) it
# stays up and we have recovered automatically; if it is still "bad" it triggers
# OnFailure= back into detached mode and another retry is scheduled.
#
# This runs on every (re)start of viam-server-detached, INCLUDING viam-server
# crash-restarts within the same detached episode (Restart=always). To avoid
# stacking timers on those restarts, it is a no-op while a retry is already pending.

set -u

RETRY_NAME=viam-agent-retry
RETRY_TIMER="${RETRY_NAME}.timer"
DELAY=180

# Already waiting to retry? Then this is a viam-server restart within the same
# detached episode; leave the existing schedule untouched.
if [ "$(systemctl is-active "$RETRY_TIMER" 2>/dev/null)" = "active" ]; then
    exit 0
fi

# Drop any spent transient unit from a previous attempt so we can reuse the name.
systemctl reset-failed "$RETRY_TIMER" "${RETRY_NAME}.service" 2>/dev/null || true

echo "detached mode: scheduling viam-agent retry in ${DELAY}s"
# Transient one-shot timer that starts viam-agent (which Conflicts= us away).
exec systemd-run \
    --unit="$RETRY_NAME" \
    --on-active="$DELAY" \
    --timer-property=AccuracySec=10s \
    systemctl start viam-agent.service
