"""The state layer every loop shares.

Four stores, each holding what fits it, and none holding what belongs to another:

    db.py       paths and one connection helper
    sql.py      SQLite — the domain model: submissions, repairs, hints        (slice A)
    rules.py    markdown — the operating rules, editable by hand              (slice A)
    docs.py     a document store — free-form facts with cues, scoped          (slice B)
    events.py   an append-only hash-chained log + the agents' hand-off table  (slice B)
    notes.py    shared notes students leave on a problem, quoted as data      (slice C)

The rule for what goes where: if two loops need to *query* it, it is a row; if an agent wrote
it in prose for another to read later, it is a document; if it must hold on every run, it is
in the markdown.

Modules are imported directly (`from memory import docs, events`) rather than re-exported
here, so three people can add files without ever editing the same one.
"""
