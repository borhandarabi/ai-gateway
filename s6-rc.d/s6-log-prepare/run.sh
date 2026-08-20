#!/command/with-contenv bash
set -e
mkdir -p /run/log/cloudflared /run/log/deepseek-api /run/log/flaresolverr /run/log/grok2api /run/log/kimi-api /run/log/mimo /run/log/omniroute /run/log/qwen2api /run/log/singbox /run/log/zai-api
chown -R nobody:nogroup /run/log
chmod -R 02755 /run/log
