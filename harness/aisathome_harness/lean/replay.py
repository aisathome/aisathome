# SPDX-License-Identifier: Apache-2.0
"""The verdict on a compiled proof without loading the proof's code: a kernel replay against a target compiled
without it.

The check that `axioms.check_command` does inside a Lean file has two weak points when the proof is untrusted.
The check file imports the proof's module, so the proof's own initialisers run in the checking process (and can
print the success line), and a target restated in that file is elaborated with everything the proof exports in
scope (a global instance or notation can make it say something else, and `isDefEq` then agrees with the proof).

`PROGRAM` is a small Lean program (`lean --run`) that never imports the proof's module:

1. The target is a module of its own, compiled before and without the proof (the caller's job: a `Targets.*`
   module with the statement under a fixed name, proved by `sorry`). Its compiled constant is the reference.
2. The environment is the proof's own imports plus the target's module, imported without running any
   initialiser or loading any environment extension (as Lean's `leanchecker` does). Each of those imports must
   be a plain name under an allowed root that resolves to a file under the search path's own entries (the trusted
   outputs, the package farm, Lean's library), and the proof must not be a `module` file (whose .olean exports its
   theorems as axioms).
3. The proof's module is read as data (its .olean) and every constant in it is sent to the kernel
   (`Environment.replay`). Only theorems, safe definitions and axioms are accepted (an axiom then fails the axiom
   check); an inductive type, an opaque or unsafe constant, or a name the environment already has is refused.
4. `decl` must be a theorem of the proof's module whose type is the target constant's type: the same expression
   (`same_expr`), or definitionally equal by the kernel (`Kernel.isDefEq`, which knows no instances, notation
   or unification hints). The universe parameters must match in number.
5. Its axioms, collected from the replayed environment (a constant it cannot find is an error, never skipped),
   must be among the allowed ones.

It prints `<MARK>_OK same_expr=<bool> axioms=[...]` or `<MARK>_FAIL <reason>: <detail>` (reasons as
`check_command`'s, plus `not_allowed`, `kernel` and `check_error`), which `axioms.parse_check` reads; exit 0 only
on success. `run` runs it in a jail (sandbox.jail) with only the package farm, the trusted build outputs (the
target's and the definitions' modules, compiled before the proof) and a copy of the proof's .olean mounted, all
read-only; nothing else of the workspace.
Standard library only.
"""
from __future__ import annotations

import os
import shutil
from pathlib import Path

from .. import safefs
from ..sandbox.container import Mount
from ..sandbox.jail import Jail
from .toolchain import JAIL_ELAN, LeanToolchain, packages_dir

IMPORT_ROOTS = ("Mathlib", "Definitions", "Theorems", "Init", "Std", "Batteries", "Aesop", "Qq", "Plausible")
CHK = "/chk"                 # the check's own files inside the jail (read-only)
TRUSTED = "/trusted"         # the build outputs compiled before the proof (read-only)
PKGS = "/pkgs"               # the workspace's package farm (read-only)

PROGRAM = r"""
import Lean
open Lean

def say (s : String) : IO Unit := do
  IO.println s
  (← IO.getStdout).flush

/-- The axioms `c` depends on in `kenv`, through types and values; a constant missing from `kenv` is an error. -/
def axiomsOf (kenv : Kernel.Environment) (c : Name) : Except String (Array Name) := Id.run do
  let mut seen : NameSet := {}
  let mut todo : Array Name := #[c]
  let mut axs : Array Name := #[]
  while h : todo.size > 0 do
    let n := todo[todo.size - 1]
    todo := todo.pop
    if seen.contains n then continue
    seen := seen.insert n
    let some ci := kenv.find? n | return .error s!"unknown constant {n}"
    if ci matches .axiomInfo _ then axs := axs.push n
    for m in ci.getUsedConstantsAsSet do
      unless seen.contains m do todo := todo.push m
  return .ok (axs.qsort Name.lt)

/-- Every component an ordinary identifier: no `«..»`, no path separator, nothing a file path could climb out with. -/
def plainName : Name → Bool
  | .anonymous => true
  | .str p s => plainName p && !s.isEmpty && (s.front.isAlpha || s.front == '_')
      && s.all (fun c => c.isAlphanum || c == '_' || c == '\'')
  | .num _ _ => false

/-- Where `m`'s .olean is, and that it lies under one of `trusted` (real paths): the search path's own entries. -/
def resolvedUnder (trusted : Array System.FilePath) (m : Name) : IO (Option System.FilePath) := do
  let f ← IO.FS.realPath (← findOLean m)
  return if trusted.any (fun t => (t.toString ++ "/").isPrefixOf f.toString) then some f else none

unsafe def check (mark : String) (target decl tgtMod : Name) (olean : System.FilePath) (roots allowed : List Name) :
    IO UInt32 := do
  let fail (reason detail : String) : IO UInt32 := do
    say s!"{mark}_FAIL {reason}: {detail}"
    return 2
  let parts ← readModuleDataParts #[olean]
  let some (mod, _) := parts[0]? | fail "check_error" "no module data in the proof's .olean"
  if mod.isModule then
    -- a `module` file keeps its proofs in .olean.private and exports theorems as axioms; a proof is never one
    return (← fail "not_allowed" "the proof is a `module` file")
  let trusted ← (← searchPathRef.get).toArray.filterMapM fun p => do
    if ← p.isDir then return some (← IO.FS.realPath p) else return none
  for i in mod.imports do
    unless plainName i.module && roots.contains i.module.getRoot do
      return (← fail "not_allowed" s!"the proof imports {i.module}")
  for m in (mod.imports.map (·.module)).push tgtMod do
    if (← resolvedUnder trusted m).isNone then
      return (← fail "not_allowed" s!"{m} resolves outside the trusted modules")
  let imports := mod.imports.push { module := tgtMod }
  let (_, s) ← (importModulesCore imports).run
  let env ← finalizeImport s imports {} 0 false false (isModule := false)
  let mut news : Std.HashMap Name ConstantInfo := {}
  for n in mod.constNames, ci in mod.constants do
    match ci with
    | .thmInfo _ | .axiomInfo _ => pure ()
    | .defnInfo d =>
      unless d.safety matches .safe do
        return (← fail "not_allowed" s!"`{n}` is not a safe definition")
    | _ => return (← fail "not_allowed" s!"`{n}` is not a theorem or a definition")
    if env.contains n then
      return (← fail "not_allowed" s!"`{n}` is declared already")
    news := news.insert n ci
  let env ← try env.replay news catch e => return (← fail "kernel" (toString e))
  let kenv := env.toKernelEnv
  unless news.contains decl do
    return (← fail "no_solution" s!"no top-level declaration `{decl}`")
  let some sol := kenv.find? decl | fail "no_solution" s!"no top-level declaration `{decl}`"
  let some t := kenv.find? target | fail "check_error" s!"target {target} not found"
  unless sol matches .thmInfo _ do
    return (← fail "not_theorem" s!"`{decl}` is not a theorem")
  unless sol.levelParams.length == t.levelParams.length do
    return (← fail "type_mismatch" s!"universe parameters {sol.levelParams} vs {t.levelParams}")
  let solTy := sol.instantiateTypeLevelParams (t.levelParams.map mkLevelParam)
  let same := solTy == t.type
  unless same || Kernel.isDefEqGuarded env {} solTy t.type do
    return (← fail "type_mismatch" s!"`{decl}` does not have the target's type")
  let axs ← match axiomsOf kenv decl with
    | .ok a => pure a
    | .error e => return (← fail "check_error" e)
  let bad := axs.filter (!allowed.contains ·)
  unless bad.isEmpty do
    return (← fail "axioms" s!"{bad.toList}")
  say s!"{mark}_OK same_expr={same} axioms={axs.toList}"
  return 0

unsafe def main (args : List String) : IO UInt32 := do
  match args with
  | mark :: target :: decl :: tgtMod :: olean :: roots :: allowed =>
    initSearchPath (← findSysroot)
    try
      check mark target.toName decl.toName tgtMod.toName olean
        ((roots.splitOn ",").map String.toName) (allowed.map String.toName)
    catch e =>
      say s!"{mark}_FAIL check_error: {e}"
      return 3
  | _ =>
    IO.eprintln "usage: lean --run Replay.lean MARK TARGET DECL TARGET_MODULE OLEAN ROOTS [AXIOM...]"
    return 64
"""


LIB = ".lake/build/lib/lean"      # a workspace's build outputs, relative to the workspace


def lean_path(packages: Path | None) -> str:
    """LEAN_PATH inside the jail: the trusted outputs first, then each package's build outputs (as `lake env`
    lists them), from the farm mounted at PKGS."""
    pk = sorted(p.name for p in Path(packages).iterdir() if p.is_dir() and not p.is_symlink()) \
        if packages is not None and Path(packages).is_dir() else []
    return ":".join([TRUSTED] + [f"{PKGS}/{p}/.lake/build/lib/lean" for p in pk])


def snapshot(ws: Path, dest: Path, dirs: tuple[str, ...]) -> int:
    """Copy workspace `ws`'s build outputs for the module trees `dirs` (e.g. Definitions, Theorems, Targets) to
    `dest`: the trusted outputs for `run`, taken before the proof is compiled in the same workspace. Regular files
    only, read without following a symlink anywhere below `ws` (a jail could write `.lake/build`). Returns the
    number of files copied."""
    if dest.exists():
        shutil.rmtree(dest)
    dest.mkdir(parents=True)
    n = 0
    for d in dirs:
        top = f"{LIB}/{d}"
        if not safefs.is_dir(ws, top):
            continue
        for root, subdirs, files in os.walk(Path(ws) / top):
            rel = Path(root).relative_to(ws)
            subdirs[:] = [s for s in subdirs if not os.path.islink(os.path.join(root, s))]
            (dest / rel.relative_to(LIB)).mkdir(parents=True, exist_ok=True)
            for f in files:
                if os.path.islink(os.path.join(root, f)):
                    continue
                (dest / rel.relative_to(LIB) / f).write_bytes(safefs.read_bytes(ws, rel / f))
                n += 1
    return n


def run(tc: LeanToolchain, image: str, *, ws: Path, trusted: Path, module: str, target: str, target_module: str,
        decl: str, mark: str, allowed: tuple[str, ...], work: Path, roots: tuple[str, ...] = IMPORT_ROOTS,
        timeout: float | None = None, log: Path | None = None, execute=None, **limits) -> tuple[int, str]:
    """Replay the proof `module` (its .olean in workspace `ws`, read without following a symlink) against `target`
    (in `target_module`, compiled into `trusted` by `snapshot`); (exit code, output). `work`: a host directory of
    this check's own (emptied). `execute(jail, argv, timeout, log)` runs the jail (default: Jail.run; a test's
    stand-in). `limits`: the jail's."""
    if work.exists():
        shutil.rmtree(work)
    work.mkdir(parents=True)
    rel = Path(LIB, *module.split(".")).with_suffix(".olean")
    (work / "Proof.olean").write_bytes(safefs.read_bytes(ws, rel, limit=1 << 30))
    (work / "Replay.lean").write_text(PROGRAM)
    if (Path(ws) / "lean-toolchain").is_file():
        (work / "lean-toolchain").write_bytes(safefs.read_bytes(ws, "lean-toolchain"))
    # nothing of the workspace but its package farm (checked: no symlink on the way) and the proof's compiled file
    # (copied above): the jail could have written anything else there
    pk = packages_dir(ws)
    ms = [Mount(Path(tc.elan_home).resolve(), JAIL_ELAN), Mount(Path(trusted).resolve(), TRUSTED),
          Mount(Path(work).resolve(), CHK)] + ([Mount(pk, PKGS)] if pk is not None else [])
    jail = Jail(image, mounts=ms, env={**tc.jail_env(), "LEAN_PATH": lean_path(pk)}, workdir=CHK, **limits)
    argv = ["lean", "--run", f"{CHK}/Replay.lean", mark, target, decl, target_module, f"{CHK}/Proof.olean",
            ",".join(roots), *allowed]
    if execute is not None:
        return execute(jail, argv, timeout, log)
    return jail.run(argv, timeout=timeout, log=log)
