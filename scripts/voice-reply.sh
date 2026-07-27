#!/usr/bin/env bash
# voice-reply.sh — speak a reply in Muthu's cloned voice and send it as a WhatsApp
# (or any messenger channel's) voice note.
#
# text -> ElevenLabs TTS (voice_id KUuOXc3i6FSizlq3R9X9) -> mp3 -> ffmpeg -> ogg/opus
# 16kHz mono -> POST /send on the LOCAL messenger hub with attachments[0].type=voice,
# which whatsapp.go maps onto `wacli send file --ptt` so it renders as a proper PTT
# voice-note bubble.
#
# Usage:
#   scripts/voice-reply.sh --channel <name> [--to <thread>] [--reply-to <id>] "<text>"
#
# Env:
#   ELEVENLABS_API_KEY      required, ElevenLabs API key (never printed)
#   MESSENGER_SERVE_TOKEN   required unless already set; else read from
#                            ~/.config/messenger/serve-token.env (never printed)
#   MESSENGER_ADDR           hub base URL (default http://127.0.0.1:14310)
#   VOICE_ID                 ElevenLabs voice id (default Muthu's clone)
#   TTS_MODEL                ElevenLabs model id (default eleven_turbo_v2_5, low latency)

set -euo pipefail

MESSENGER_ADDR="${MESSENGER_ADDR:-http://127.0.0.1:14310}"
VOICE_ID="${VOICE_ID:-KUuOXc3i6FSizlq3R9X9}"
TTS_MODEL="${TTS_MODEL:-eleven_turbo_v2_5}"
SERVE_TOKEN_ENV="${MESSENGER_SERVE_TOKEN_ENV:-$HOME/.config/messenger/serve-token.env}"

channel=""
to=""
reply_to=""
text=""

usage() {
  echo "usage: $0 --channel <name> [--to <thread>] [--reply-to <id>] \"<text>\"" >&2
  exit 1
}

while [ $# -gt 0 ]; do
  case "$1" in
    --channel) channel="$2"; shift 2 ;;
    --to) to="$2"; shift 2 ;;
    --reply-to) reply_to="$2"; shift 2 ;;
    -h|--help) usage ;;
    *)
      if [ -n "$text" ]; then usage; fi
      text="$1"; shift ;;
  esac
done

[ -n "$channel" ] || usage
[ -n "$text" ] || usage

command -v curl >/dev/null || { echo "voice-reply: curl not found" >&2; exit 1; }
command -v ffmpeg >/dev/null || { echo "voice-reply: ffmpeg not found" >&2; exit 1; }
command -v jq >/dev/null || { echo "voice-reply: jq not found" >&2; exit 1; }

if [ -z "${ELEVENLABS_API_KEY:-}" ]; then
  echo "voice-reply: ELEVENLABS_API_KEY is not set in this shell" >&2
  exit 1
fi

if [ -z "${MESSENGER_SERVE_TOKEN:-}" ]; then
  if [ -f "$SERVE_TOKEN_ENV" ]; then
    # shellcheck disable=SC1090
    set -a; source "$SERVE_TOKEN_ENV"; set +a
  fi
fi
if [ -z "${MESSENGER_SERVE_TOKEN:-}" ]; then
  echo "voice-reply: MESSENGER_SERVE_TOKEN is not set and not found in $SERVE_TOKEN_ENV" >&2
  exit 1
fi

workdir="$(mktemp -d "${TMPDIR:-/tmp}/voice-reply.XXXXXX")"
trap 'rm -rf "$workdir"' EXIT

mp3="$workdir/reply.mp3"
ogg="$workdir/reply.ogg"

http_status=$(curl -sS -X POST "https://api.elevenlabs.io/v1/text-to-speech/${VOICE_ID}" \
  -H "xi-api-key: ${ELEVENLABS_API_KEY}" \
  -H "Content-Type: application/json" \
  -d "$(jq -n --arg text "$text" --arg model "$TTS_MODEL" '{text:$text, model_id:$model}')" \
  --output "$mp3" \
  -w '%{http_code}')

if [ "$http_status" != "200" ] || [ ! -s "$mp3" ]; then
  echo "voice-reply: ElevenLabs TTS failed (http $http_status)" >&2
  exit 1
fi

ffmpeg -y -loglevel error -i "$mp3" -ar 16000 -ac 1 -c:a libopus "$ogg"
[ -s "$ogg" ] || { echo "voice-reply: ffmpeg produced an empty file" >&2; exit 1; }

body=$(jq -n \
  --arg channel "$channel" \
  --arg to "$to" \
  --arg reply_to "$reply_to" \
  --arg path "$ogg" \
  '{channel:$channel} +
   (if $to != "" then {to:$to} else {} end) +
   (if $reply_to != "" then {reply_to:$reply_to} else {} end) +
   {attachments:[{type:"voice", mime:"audio/ogg", path:$path}]}')

resp=$(curl -sS -X POST "${MESSENGER_ADDR}/send" \
  -H "Authorization: Bearer ${MESSENGER_SERVE_TOKEN}" \
  -H "Content-Type: application/json" \
  -d "$body")

echo "$resp"
echo "$resp" | jq -e '.ok == true' >/dev/null
