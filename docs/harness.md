# The harness

The harness is everything wrapped around the model call. A plain
call-tools-until-done loop works for a short request and fails in specific,
predictable ways on a long one. Four mechanisms address those failures.

## The repetition guard

A model that has lost the thread calls the same tool with the same arguments,
reads the same failure, and calls it again. Left alone it burns the entire turn
budget doing that, and the run ends with nothing to show and no explanation.

Every tool call is fingerprinted by name and arguments, normalised so that a
re-serialised identical call still counts as the same one. After three, the
model is told plainly:

> You have called read_file with the same arguments several times and it is not
> getting you anywhere. Do not call it again. Either try a different approach,
> or say what is blocking you.

That nudge fires once, not on every subsequent call — otherwise the history
fills with the same message. After six, the run stops and says why.

```yaml
agent:
  repeat_limit: 3     # identical calls tolerated before the nudge
```

Different arguments are not repetition. Reading twenty files in a row is fine.

## Steering

You notice a run going the wrong way. Waiting for it to finish wastes the work;
interrupting it throws away the work that was good.

`/steer <instruction>` queues a note and delivers it after the current batch of
tools completes — the first moment the model can act on it without discarding
anything already underway.

```
/steer stop looking at the tests, the bug is in the parser
```

It reports back when nothing is running, so the note is never silently dropped.

## Verification

Models describe work they did not do. Not usually, and not deliberately, but
often enough that a long autonomous run needs a check.

With `agent.verify_replies` on, a finished answer goes to a second, cheap model
along with the list of tools that actually ran. The critic is asked one thing:
was the request carried out? Not whether it was done well, and not whether more
could be done — only whether what was asked for actually happened.

When it finds something missing, the turn does not end. The gap is fed back:

> Before finishing: the second file was never written. Do that now, or explain
> why it cannot be done.

This is bounded by `agent.verify_max`, because a critic that is never satisfied
would loop forever. Two passes is the default.

```yaml
agent:
  verify_replies: true
  verify_max: 2
```

It is off by default because it costs an extra call per turn. On a long
unattended run — a cron job, a standing goal — it earns that cost back the first
time it catches a silent omission.

Sub-agents are not verified: whoever delegated the work checks it.

## Standing goals

A goal outlives a turn.

```
/goal get the test suite green, then open a pull request
```

From then on the goal is part of every system prompt, and when the agent thinks
it is finished, a judge model decides whether the goal is actually met. If not,
the judge names one concrete next step and the loop continues with it.

```
/goal status     where it stands, and how many iterations it has taken
/goal pause      hold it without losing it
/goal resume     pick it back up
/goal clear      drop it
```

The judge is told that a plan is not completion and an intention is not doing —
but also not to invent extra work. If the goal is met, it says so.

Iterations are capped:

```yaml
agent:
  goal_max_iterations: 10
```

At the cap the goal pauses rather than stopping, so `/goal resume` continues from
where it got to. If no judge model is available, the loop ends rather than
running forever.

### Confident (autonomous) goals

A normal goal advances one judge step per turn and then waits for you. A
**confident** goal does not wait: it keeps starting its own turns and works
towards the goal unattended until it is met — across the web dashboard, the
gateways, and even a server restart.

```
/goal auto <what you want done>       confident, default cap
/goal auto <n> <what you want done>   confident, cap at n iterations
/goal auto 0 <what you want done>     confident, no cap — runs until met
/goal auto on | off                   flip an existing goal in or out of the mode
```

It is opt-in because it spends tokens on its own. Every iteration prints a
visible notice, so you always see it working.

**Smarter when stuck.** If the judge asks for the same next step again — no
progress — the goal escalates its tactics instead of repeating a failing step:

1. change approach — a different command, tool, or angle;
2. gather information first — read the documentation, search the web or a
   tutorial for the exact error or task, then apply what it learns;
3. change strategy — delegate a focused research sub-agent, or break the goal
   into a smaller step it can complete now.

**Bounds.** It stops on its own only when the goal is met, the cap is reached
(then it pauses, resumable), or it hits something **only you** can resolve — a
missing secret, an ambiguous requirement, an irreversible choice. In that last
case it pauses and says exactly what it needs, rather than looping on the
impossible. A hard technical problem is not a blocker: it keeps trying. Stop it
any time with `/goal pause` or `/goal clear`.

```yaml
agent:
  goal_autonomous_max_iterations: 50   # default cap for /goal auto; a per-goal 0 means unlimited
```

## Learning

`/learn` turns what just happened into a skill.

The session transcript goes to a model with one instruction: write the procedure
someone would want next time, with the specific commands, paths, and gotchas
that actually came up — and skip everything particular to this one conversation.
If nothing general was learned, it says so and writes nothing.

What comes back is saved into the skill library, front matter and all, and is
offered the next time something similar comes up.

```
/learn                        distil the whole session
/learn the deploy sequence    focus on one part of it
```

This is the loop that makes the agent better at your work specifically:
solve something once, keep the procedure, do it faster next time.

## Everything together

A long autonomous run with all four:

1. `/goal migrate the config loader to the new schema`
2. The agent works. The repetition guard catches it re-reading the same file and
   redirects it.
3. You notice it editing the wrong package: `/steer the loader is in
   internal/config, not internal/settings`.
4. It says it is done. Verification notices the tests were never run and sends it
   back.
5. It runs them, fixes two failures, and says it is done again.
6. The judge agrees the goal is met and the run ends.
7. `/learn` keeps the migration procedure for the next one.

## A panel of models

One model's blind spot is often another's strength. `/panel` asks several the
same question independently and synthesises one answer.

```yaml
model:
  panel:
    - anthropic/claude-sonnet-4.5
    - openai/gpt-5
    - google/gemini-3-pro
```

```
/panel is it safe to run this migration on a live database?
```

The value is in the disagreement. Where independent answers diverge is usually
where the question was ambiguous or the problem is genuinely hard — exactly what
a single sample hides.

The synthesiser is told not to average: where the answers differ it says so and
follows the one best supported by its own reasoning, rather than producing
something none of them said. A model that fails to answer is named, so a quiet
failure is not mistaken for a smaller panel.

It costs one call per model plus one to synthesise, so it is a command rather
than a mode.

## Undo

Every write and edit copies what was there first, keyed by session.

```
/rollback              list what changed
/rollback all          put all of it back
/rollback <path>       put one file back
```

The first copy of a file wins, so rolling back returns it to how it was before
the session started rather than before the most recent edit — undoing one step
of five is rarely what "undo this" means. A file that did not exist before is
deleted, because that is what restoring it means.

Bare `/rollback` only lists. Undoing work takes a second word.

## Tuning

| Setting | Default | What it controls |
|---|---|---|
| `agent.repeat_limit` | 3 | Identical calls before the nudge |
| `agent.verify_replies` | off | Check answers against the request |
| `agent.verify_max` | 2 | How many times a turn can be sent back |
| `agent.goal_max_iterations` | 10 | Iterations before a normal goal pauses |
| `agent.goal_autonomous_max_iterations` | 50 | Default cap for a `/goal auto` goal (per-goal 0 = unlimited) |
| `agent.max_turns` | 200 | Hard ceiling on model calls per run |
| `tools.max_tool_calls_per_turn` | 32 | Tool budget before the model is told to wrap up |

The two hard limits are the backstop. Everything above them is about failing
usefully rather than failing quietly.
