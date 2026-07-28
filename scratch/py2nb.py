#!/usr/bin/env python3
"""Convert a percent-format .py file into a .ipynb notebook (no nbformat needed)."""
import json
import sys
from pathlib import Path


def parse(text):
    cells = []
    kind, buf = None, []

    def flush():
        if kind is None:
            return
        src = "\n".join(buf).strip("\n")
        if not src.strip():
            return
        if kind == "markdown":
            lines = []
            for ln in src.split("\n"):
                if ln.startswith("# "):
                    lines.append(ln[2:])
                elif ln.strip() == "#":
                    lines.append("")
                else:
                    lines.append(ln)
            src = "\n".join(lines)
        cells.append((kind, src))

    for line in text.split("\n"):
        if line.startswith("# %%"):
            flush()
            kind = "markdown" if "[markdown]" in line else "code"
            buf = []
        else:
            buf.append(line)
    flush()
    return cells


def to_nb(cells):
    out = []
    for kind, src in cells:
        lines = src.split("\n")
        source = [ln + "\n" for ln in lines[:-1]] + [lines[-1]]
        cell = {"cell_type": kind, "metadata": {}, "source": source}
        if kind == "code":
            cell["execution_count"] = None
            cell["outputs"] = []
        out.append(cell)
    return {
        "cells": out,
        "metadata": {
            "kernelspec": {
                "display_name": "Python 3",
                "language": "python",
                "name": "python3",
            },
            "language_info": {"name": "python", "version": "3.14.6"},
        },
        "nbformat": 4,
        "nbformat_minor": 5,
    }


if __name__ == "__main__":
    src, dst = Path(sys.argv[1]), Path(sys.argv[2])
    nb = to_nb(parse(src.read_text()))
    dst.write_text(json.dumps(nb, indent=1, ensure_ascii=False) + "\n")
    print(f"{dst} <- {src} ({len(nb['cells'])} cells)")
