# Что кому коммитить и что писать в отчёте

Сдаём **каждый свой коммит** + свои 1-2 страницы. Чужие коммиты не засчитываются, поэтому в
один коммит попадают только свои файлы. `docs/hw2-split.md` — кто что делает, здесь только
границы коммитов и тексты.

Порядок: сначала коммит B (там `memory/db.py`, от которого зависят A и C), потом A и C
параллельно, в любом порядке.

Весь код всех трёх слайсов написан и лежит в рабочей копии — 185 проверок зелёные, трейсы сняты
на живой модели. Каждому остаётся прочитать свой раздел, прогнать свои тесты, убедиться, что
согласен с тем, что написано в его отчёте, и закоммитить свои файлы. Отчёт не переписывать
целиком не обязательно, но сдавать текст, за который не можешь ответить на вопрос
преподавателя, — плохая идея: прогони свои тесты и трейсы сам.

---

## B — куратор (Витя). Готово, можно коммитить сейчас

```bash
git add memory/__init__.py memory/db.py memory/docs.py memory/events.py \
        04_curator_loop.ipynb scratch/nb4.py scratch/trace_b.py \
        rules/curator_brief.md traces/b_curator.md \
        docs/hw2-split.md docs/hw2-commits.md docs/hw2-writeup-b.md \
        .gitignore scratch/README.md scratch/py2nb.py scratch/nb0.py \
        scratch/nb2.py scratch/nb3.py
git commit
```

`scratch/` больше не в `.gitignore`, поэтому исходники ноутбуков теперь коммитятся. Раз это
решение приземляется здесь, в этот же коммит идут и старые файлы `scratch/` от HW1 — иначе они
повиснут в untracked у всех троих. `__pycache__/` по-прежнему игнорируется.

Сообщение коммита:

```
feat: add the curator agent and the memory it writes to

Second agent, running after the repair and hint loops on the finished run,
deciding what the system should still know next week.

- memory/docs.py   document store: free-form facts with a retrieval cue,
                   scoped user:<id> or shared, scope applied before ranking
- memory/events.py append-only hash-chained event log + the handoffs table
                   the two agents coordinate through, both keyed by run_id
- memory/db.py     paths and connect(); the only file other slices touch
- 04_curator_loop  three tools, a four-call effort budget, a human gate on
                   propose_rule, and no learning from a run that errored
- rules/curator_brief.md  delegation brief; the system prompt is built from it

Replaces state/memory.json, which was a {user: {tag: count}} dump written by
the orchestrator on a fixed rule rather than by an agent making a judgement.

Tests: 31 in the notebook, 38 in the stores (python -m memory.docs,
python -m memory.events), all offline with a scripted model and real stores.
Live trace in traces/b_curator.md.

Co-Authored-By: Claude Opus 5 (1M context) <noreply@anthropic.com>
```

Трейлер `Co-Authored-By` — потому что код писался с ассистентом, а курс прямо говорит, что это
и есть предполагаемый способ работы. Если не хочешь — убери, ничего не сломается.

Отчёт: `docs/hw2-writeup-b.md`, готов.

---

## A — хранилища домена и сборка контекста

```bash
git add memory/sql.py memory/rules.py rules/tutor_rules.md \
        01_repair_loop.ipynb scratch/nb1.py \
        traces/a_push_pull.md docs/hw2-writeup-a.md
git commit
```

Сообщение коммита:

```
feat: move the domain model into SQLite and the rules into markdown

The per-student memory was {user: {tag: count}} in state/memory.json. A count
answers "how often"; the rows now also answer "on which problem, when, and what
it was", which is what the next session's context actually needs.

- memory/sql.py    submissions, repairs, hints; top_mistakes() is a GROUP BY,
                   record_repair() rejects a tag outside the enum
- memory/rules.py  operating rules read from markdown fresh on every run, so an
                   admin edit lands on the next run with no restart
- rules/tutor_rules.md  the rules themselves, previously a Python constant
- 01_repair_loop   push: rules first (identical for everyone, so it stays in the
                   cacheable prefix), per-student blocks last. pull: retrieve_memory
                   and read_problem_notes, called by the model when it needs them

Tests: 60 checks, no API key (18 in memory.sql, 16 in memory.rules, 26 in the
notebook).
```

Отчёт: черновик лежит в `docs/hw2-writeup-a.md`. Всё, что следует из дизайна, уже написано;
заполнить нужно места, помеченные `[...]` — цифры и то, что реально поймали тесты. Придуманных
результатов там быть не должно.

---

## C — shared-память и монитор

```bash
git add memory/notes.py monitor.py ask_tutor.py \
        05_monitor.ipynb scratch/nb5.py scratch/trace_ac.py \
        docs/tool_descriptions.txt \
        traces/c_shared_vs_private.md traces/c_planted_comment.md \
        traces/c_monitor_report.md docs/hw2-writeup-c.md
git commit
```

`scratch/trace_ac.py` генерирует и трейс слайса A тоже — сам файл трейса лежит в коммите A,
а генератор здесь, потому что три трейса из четырёх его.

Сообщение коммита:

```
feat: add shared student notes and a monitor that judges runs after the fact

Notes are the shared layer: one student writes a note on a problem, every other
student's agent reads it. That makes them attacker-controlled text on a path that
already feeds a model, so they are scored on write and quoted on read.

- memory/notes.py  notes table; risk and flagged computed at write time so a
                   reader cannot skip them; read_problem_notes() returns each note
                   wrapped with its author and a banner saying it is data, not
                   instructions. A high score does not block the write.
- ask_tutor.py     the smallest student-question path, since none of the three
                   loops is one; this is where A's note reaches B's agent
- monitor.py       runs off the request path, over the hash-chained event log,
                   sampling finished runs
- 05_monitor       named-value rubrics, not a 1-10 score: prompt_adherence is
                   strictly_adheres / minor_violation / serious_violation, with the
                   line drawn at whether the student's outcome changed. Every
                   violation carries a rationale of expected vs got; a verdict the
                   judge cannot ground is indistinguishable from a hallucination.

Found, on the first live run: tutor_rules.md tells the agent to say so when the
hint budget runs out, and there is no tool that sends a non-hint message. A rule
nobody could obey, in a file we wrote ourselves.

Tests: 56 checks, no API key (28 in memory.notes, 28 in the notebook).
```

Отчёт: черновик лежит в `docs/hw2-writeup-c.md`. Заполнить места `[...]` — в первую очередь
раздел «что нашёл монитор», это обязательный пункт задания, и вердикт с полем `rationale` надо
скопировать из своего же трейса, а не пересказать.

---

## Общий ответ про архитектуру — одинаковый у всех троих, своими словами

Требование задания: «одна строка о том, почему архитектура агентов вашей команды устроена
именно так, и что стоил бы один агент; одна из четырёх причин или пятая своя».

Система: **исполнитель** (shield → repair → hint) + **куратор** + **монитор**. Две причины из
четырёх:

- **Focus, на реально конфликтующих целях.** У исполнителя цель «почини и скажи полезное
  сейчас», у куратора — «что из этого верно и через неделю». Сессия 5 говорит резать там, где
  конфликтуют цели, а не по названиям должностей, — здесь они конфликтуют.
- **Decoupled execution.** Куратор работает после того, как студент получил подсказку, монитор
  — вообще на своих часах над логами. Ни один не на критическом пути живого ответа.

Что стоил бы один агент — не гипотеза, а то, что лежало в репозитории: `state/memory.json`.
Агенту не доверили решать, что запоминать, поэтому память писал оркестратор по фиксированному
правилу — счётчик тегов. Записывалось единственное, что и так было энумом, и ничего из того,
что требовало суждения. «Хочет только концептуальные подсказки» такая схема не смогла бы
записать, даже если бы студент повторил это в каждом сообщении.

---

## Что отправить в чат

**Обоим:**

> Дедлайн вчера прошёл, поэтому давайте сегодня. Разложил задание на три части так, чтобы мы
> не лезли в одни файлы — `docs/hw2-split.md`, там общий контракт и по разделу на каждого.
> Границы коммитов и заготовки сообщений — `docs/hw2-commits.md`.
>
> Свою часть (куратор + документный стор + лог) я закоммитил, `memory/db.py` уже в репозитории,
> так что вы не заблокированы. Читаем «Общий контракт» в split.md **до** того, как начали
> писать: ключ сессии, конверт `{status, result, needs_approval}`, формат `run_id` и записи
> лога — это единственное, что должно совпасть у всех троих.
>
> Правило про файлы: один ноутбук — один владелец. Ноутбуки в git это JSON, мерж-конфликты в
> них разбирать больно. `01_repair_loop.ipynb` правит только A, `02_hint_loop` и
> `03_prompt_shield` не трогает никто.
>
> Сдаём каждый свой коммит + свои 1-2 страницы, чужие коммиты не засчитываются. Что писать в
> отчёте — в hw2-commits.md, там же общий кусок про архитектуру, он у нас у всех один и должен
> совпадать по смыслу.

**Отдельно A:** твой раздел — «Слайс A» в split.md. Главное: `repairs` строками, а не
счётчиком, и правила читаются из markdown заново каждый ран. Схема таблиц и список тестов
расписаны.

**Отдельно C:** твой раздел — «Слайс C». Монитор должен найти настоящую проблему — она уже есть
в коде, описана в конце твоего раздела: `tutor_rules.md` и схема `propose_hint` из loop 2
противоречат друг другу.
