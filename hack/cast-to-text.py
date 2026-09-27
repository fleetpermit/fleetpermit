#!/usr/bin/env python3
# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Writes a plain-text transcript of an asciicast (v2 or v3) recording: the
# terminal output with colour and cursor escape sequences removed. Used as the
# text alternative for the demo videos.
#
#   hack/cast-to-text.py demo/recordings/demo-overview.cast > demo-overview.txt
import json
import re
import sys

ESCAPES = re.compile(r"\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07]*\x07|\x1b[()][A-Za-z0-9]")


def transcript(path):
    out = []
    with open(path, encoding="utf-8") as f:
        next(f)  # header
        for line in f:
            line = line.strip()
            if not line:
                continue
            event = json.loads(line)
            if len(event) >= 3 and event[1] == "o":
                out.append(event[2])
    text = ESCAPES.sub("", "".join(out)).replace("\r\n", "\n").replace("\r", "\n")
    lines = [l.rstrip() for l in text.split("\n")]
    # Collapse runs of blank lines left by screen clears.
    cleaned, blank = [], 0
    for l in lines:
        blank = blank + 1 if not l else 0
        if blank <= 1:
            cleaned.append(l)
    return "\n".join(cleaned).strip() + "\n"


if __name__ == "__main__":
    sys.stdout.write(transcript(sys.argv[1]))
