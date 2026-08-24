#!/bin/sh -e
# gateway-entrypoint -- ENTRYPOINT of the image; hands over to s6-overlay.
#
# WHY POSIX sh AND NOT #!/command/with-contenv bash:
# At ENTRYPOINT time s6-overlay's preinit has NOT run yet, so PATH is still
# Docker's default (/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin)
# WITHOUT /command. `with-contenv` is an execlineb script whose first command
# is the bare name `ifelse`; execlineb resolves it via PATH -> not found ->
# "execlineb: fatal: unable to exec ifelse: No such file or directory" and the
# container crash-loops (~1/s). Upstream /init avoids this by being #!/bin/sh
# and adding /bin,/usr/bin,/command to PATH itself before anything else.
#
# WHAT THIS SCRIPT DOES NOW (v3):
# It no longer touches s6-rc service definitions at all. Writing an empty
# `down` file into the SOURCE definition dir does NOT survive compilation:
# per skarnet docs (s6-rc-compile), ./down files in definition directories
# are deliberately ignored/not replicated into the generated live dirs.
# Instead it just COMPUTES the boot-stop list (which services the operator
# wants down at this boot) and stores it at $S6_BOOTDOWN_FILE. The real
# stopping is done later, once every longrun is up, by the `service-bootdown`
# oneshot which depends on all of them and runs `/command/s6-svc -d` against
# each listed service under /run/service -- exactly what the dashboard Stop
# button does.
#
# State model (persistent across restarts/redeploys), kept in
# /data/s6-state (survives container replacement because /data is a volume):
#   <name>.up     -> force UP at boot   (user clicked Start, or ENABLE_*=true)
#   <name>.down   -> force DOWN at boot (user clicked Stop, or ENABLE_*=false)
#   (no marker)   -> default policy applies
#
# Default policy: everything DOWN except the always-on core -- singbox
# (the gateway itself + proxy kernel) is NOT in the managed list and always
# boots; cloudflared boots only when TUNNEL_TOKEN is set (the tunnel is the
# management path for many deployments).
#
# ENABLE_* env vars (evaluated once per boot, before marker logic):
#   ENABLE_FLARESOLVERR=true|false etc. force that service's initial state
#   and persist the choice via markers.

# Mirror upstream /init: make sure s6 tools are reachable before anything else.
addpath () {
    x=$1
    OIFS=$IFS
    IFS=:
    set -- $PATH
    IFS=$OIFS
    while test $# -gt 0 ; do
        if test "$1" = "$x" ; then
            return
        fi
        shift
    done
    PATH="$x:$PATH"
}

if test -z "$PATH" ; then
    PATH=/bin
fi
addpath /bin
addpath /usr/bin
addpath /command
export PATH

STATE_DIR="${S6_STATE_DIR:-/data/s6-state}"
# NOTE: deliberately NOT under /run/s6 -- preinit later does `s6-rmrf /run/s6`,
# which would wipe the file before service-bootdown ever reads it. /tmp lives
# on the container's writable layer and survives the whole boot process.
BOOTDOWN="${S6_BOOTDOWN_FILE:-/tmp/ai-gateway-bootstop}"
mkdir -p "$STATE_DIR" 2>/dev/null || STATE_DIR="/tmp/s6-state"
mkdir -p "$STATE_DIR" 2>/dev/null || true

# Managed longruns. Must match servicedir names under $SRC_DIR. The core
# gateway service (singbox) is deliberately NOT managed here: it always
# boots, or there is no dashboard to turn anything on. The *-log companions
# are stopped too (see service-bootdown): a stopped producer with a running
# logger looks "up" to nothing but still burns a supervisor each.
ALL_SERVICES="zenfreeapi mimo zai kimi deepseek grok2api qwen2api flaresolverr cloudflared"
LOG_SERVICES="zenfreeapi-log mimo-log zai-log kimi-log deepseek-log grok2api-log qwen2api-log flaresolverr-log singbox-log cloudflared-log"

is_true () {
    case "${1:-}" in
        true|TRUE|True|1|yes|YES|Yes|on|ON|On) return 0 ;;
        *) return 1 ;;
    esac
}

: > "$BOOTDOWN"

want_down () {
    # 1) explicit env override wins for THIS boot and persists via marker
    eval "env_val=\${ENABLE_$(echo "$1" | tr 'a-z' 'A-Z'):-}"
    if test -n "$env_val" ; then
        rm -f "$STATE_DIR/$1.up" "$STATE_DIR/$1.down" 2>/dev/null || true
        if is_true "$env_val" ; then
            : > "$STATE_DIR/$1.up"
            return 1
        fi
        : > "$STATE_DIR/$1.down"
        return 0
    fi
    # 2) persistent marker from a previous dashboard toggle wins
    if test -f "$STATE_DIR/$1.up" ; then
        return 1
    fi
    if test -f "$STATE_DIR/$1.down" ; then
        return 0
    fi
    # 3) default policy: core + tunnel up, everything else off
    case "$1" in
        cloudflared)
            test -n "${TUNNEL_TOKEN:-}" && return 1
            return 0
            ;;
    esac
    return 0 # everything not explicitly kept up boots DOWN
}

for svc in $ALL_SERVICES ; do
    if want_down "$svc" ; then
        echo "$svc" >> "$BOOTDOWN"
        # stop its logger as well so the pipeline does not linger half-up;
        # loggers have their own names, markers do not manage them directly.
        for lg in $LOG_SERVICES ; do
            if test "${lg%-log}" = "$svc" ; then
                echo "$lg" >> "$BOOTDOWN"
            fi
        done
    fi
done

echo "[gateway-entrypoint] services to keep UP this boot:"
up_any=0
for svc in $ALL_SERVICES ; do
    if ! grep -qx "$svc" "$BOOTDOWN" ; then
        echo "[gateway-entrypoint]   $svc: up"
        up_any=1
    fi
done
test "$up_any" -eq 1 || echo "[gateway-entrypoint]   (none -- all optional services start down)"
echo "[gateway-entrypoint] boot-stop list -> $BOOTDOWN ($(wc -l < "$BOOTDOWN") entries)"

# Hand control to s6-overlay with the upstream argv contract ("$@" matters:
# rc.init runs S6_CMD_ARG0 with these as CMD). exec keeps PID 1 semantics
# (signal handling, zombie reaping) intact.
exec /init "$@"
