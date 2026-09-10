#!/usr/bin/env python3
"""Check plan ownership and references; never execute CQ or touch user state."""
import json
from pathlib import Path
import re
import sys

HERE = Path(__file__).resolve().parent
BASE = HERE.parent


def require(condition, message):
    if not condition:
        raise ValueError(message)


def keyed(rows, key, label):
    result = {row[key]: row for row in rows}
    require(len(result) == len(rows), f"Duplicate {label}")
    return result


def main():
    spec = json.loads((BASE / "commands.json").read_text())
    migration = json.loads((BASE / "migration.json").read_text())
    coverage = json.loads((HERE / "coverage.json").read_text())
    require(coverage["schema_version"] == 1, "Unknown coverage schema")
    tasks = keyed(coverage["tasks"], "id", "task")
    require(set(tasks) == {f"T{i:02}" for i in range(29)}, "Expected T00–T28")
    documents = {}
    for task_id, task in tasks.items():
        path = HERE / task["document"]
        require(path.is_file(), f"Missing task document: {path}")
        documents[path] = path.read_text()
        require(f"## {task_id} — {task['title']}" in documents[path],
                f"Missing task heading: {task_id}")
        require(set(task["depends_on"]) <= tasks.keys(), f"Unknown dependency: {task_id}")
        require(task_id not in task["depends_on"], f"Self dependency: {task_id}")
    visited, active = set(), set()

    def visit(task_id):
        require(task_id not in active, f"Dependency cycle at {task_id}")
        if task_id in visited:
            return
        active.add(task_id)
        for dependency in tasks[task_id]["depends_on"]:
            visit(dependency)
        active.remove(task_id)
        visited.add(task_id)

    for task_id in tasks:
        visit(task_id)

    commands = keyed(coverage["commands"], "path", "command owner")
    leaves = {c["path"]: c for c in spec["commands"] if c["kind"] == "command"}
    require(set(commands) == set(leaves), "Leaf ownership differs from catalogue")
    for path, command in leaves.items():
        row = commands[path]
        require(row["owner"] in tasks, f"Unknown command owner: {path}")
        require(row["options"] == [p["name"] for p in command["options"]],
                f"Option coverage differs: {path}")
        require(row["positionals"] == [p["name"] for p in command["positionals"]],
                f"Positional coverage differs: {path}")
        require(row["acceptance_case_count"] == len(command["tests"]),
                f"Acceptance catalogue changed: {path}")
        require(row["availability"] == command.get("availability", {}).get("supported", True),
                f"Availability changed: {path}")
        text = documents[HERE / tasks[row["owner"]]["document"]]
        require(f"| `{path}` |" in text, f"Missing command in owning task: {path}")
        require((BASE / "help" / (path.replace(" ", "-") + ".txt")).is_file(),
                f"Missing exact help: {path}")
    groups = keyed(coverage["groups"], "path", "group owner")
    require(set(groups) == {c["path"] for c in spec["commands"] if c["kind"] == "group"},
            "Group coverage differs")
    require(all(g["owner"] == "T02" for g in groups.values()), "Group owner must be T02")
    globals_ = keyed(coverage["global_options"], "name", "global option")
    require(set(globals_) == {p["name"] for p in spec["global_options"]},
            "Global option coverage differs")
    require(all(g["owners"] == ["T02", "T03", "T04"] for g in globals_.values()),
            "Global option ownership differs")

    legacy = keyed(coverage["legacy"], "legacy_path", "legacy owner")
    require(set(legacy) == {m["legacy_path"] for m in migration}, "Legacy coverage differs")
    for entry in migration:
        row = legacy[entry["legacy_path"]]
        targets = sorted({p for p in [entry.get("canonical_path")] +
                          [v.get("target") for v in entry.get("variants", [])] if p})
        owners = (["T01"] if entry["disposition"] == "frozen_machine_abi" else
                  sorted({commands[p]["owner"] for p in targets if p in commands}) or ["T03"])
        require(row["owner"] == "T03", "Legacy grammar must have one owner")
        require(row["disposition"] == entry["disposition"] and row["targets"] == targets,
                f"Legacy translation changed: {entry['legacy_path']}")
        require(row["semantic_owners"] == owners,
                f"Legacy semantic owner differs: {entry['legacy_path']}")
    for resource in coverage["resource_documents"]:
        require((BASE / resource["path"]).is_file(), f"Missing resource: {resource['path']}")
        require(resource["owners"] and set(resource["owners"]) <= tasks.keys(),
                f"Missing resource owners: {resource['path']}")

    documents[BASE / "plan.md"] = (BASE / "plan.md").read_text()
    for path, text in documents.items():
        for header in ["**Goal:**", "**Architecture:**", "**Tech Stack:**", "**Spec:**",
                       "## Global Constraints", "REQUIRED SUB-SKILL:"]:
            require(header in text, f"Missing plan header {header}: {path.name}")
        require(not re.search(r"\b(?:TODO|TBD|FIXME)\b|fill in details|implement later", text),
                f"Unresolved placeholder: {path.name}")
        for match in re.finditer(r"(?<!!)\[[^\]]+\]\(([^)]+)\)", text):
            target = match.group(1).split("#", 1)[0]
            if target and "://" not in target:
                require((path.parent / target).exists(), f"Broken plan link: {target}")
        for subject in re.findall(r"Commit task files with subject `([^`]+)`", text):
            require(len(subject) < 50, f"Commit subject too long: {subject}")
            require(re.match(r"(?:feat|fix|test|docs|refactor|chore|perf|ci): ", subject),
                    f"Invalid Conventional Commit subject: {subject}")
    option_count = sum(len(c["options"]) for c in leaves.values())
    positional_count = sum(len(c["positionals"]) for c in leaves.values())
    print(f"Plan coverage OK: {len(tasks)} tasks, {len(leaves)} commands, "
          f"{len(groups)} groups, {len(legacy)} legacy entries, "
          f"{option_count} local options, {positional_count} positionals, "
          f"{len(globals_)} global options.")
    print("Ownership coverage only; implementation and native acceptance remain future work.")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, json.JSONDecodeError) as error:
        print(f"Plan coverage failed: {error}", file=sys.stderr)
        sys.exit(1)
