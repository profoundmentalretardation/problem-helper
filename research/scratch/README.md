# scratch/ — not committed

`nb0.py` … `nb3.py` are the percent-format (`# %%`) sources the four notebooks were generated
from, and `py2nb.py` converts them:

    ../.venv/bin/python py2nb.py nb3.py ../03_prompt_shield.ipynb

They are here so nothing lives only in `/tmp`, but the **`.ipynb` files are the source of
truth**. Edit a notebook in Jupyter and the `.py` next to it goes stale — either regenerate the
`.py` by hand or ignore this directory entirely.

Running a `.py` directly works too (the `# %%` markers are just comments), which is how the
test sections were checked:

    ../.venv/bin/python nb3.py
