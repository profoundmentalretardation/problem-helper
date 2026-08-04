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
        "search_learning_materials",
        "get_learning_material",
        "list_material_topics",
    }


def test_tool_schemas_describe_their_arguments():
    spec = {s["name"]: s for s in tools.specs()}

    assert "query" in spec["search_learning_materials"]["args"]
    assert spec["search_learning_materials"]["description"]
    assert spec["get_learning_material"]["args"]["material_id"]["type"] == "string"


def test_search_ranks_the_matching_material_first():
    found = call(tools.search_learning_materials, query="binary search off-by-one", limit=2)

    assert found[0]["id"] == "algo-binary-search"
    assert len(found) == 2
    assert found[0]["summary"]


def test_search_without_a_match_returns_an_empty_list():
    assert call(tools.search_learning_materials, query="quantum entanglement") == []


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
    assert sum(len(ids) for ids in grouped.values()) == len(materials.CATALOG)


def test_read_materials_reconstructs_what_the_agent_pulled():
    conversation = [
        AIMessage("thinking"),
        ToolMessage(
            content=tools.search_learning_materials.invoke({"query": "prefix sums", "limit": 1}),
            tool_call_id="1",
        ),
        ToolMessage(
            content=tools.get_learning_material.invoke({"material_id": "algo-prefix-sums"}),
            tool_call_id="2",
        ),
        ToolMessage(content="not json at all", tool_call_id="3"),
    ]

    refs = tools.read_materials(conversation)

    assert [ref.id for ref in refs] == ["algo-prefix-sums"]
    assert refs[0].title == "Prefix sums for range queries"


def test_read_materials_ignores_ids_that_are_not_in_the_catalog():
    message = ToolMessage(content=json.dumps([{"id": "made-up"}]), tool_call_id="1")

    assert tools.read_materials([message]) == []
