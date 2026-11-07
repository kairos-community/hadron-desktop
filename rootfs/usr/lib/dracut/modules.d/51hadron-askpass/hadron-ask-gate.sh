#!/bin/sh
# ExecStartPre of systemd-ask-password-console.service in the initramfs.
#
# systemd starts the console agent when a question appears in
# /run/systemd/ask-password. Two agents answering the same question would put
# two prompts on one screen, so exactly one of them asks:
#
#   - kairos-splash has a framebuffer and the keyboard: it wrote its PID to
#     /run/hadron-ask/splash-agent and draws the prompt. This gate waits, and
#     the console agent starts only after the splash is gone (switch-root,
#     emergency, a crash). A question still open then is asked on the console.
#   - kairos-splash has no framebuffer, did not start, or was told not to
#     (hadron.splash.agent=0): no PID file, or a dead PID. The gate stops the
#     splash, which puts tty1 back in text mode, and the console agent asks.
#
# The splash unit is Type=notify and sends READY=1 only after that decision,
# and this unit is After= it, so the PID file is never looked at too early.

pidfile=/run/hadron-ask/splash-agent

# switch-root stops this unit while the gate is still waiting. That is the
# normal end of a boot where the splash answered, not a failure.
trap 'exit 0' TERM

splash_answers() {
    [ -r "$pidfile" ] || return 1
    read -r pid < "$pidfile" || return 1
    [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null
}

if splash_answers; then
    echo "hadron-ask-gate: kairos-splash (PID $pid) answers password questions"
    while splash_answers; do
        sleep 1
    done
    echo "hadron-ask-gate: kairos-splash is gone, the console agent takes over"
fi

if systemctl -q is-active kairos-splash.service 2>/dev/null; then
    systemctl stop kairos-splash.service
fi
exit 0
