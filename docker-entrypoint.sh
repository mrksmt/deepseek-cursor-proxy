#!/bin/sh
set -eu

# ── ngrok authtoken setup ──────────────────────────────────────────────────
if [ -n "${NGROK_AUTHTOKEN:-}" ]; then
    ngrok config add-authtoken "$NGROK_AUTHTOKEN" 2>/dev/null || \
        echo "[entrypoint] warning: failed to configure ngrok authtoken" >&2
fi

# ── Environment variable to CLI argument mapping ───────────────────────────
# Boolean env vars → --flag / --no-flag; string/numeric → --key value.
# We accumulate args in a variable because `set --` inside functions is local
# and would lose the accumulated flags.

ARGS=""

add_boolean_flag() {
    env_name=$1
    flag_name=$2
    eval "val=\${$env_name:-}"
    if [ -n "$val" ]; then
        case "$val" in
            1|true|yes|on)  ARGS="$ARGS --${flag_name}" ;;
            0|false|no|off) ARGS="$ARGS --no-${flag_name}" ;;
        esac
    fi
}

add_string_flag() {
    env_name=$1
    flag_name=$2
    eval "val=\${$env_name:-}"
    if [ -n "$val" ]; then
        ARGS="$ARGS --${flag_name} $val"
    fi
}

add_boolean_flag DEEPSEEK_VERBOSE verbose
add_boolean_flag DEEPSEEK_NGROK ngrok
add_boolean_flag DEEPSEEK_DISPLAY_REASONING display-reasoning
add_boolean_flag DEEPSEEK_COLLAPSIBLE_REASONING collapsible-reasoning
add_boolean_flag DEEPSEEK_CORS cors
add_string_flag DEEPSEEK_HOST host
add_string_flag DEEPSEEK_PORT port
add_string_flag DEEPSEEK_MODEL model
add_string_flag DEEPSEEK_BASE_URL base-url
add_string_flag DEEPSEEK_THINKING thinking
add_string_flag DEEPSEEK_REASONING_EFFORT reasoning-effort
add_string_flag DEEPSEEK_REASONING_CONTENT_PATH reasoning-content-path
add_string_flag DEEPSEEK_TRACE_DIR trace-dir
add_string_flag DEEPSEEK_CONFIG_PATH config
add_string_flag DEEPSEEK_REQUEST_TIMEOUT request-timeout
add_string_flag DEEPSEEK_MAX_REQUEST_BODY_BYTES max-request-body-bytes
add_string_flag DEEPSEEK_REASONING_CACHE_MAX_AGE_SECONDS reasoning-cache-max-age-seconds
add_string_flag DEEPSEEK_REASONING_CACHE_MAX_ROWS reasoning-cache-max-rows
add_string_flag DEEPSEEK_MISSING_REASONING_STRATEGY missing-reasoning-strategy
if [ "${DEEPSEEK_CLEAR_REASONING_CACHE:-}" = "1" ] || [ "${DEEPSEEK_CLEAR_REASONING_CACHE:-}" = "true" ]; then
    ARGS="$ARGS --clear-reasoning-cache"
fi

# ── Symlink /data → ~/.deepseek-cursor-proxy ──────────────────────────────
DATA_DIR="${HOME:-/root}/.deepseek-cursor-proxy"
if [ ! -L "$DATA_DIR" ]; then
    rm -rf "$DATA_DIR"
    ln -sf /data "$DATA_DIR"
fi

# shellcheck disable=SC2086
exec "$@" $ARGS
