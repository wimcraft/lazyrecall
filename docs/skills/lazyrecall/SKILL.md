---
name: lazyrecall
description: Search the user's local coding-agent session history (Claude Code, pi, omp, goose, opencode, kilo, hermes) with the lazyrecall CLI, and read the matching session. Use when the user asks whether something was already done or solved before ("did we fix X?", "have I tried this already?"), refers to an earlier session or conversation ("last week we...", "in that session where..."), wants to find where a change was discussed or decided ("where did we decide about Y?", "which session set up Z?"), or asks what was decided about a topic some time ago. Trigger phrases include "did we already", "have we solved", "find the session", "which conversation", "what did we decide about", "search my history".
---

# lazyrecall

lazyrecall is a local, read-only index over every coding-agent session already on the user's disk - Claude Code, pi, omp, goose, opencode, kilo, hermes. It searches the user's own prompts and topics, and it can print a whole session's conversation. It makes no network calls and writes nothing to another tool's data.

## Workflow

1. **Search.** Put two or three distinctive words in quotes as the query - a function name, an error string, a product term - not a full sentence:

   ```sh
   lazyrecall search "flaky login test" --json
   ```

   `--json` gives one object per session with `match_snippet`, `topic`, `last_activity_at`, `cwd` and more. Narrow with the filters when the user named a project or a time: `--repo=PATH` (a repository root or working directory), `--since=DAYS`, `--agent=NAME` (`claude`, `pi`, `omp`, `hermes`, `goose`, `opencode`, `kilo`), `--tag=NAME`, `--starred`, or `--group=NAME`. Add `--all` to include sessions that the hide rules or the archive flag would otherwise suppress.

2. **Pick at most three candidates.** Read `match_snippet` first - it is the surrounding text of the prompt that matched - then `topic`, `last_activity_at`, and `cwd` to tell which one the user means. Do not read every hit.

3. **Read the session.** Export it as cleaned Markdown and read only the part you need:

   ```sh
   lazyrecall export <session_id> --clean
   lazyrecall export <session_id> --clean | head -60
   lazyrecall export <session_id> --clean | grep -n -i -C 5 "flaky"
   ```

   `<session_id>` is the `session_id` field from search - the full composite identifier. `--clean` leaves out tool calls and prints just the questions and answers. Long sessions are thousands of lines: pipe through `head` or `grep` rather than reading the whole thing.

4. **Answer.** Give the session's handle, its date, and the folder it ran in (`cwd`), and quote the part that answers the question. Then offer the resume command for the user to run themselves:

   ```
   To pick it up where you left off: lazyrecall resume <session_id>
   ```

   The handle in the listing is the short id: `lazyrecall resume 3` works, and so does an unambiguous prefix of the composite id.

## Rules

- **Never run `lazyrecall resume`.** It replaces the running process with the agent, taking over the terminal, so an agent must never invoke it. Tell the user the command instead.
- **Never change anything without being asked.** `star`, `tag`, `archive`, `name` and `comment` write to the user's own index, and `refresh --full` discards and rebuilds it - run none of them unless the user asks in this conversation.
- **Never disable redaction.** `export` redacts credentials (API keys, tokens, private keys) by default; never pass `--no-redact`.
- **Skip sessions whose `transcript_gone` is true.** The agent has deleted the file, so there is nothing to read; say so if such a session is the only hit.
- **Say when nothing matches** instead of guessing. `search` prints `No session matched ...` and an empty `items` array; report that and offer a looser query rather than inventing an answer.

## Example

The user asks: "did we already fix the flaky login test?"

Search with the distinctive words, scoped to nothing in particular:

```sh
lazyrecall search "flaky login" --json
```

Of the results, take the one whose `match_snippet` and `cwd` fit (trimmed):

```json
{
  "session_id": "claude:cc:9f2a1b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
  "handle": 3,
  "cwd": "/work/api",
  "last_activity_at": "2026-09-01T12:01:00+02:00",
  "last_prompt": "fixed the flaky login test by seeding the random source",
  "match_snippet": "why does the >>>flaky<<< >>>login<<< test fail"
}
```

Then read it:

```sh
lazyrecall export claude:cc:9f2a1b3c-4d5e-6f70-8192-a3b4c5d6e7f8 --clean | head -40
```

Answer:

> Yes - session `#3`, 1 September 2026 in `/work/api`. The flaky login test failed because the retry loop reused a random source; seeding it per test fixed it ("fixed the flaky login test by seeding the random source"). To pick it up where you left off: `lazyrecall resume 3`.
