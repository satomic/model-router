"""Let the scripts under verify/ be run directly as `python verify/xxx.py`.

Once the scripts moved into a subdirectory, sys.path[0] became verify/ and `import app` broke;
the relative paths the scripts use (`logs/traces`, `config.yaml`, ...) also assume the repository
root. So this module adds the repository root to sys.path and chdir()s into it -- every script
only needs `import _bootstrap` as its first line.
"""
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent

if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))
os.chdir(ROOT)
