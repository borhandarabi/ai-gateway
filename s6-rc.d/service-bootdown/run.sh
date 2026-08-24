#!/command/with-contenv bash
# service-bootdown (oneshot) -- stops the services that must boot DOWN.
#
# WHY THIS EXISTS: s6-rc-compile deliberately does NOT replicate ./down files
# from service DEFINITION dirs into the generated live dirs ("Even if such a
# file exists in the definition directory, it will be ignored" -- skarnet
# s6-rc-compile docs). So the entrypoint cannot pre-mark services down; the
# only supported runtime mechanism is `s6-svc -d` against the LIVE servicedir
# under /run/service -- the same call the dashboard Stop button makes.
#
# TIMING: this oneshot depends on every managed longrun (+ their -log
# companions), so s6-rc brings the whole user bundle up first, then runs us;
# we immediately send -d to everything listed in $S6_BOOTDOWN_FILE. The stop
# is fast and idempotent: a brief window where a service starts and is then
# stopped within the same boot second is expected and harmless -- its run
# script's own startup checks keep it from doing meaningful work before it
# gets the down signal, and sing-box routes to these ports only after the
# dashboard marks them enabled.
set -e
exec 2>&1

BOOTDOWN="${S6_BOOTDOWN_FILE:-/tmp/ai-gateway-bootstop}"
LIVE="${S6_SERVICE_DIR:-/run/service}"

if test ! -s "$BOOTDOWN" ; then
    echo "[service-bootdown] nothing to stop"
    exit 0
fi

while IFS= read -r svc ; do
    test -n "$svc" || continue
    if test -d "$LIVE/$svc/supervise" ; then
        echo "[service-bootdown] stopping $svc"
        /command/s6-svc -d "$LIVE/$svc"
    else
        echo "[service-bootdown] skip $svc (not supervised)"
    fi
done < "$BOOTDOWN"

echo "[service-bootdown] done"
