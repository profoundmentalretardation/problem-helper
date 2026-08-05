import json

from langchain_core.messages import AIMessage, ToolMessage
from langchain_core.tools import BaseTool

from problem_helper import materials, tools


def call(tool: BaseTool, **kwargs) -> object:
    return json.loads(tool.invoke(kwargs))


def test_every_tool_is_registered_with_the_framework():
    assert len(tools.TOOLS) >= 2
    assert all(isinstance(t, BaseTool) for t in tools.TOOLS)
    assert {t.name for t in tools.TOOLS} == {
        "search_corpus",
        "get_learning_material",
        "list_material_topics",
    }


def test_tool_schemas_describe_their_arguments():
    spec = {s["name"]: s for s in tools.specs()}

    assert "query" in spec["search_corpus"]["args"]
    assert spec["search_corpus"]["description"]
    assert spec["get_learning_material"]["args"]["material_id"]["type"] == "string"


def test_search_returns_passages_with_their_rank_and_material():
    found = call(tools.search_corpus, query="binary search off-by-one", k=3)

    assert len(found) == 3
    assert {item["rank"] for item in found} == {1, 2, 3}
    first = next(item for item in found if item["rank"] == 1)
    assert first["material_id"] == "algo-prefix-sums"
    assert first["excerpt"]
    assert first["heading"] == "Building the array"


def test_search_hands_the_model_a_lim_packed_list():
    found = call(tools.search_corpus, query="anything", k=3)

    # strongest at the edges, weakest in the middle — the rank field keeps the truth
    assert [item["rank"] for item in found] == [1, 3, 2]


def test_search_clamps_k_to_the_supported_range():
    assert len(call(tools.search_corpus, query="anything", k=99)) == 3


def test_get_returns_the_full_material():
    material = call(tools.get_learning_material, material_id="algo-two-pointers")

    assert material["title"] == "Two pointers on a sorted array"
    assert "Pitfalls" in material["body"]


def test_get_with_an_unknown_id_explains_itself():
    answer = call(tools.get_learning_material, material_id="algo-nope")

    assert "unknown material_id" in answer["error"]
    assert "algo-two-pointers" in answer["known_ids"]


def test_topics_cover_the_whole_catalog():
    grouped = call(tools.list_material_topics)

    assert set(grouped) == set(materials.topics())
    assert sum(len(ids) for ids in grouped.values()) == len(materials.all())


def test_read_materials_reconstructs_what_the_agent_pulled():
    conversation = [
        AIMessage("thinking"),
        ToolMessage(
            content=tools.search_corpus.invoke({"query": "prefix sums", "k": 1}),
            tool_call_id="1",
        ),
        ToolMessage(
            content=tools.get_learning_material.invoke({"material_id": "algo-binary-search"}),
            tool_call_id="2",
        ),
        ToolMessage(content="not json at all", tool_call_id="3"),
    ]

    refs = tools.read_materials(conversation)

    assert [ref.id for ref in refs] == ["algo-prefix-sums", "algo-binary-search"]
    assert refs[0].title == "Prefix sums for range queries"


def test_read_materials_ignores_ids_that_are_not_in_the_catalog():
    message = ToolMessage(content=json.dumps([{"material_id": "made-up"}]), tool_call_id="1")

    assert tools.read_materials([message]) == []
