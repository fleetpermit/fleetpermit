#!/usr/bin/env bash
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Records the three demo videos from REAL runs of demo/run.sh against the lab:
# asciinema captures the terminal, agg renders it, ffmpeg encodes MP4.
# Output: dist/video/{demo-overview,demo-security,demo-disconnected-expiry}.{cast,mp4}
# plus a poster PNG for each, README GIFs in docs/assets/, and a copy of each
# cast in demo/recordings/. With FP_SITE_DIR set, the MP4s and JPEG posters are
# also copied into that website checkout. Media tools are development tooling
# only.
#
#   FP_VIDEO_OUT   output directory (default dist/video)
set -euo pipefail
cd "$(dirname "$0")/.."
for t in asciinema agg ffmpeg; do command -v "$t" >/dev/null || { echo "missing: $t" >&2; exit 1; }; done
out="${FP_VIDEO_OUT:-dist/video}"
mkdir -p "${out}"

record() {
  local name="$1" mode="$2" title="$3"
  local cast="${out}/${name}.cast" gif="${out}/${name}.gif"
  echo "== recording ${name} (demo/run.sh ${mode})"
  local pause="${4:-1}"
  DEMO_PAUSE="${pause}" DEMO_LEASE_SECONDS="${DEMO_LEASE_SECONDS:-40}" asciinema rec --overwrite --headless --window-size 112x38 \
    --idle-time-limit "$((pause + 1))" --title "${title}" --command "./demo/run.sh ${mode}" "${cast}" </dev/null
  agg --font-size 18 --theme monokai --idle-time-limit "$((pause + 1))" --last-frame-duration 4 --fps-cap 15 "${cast}" "${gif}" >/dev/null
  # ffmpeg drops the duration of a GIF's last frame; hold it for 5 seconds.
  ffmpeg -loglevel error -y -i "${gif}" -movflags faststart -pix_fmt yuv420p -c:v libx264 -crf 28 -preset slow \
    -vf "tpad=stop_mode=clone:stop_duration=5,scale=trunc(iw/2)*2:trunc(ih/2)*2" "${out}/${name}.mp4"
  ffmpeg -loglevel error -y -sseof -1 -i "${out}/${name}.mp4" -frames:v 1 -update 1 "${out}/${name}.png"
  rm -f "${gif}"
  # Lightweight GIF for the README (GitHub plays GIFs inline).
  if [[ "${name}" == demo-overview || "${name}" == demo-disconnected-expiry ]]; then
    agg --font-size 16 --speed 1.3 --fps-cap 8 --theme monokai --idle-time-limit 1.5 --last-frame-duration 6 \
      "${cast}" "docs/assets/${name}.gif" >/dev/null
  fi
  printf '   %s: %s, %s bytes, %ss\n' "${name}" "${out}/${name}.mp4" "$(wc -c <"${out}/${name}.mp4" | tr -d ' ')" \
    "$(ffprobe -v error -show_entries format=duration -of csv=p=0 "${out}/${name}.mp4" | cut -d. -f1)"
}

# FP_DEMOS selects which recordings to make (default: all three).
for d in ${FP_DEMOS:-overview security disconnect}; do
  case "$d" in
    overview) record demo-overview overview "FleetPermit: least privilege for agents, across every cluster" 1 ;;
    security) record demo-security security "FleetPermit: security boundaries" 4 ;;
    disconnect) record demo-disconnected-expiry disconnect "FleetPermit: the lease expires even when the hub is gone" 1 ;;
  esac
done

# Keep the casts in the repository, next to the README that describes them.
cp "${out}"/demo-*.cast demo/recordings/
# A plain-text transcript of each recording is the text alternative for its video.
for c in demo/recordings/demo-*.cast; do
  python3 "$(dirname "$0")/cast-to-text.py" "$c" >"${c%.cast}.txt"
done
# With FP_SITE_DIR set to a website checkout, publish the videos and posters.
if [[ -n "${FP_SITE_DIR:-}" && -d "${FP_SITE_DIR}/assets/video" ]]; then
  for f in "${out}"/demo-*.mp4; do
    n="$(basename "$f" .mp4)"
    cp "$f" "${FP_SITE_DIR}/assets/video/${n}.mp4"
    ffmpeg -loglevel error -y -i "${out}/${n}.png" -q:v 4 "${FP_SITE_DIR}/assets/video/${n}.jpg"
  done
  echo "   published videos to ${FP_SITE_DIR}/assets/video"
fi
