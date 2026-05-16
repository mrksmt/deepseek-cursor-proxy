#!/bin/sh
set -eu

# ── ngrok authtoken setup ──────────────────────────────────────────────────
if [ -n "${NGROK_AUTHTOKEN:-}" ]; then
    ngrok config add-authtoken "$NGROK_AUTHTOKEN" 2>/dev/null || \
        echo "[entrypoint] warning: failed to configure ngrok authtoken" >&2
fi

# ── Environment variable to CLI argument mapping ───────────────────────────
# Boolean env vars → --flag / --no-flag; string/numeric → --key value.

add_boolean_flag() {
    env_name=$1
    flag_name=$2
    eval "val=\${$env_name:-}"
    if [ -n "$val" ]; then
        case "$val" in
            1|true|yes|on)  set -- "$@" "--${flag_name}" ;;
            0|false|no|off) set -- "$@" "--no-${flag_name}" ;;
        esac
    fi
}

add_string_flag() {
    env_name=$1
    flag_name=$2
    eval "val=\${$env_name:-}"
    if [ -n "$val" ]; then
        set -- "$@" "--${flag_name}" "$val"
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
    set -- "$@" --clear-reasoning-cache
fi

# ── Symlink /data → ~/.deepseek-cursor-proxy ──────────────────────────────
DATA_DIR="${HOME:-/root}/.deepseek-cursor-proxy"
if [ ! -L "$DATA_DIR" ]; then
    rm -rf "$DATA_DIR"
    ln -sf /data "$DATA_DIR"
fi

exec "$@"
