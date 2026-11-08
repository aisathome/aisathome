<!-- SPDX-License-Identifier: Apache-2.0 -->
# Playbook: proving a Lean statement

For any Lean 4 task with a statement to prove. The task message names the file you hand in and the
command that builds it; where it says otherwise, the task wins.

## The loop: skeleton first, then one hole at a time

1. **Write the skeleton early and build it.** In your first few steps: the imports and `open` lines
   the task gives, then the theorem with exactly the given statement and `sorry` as the proof. A
   skeleton that builds (with only the `sorry` warning) shows that the statement elaborates.
2. **Build after every edit** and read the first error before you change anything else. Fix errors
   in order: a later error is often caused by an earlier one.
3. **Split the proof into `have` steps, each with its own `sorry`**, build, then close one at a time.
   A step that resists gets split again. The file builds at every stage.
4. **Finish:** no `sorry` left, a clean build, and `#print axioms <theorem>` (in a scratch file) shows
   only the axioms the task allows (usually `propext`, `Classical.choice`, `Quot.sound`).

## Ask Lean before you search the source

Lean answers questions about the library faster and more exactly than reading its files. Put the
question in a scratch file (one the check does not read) with the same imports as your file and run
it with `lake env lean <scratch file>`:

```lean
#check @Finset.sum_le_sum                -- a lemma's exact statement and implicit arguments
#print Function.Injective                -- a definition's body
example (a b : ℕ) (h : a ≤ b) : a < b + 1 := by exact?   -- search the library for a proof
example ... := by apply?                                  -- lemmas whose conclusion fits the goal
example ... := by rw?                                     -- rewrites that apply
example ... := by simp?                                   -- the `simp only [...]` call that works
```

- The `?` tactics print `Try this: ...`; paste that into your proof.
- State the exact subgoal as an `example` with only the hypotheses it needs, and search on that.
- `#check` a name before you use it: names change between library versions.
- A search that runs for minutes is too general: add hypotheses or split the goal.
- Use text search (`grep`) for short local files, or when Lean cannot name what you look for.
- The same search with the same empty result will not change: try another name, ask Lean, or go on.

## Keep turns short

One decision, one tool call. Do not plan the whole proof before the first build: the compiler's
errors are cheaper than long reasoning.

## What a proof must not contain

`sorry`, `admit`, `native_decide`, a new `axiom`, `implemented_by`/`extern`, `unsafe`, and
meta-programming (`#eval`, `run_cmd`, `elab`, macros) are refused before the check compiles anything.
If the statement is false as given, say why (with a counterexample) in your report and stop.
