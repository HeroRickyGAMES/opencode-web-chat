#!/usr/bin/env python3
"""OCR intermediary: receives image path, returns extracted text to stdout."""

import subprocess
import sys
import os

TESSERACT = "/tmp/tesseract-extract/usr/bin/tesseract"
TESSDATA  = "/tmp/tesseract-extract/usr/share/tesseract-ocr/5/tessdata"
LIB_PATH  = "/tmp/tesseract-extract/usr/lib/x86_64-linux-gnu"


def ocr(image_path: str) -> str:
    if not os.path.exists(image_path):
        return ""

    env = {**os.environ, "TESSDATA_PREFIX": TESSDATA, "LD_LIBRARY_PATH": LIB_PATH}
    cmd = [TESSERACT, image_path, "stdout", "-l", "por+eng", "--psm", "3"]

    r = subprocess.run(cmd, capture_output=True, text=True, timeout=30, env=env)

    text = r.stdout.strip()
    if r.returncode != 0:
        text = f""

    return text


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print("", flush=True)
        sys.exit(1)

    result = ocr(sys.argv[1])
    print(result, flush=True)
