# 2. Relationship strength is computed from signals, not asked of a model

**Status:** accepted

## Context

Relata must decide how strongly two items are related. The obvious approach is to show a
language model both documents and ask for a confidence percentage.

## Decision

Strength is a weighted sum of named, individually-stored signals under a versioned scoring
model. A language model may classify the relationship *type* or describe the connection; its
opinion is **one signal**, weighted 0.15 — below the storage threshold, so it cannot create a
relationship on its own.

## Rationale

A model's percentage cannot be explained, reproduced or debugged. Asked twice it may answer
differently; asked why, it produces a plausible rationalisation rather than the reason. A user
seeing "87%" has no way to judge it, and no recourse when it is wrong.

A weighted signal sum is inspectable: the interface shows every signal, its strength, its
weight and its contribution, and the total is arithmetic the user can check. When a
relationship looks wrong, the reason is visible — and often it is a signal firing incorrectly,
which is fixable.

It is also *stable*. The same evidence yields the same score, so a change in score means a
change in evidence, not a change in the model's mood.

The weights encode judgements that would otherwise be invisible: similarity alone tops out
below the storage threshold; a maximally confident model opinion cannot reach it either; human
confirmation dominates everything. Those are the design's central claims, and as weights they
are both explicit and testable — `scoring_test.go` asserts each one.

## Consequences

**Good:** every score is explainable and reproducible. Changing behaviour means changing a
weight, visibly. The system works without a language model at all.

**Bad:** the weights are judgement, not calibration — which is exactly why the output is called
a *heuristic strength* and never a probability. Signals must be hand-built; a relationship
whose evidence has no corresponding signal is invisible.

**Rejected alternative:** learning weights from user confirmations. Tempting, but it would make
scores unstable, unexplainable, and dependent on a training set that does not exist on first
run. Confirmations influence ranking directly instead.
