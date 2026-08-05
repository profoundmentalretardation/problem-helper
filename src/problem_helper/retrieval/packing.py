"""Lost-in-the-middle packing — a presentation step, never an evaluation step.

Liu et al. (2024) measured a U-shaped curve in how much attention a long context gets: what
sits at the beginning and at the end is used, what sits in the middle is often ignored.
`pack_for_lim` therefore places the strongest chunks at the two edges and buries the weakest
in the middle.

The important part is where this is allowed to run. It **reorders the ranking**, so MRR and
nDCG computed over a packed list measure the packing, not the retriever: the top-ranked
chunk lands at position 0, the second-ranked at the *last* position, and nDCG reads that as
a relevant document that slid down the list. Hit rate, precision and recall are order-blind
and stay unchanged, which is what makes the mistake so easy to miss — three of the five
metrics look fine.

So: the retrieval layer returns retriever order, the eval harness scores retriever order,
and packing happens only where the text is handed to a model.
"""

from __future__ import annotations


def pack_for_lim[T](ranked: list[T]) -> list[T]:
    """Strongest items at the edges, weakest in the middle; input must be best-first."""
    front: list[T] = []
    back: list[T] = []
    for position, item in enumerate(ranked):
        (front if position % 2 == 0 else back).append(item)
    return front + back[::-1]
