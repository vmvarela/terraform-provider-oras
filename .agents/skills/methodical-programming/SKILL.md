---
name: methodical-programming
description: Apply rigorous, mathematically-grounded program construction and verification. Derive correct programs from formal pre/post specifications using axiomatic semantics, structural induction, recursive design with bounding functions, algorithm immersion, and iterative derivation with loop invariants. Language-agnostic. Use this skill whenever the user wants to implement a function correctly, reason about loops or recursion, write formal specifications, derive tests from postconditions, prove termination, design data structures algebraically, or ensure program correctness by construction — even if they don't use terms like "formal methods" or "specification".
---

# Methodical Programming (Programación Metódica)

Apply rigorous, mathematically-grounded program construction and verification techniques. Programs are derived from formal specifications rather than written ad-hoc and tested afterwards. This skill is language-agnostic and applies to any programming paradigm.

## Core Principles

### 1. Specification Before Code

Every function/procedure MUST be specified with:

- **Precondition (Pre):** Conditions the inputs must satisfy.
- **Postcondition (Post):** Relations that must hold between inputs and outputs.

```
function f(params) returns results
  {Pre: conditions on params}
  {Post: relations between params and results}
```

A program is **correct** when its actual behavior matches its specification. The weaker the precondition, the more reusable the function. The stronger the postcondition, the more useful the function.

### 2. Derivation Over Verification

- **Verification:** Proving an existing program meets its specification (post-hoc).
- **Derivation:** Constructing a program that is correct **by construction** from its specification.

Always prefer derivation. Derive code from the postcondition analysis:

- **Equalities** between program variables and expressions → solve via assignments.
- **Disjunctions** in postcondition → design an alternative/conditional (if/match/switch).
- **Conjunctions** in postcondition → attempt sequential composition or conditional branches.

### 3. States, Assertions and Substitutions

- A **state** is the mapping of all variables to their current values at a given program point.
- An **assertion (aserto)** is a logical expression over program variables describing a set of valid states.
- **Substitution** `A[x ← E]`: replace every free occurrence of `x` in assertion `A` with expression `E`. This is the key operation for reasoning about assignments.

---

## Instruction Semantics (Axiomatic)

Apply these rules regardless of language syntax.

### Skip / No-op
```
{A} skip {A}
```
Everything true before is true after.

### Assignment
```
{A[x ← E]} x := E {A}
```
To prove `{P} x := E {Q}`, demonstrate:
1. Expression `E` can be evaluated without errors.
2. `P ⟹ Q[x ← E]` (the precondition implies the postcondition with `x` replaced by `E`).

### Multiple Assignment (Simultaneous)
```
<x₁, x₂, ..., xₙ> := <E₁, E₂, ..., Eₙ>
```
All expressions are evaluated using values **before** the assignment. This is important when variables appear in expressions on the right side.

### Sequential Composition
```
{A₁} P₁ ; P₂ {A₃}
```
Find an intermediate assertion `A₂` such that `{A₁} P₁ {A₂}` and `{A₂} P₂ {A₃}`.

### Conditional / Alternative
```
{Pre}
if B₁ → S₁
   B₂ → S₂
   ...
   Bₙ → Sₙ
end if
{Post}
```
Verify:
1. **Completeness:** `Pre ⟹ B₁ ∨ B₂ ∨ ... ∨ Bₙ` (at least one branch is open).
2. **Correctness per branch:** `{Pre ∧ Bᵢ} Sᵢ {Post}` for each `i`.

### Function Call
```
<x₁, ..., xₘ> := f(e₁, ..., eₖ)
```
1. Before the call: prove the arguments satisfy the function's precondition.
2. After the call: the result variables satisfy the postcondition (with parameter/result renaming).

---

## Quantifiers and Hidden Operations

Use quantifiers to express specifications concisely. Each has a **neutral element** (value on empty domain):

| Quantifier | Symbol | Neutral | Type    | Description |
|------------|--------|---------|---------|-------------|
| Summation  | Σ      | 0       | numeric | Sum over domain |
| Product    | Π      | 1       | numeric | Product over domain |
| Universal  | ∀      | true    | boolean | Conjunction (and) over domain |
| Existential| ∃      | false   | boolean | Disjunction (or) over domain |
| Counter    | N      | 0       | natural | Count where predicate holds |
| Maximum    | MAX    | —       | numeric | Max value over domain |
| Minimum    | MIN    | —       | numeric | Min value over domain |

**Splitting a quantifier** — separate one element from the domain and combine with the binary operation:
```
Σ(a: 1≤a≤n: f(a)) = Σ(a: 1≤a≤n-1: f(a)) + f(n)
MAX(a: 1≤a≤n: f(a)) = max(MAX(a: 1≤a≤n-1: f(a)), f(n))
```

The neutral element defines the base case for recursion and the initialization for iteration.

---

## Algebraic Data Types

When using a data type, reason through its **algebraic specification**:

1. **Signature:** Types (genres) and operations with their arities.
2. **Equations:** Algebraic identities that define operation behavior.
3. **Constructors:** Minimal set of operations that can build any value of the type (used as the basis for induction).

### Common Structures

**List:** Constructors `[]` (empty) and `e:l` (cons). Operations: `++` (concat), `length`.
```
[] ++ l₂ ≡ l₂
(e:l₁) ++ l₂ ≡ e:(l₁ ++ l₂)
length([]) ≡ 0
length(e:l) ≡ 1 + length(l)
```

**Stack (LIFO):** Constructors `empty_stack` and `push(e, s)`. Operations: `top`, `pop`, `is_empty`.
```
top(empty_stack) ≡ error
top(push(e, s)) ≡ e
pop(push(e, s)) ≡ s
is_empty(empty_stack) ≡ true
is_empty(push(e, s)) ≡ false
```

**Queue (FIFO):** Constructors `empty_queue` and `enqueue(e, q)`. Operations: `front`, `dequeue`, `is_empty`.

**Binary Tree:** Constructors `empty_tree` and `plant(e, left, right)`. Operations: `root`, `left`, `right`, `is_empty`, `height`, `size`.
```
height(empty_tree) ≡ 0
height(plant(e, a₁, a₂)) ≡ 1 + max(height(a₁), height(a₂))
size(plant(e, a₁, a₂)) ≡ 1 + size(a₁) + size(a₂)
```

**Table/Map:** Constructors `create` and `assign(t, index, value)`. Operations: `lookup`, `defined`.

Use the constructor set to identify the **induction structure** of each type.

---

## Principle of Induction

To prove properties of data types, identify the **constructors** and reason by structural induction:

```
[P(base) ∧ ∀x (P(smaller(x)) ⟹ P(x))] ⟹ ∀z P(z)
```

- **Base case:** Prove the property for values built with nullary constructors (empty list, empty stack, etc.).
- **Inductive step:** Assuming the property holds for all sub-structures, prove it for the composed structure.

A **well-founded preorder** (noetherian) is required: no infinite strictly decreasing sequences exist. This guarantees that induction and recursion terminate.

---

## Recursive Programs

### Pattern

Every recursive function follows this structure:

```
function f(x) returns r
  {Pre: Q(x)}
  if d(x) →          // direct case
    r := h(x)
  ¬d(x) →            // recursive case
    v := f(s(x))
    r := c(x, v)
  end if
  {Post: R(x, r)}
  return r
```

### Verification Steps

1. **Direct case correctness:** `Q(x) ∧ d(x) ⟹ R(x, h(x))`
2. **Recursive case — legal call:** `Q(x) ∧ ¬d(x) ⟹ Q(s(x))` (precondition holds for recursive argument)
3. **Recursive case — correctness:** `Q(x) ∧ ¬d(x) ∧ R(s(x), v) ⟹ R(x, c(x, v))`
4. **Bounding function `t(x)`:** Define `t: params → ℕ` such that:
   - `Q(x) ⟹ t(x) ∈ ℕ` (always a natural number)
   - `Q(x) ∧ ¬d(x) ⟹ t(s(x)) < t(x)` (strictly decreases at each recursive call)

### Bounding Function Heuristics

| Parameter type | Typical bounding function |
|---------------|--------------------------|
| Natural number `n` decreasing | `n` itself |
| Two naturals, one always decreases | `m + n` |
| Two naturals, one decreases, other grows but never reaches it | `max(m, n)` |
| Boolean `b` changing `true → false` | `if b then 1 else 0` |
| Boolean `b` changing `false → true` | `1 - (if b then 1 else 0)` |
| Stack | `height(stack)` |
| Queue | `length(queue)` |
| Binary tree | `size(tree)` or `height(tree)` |
| List | `length(list)` |
| Interval `[i, j]` shrinking | `j - i` |

### Recursion Types

- **Linear recursion:** Each call generates at most one recursive call.
- **Multiple recursion:** A call may generate more than one recursive call (e.g., tree traversals).

### Methodology for Designing Recursive Programs

1. **Specify** the function: header, precondition, postcondition.
2. **Identify the bounding function** `t(x)` — the expression for induction over ℕ.
3. **Analyze cases:** Identify at least one direct case and one recursive case. Ensure all cases are covered.
4. **Program and verify each case.** For recursive cases, assume (by induction hypothesis) that recursive calls satisfy the specification.
5. **Validate termination:** Prove `t(x)` decreases strictly at each recursive call.

---

## Algorithm Immersion (Generalization)

When a direct recursive solution is not found, not efficient, or hard to reason about, **generalize** the function by adding parameters and/or results. This is called **immersion**.

### Techniques

#### Strengthen the Precondition
1. **Weaken the postcondition** (possibly introducing new variables).
2. **Strengthen the precondition** with the weakened postcondition — require a partial version of the result as input.
3. **Add immersion parameters** so the new precondition makes sense.
4. **Keep the original postcondition.**

The weakened postcondition provides ideas for:
- **Base case guard:** The condition that "inverts" the weakening.
- **Recursive case:** Only immersion parameters change between calls → **tail recursion** with constant postcondition.

#### Weaken the Postcondition
1. Introduce new variables replacing sub-expressions in the postcondition.
2. Drop some equalities → weaker requirement.
3. The dropped equalities become the **initializations** to recover the original function from the immersion.
4. Strengthen the precondition with domain conditions on the new parameters as needed.

#### Efficiency Immersion
When a complex expression `f(x)` must be recomputed at every recursive call:
- **As extra result:** Add `w = f(x)` to the postcondition. After a recursive call with `s(x)`, update `w` from `f(s(x))` to `f(x)`.
- **As extra parameter:** Add `w = f(x)` to the precondition. Use `w` instead of computing `f(x)`. When making recursive calls, compute the new parameter value from the current `w`.

#### Unfold/Fold Technique (Tail Recursion Transformation)
Transform linear recursion into tail recursion:

1. Given `f(x) = c(f(s(x)), x)` (linear recursive case), define immersion `g(y, w) = c(f(y), w)`.
2. **Unfold:** Substitute `f`'s cases into `g`'s definition.
3. **Fold:** Manipulate the expression back into `g`'s form.
4. The result is a tail-recursive `g`, with `f(x) = g(x, neutral)` where `neutral` is the identity element of `c`.

---

## Iterative Programs (Loops)

### While Loop Semantics

```
{Invariant I}
while B do
  S
end while
{Post: Q}
```

### Verification (Two Phases)

**Phase 1 — Partial Correctness:**
1. Define an **invariant** `I`: a condition that holds at the start of every iteration.
2. **Initialization:** `Pre ⟹ I` (the precondition implies the invariant).
3. **Maintenance:** `{I ∧ B} S {I}` (the body preserves the invariant).
4. **Exit:** `I ∧ ¬B ⟹ Post` (invariant + loop exit condition implies postcondition).

**Phase 2 — Termination:**
1. Define a **bounding function** `t` with values in ℕ.
2. **Bounded:** `I ∧ B ⟹ t > 0`.
3. **Decreasing:** `{I ∧ B ∧ t = T} S {t < T}` (the body strictly decreases the bound).

### Transformation: Tail Recursion → Iteration

A tail-recursive function with **constant postcondition** transforms directly into a while loop:

| Recursive element | Iterative element |
|-------------------|-------------------|
| Precondition of immersed function | Loop **invariant** |
| Bounding function (preorder) | Loop **bounding function** |
| Immersion parameters | Loop **local variables** |
| Recursive case guard | Loop **condition** `B` |
| Parameter update in recursive call | Loop **body** |
| Direct case body | Code **after** the loop |
| Initial call arguments | Loop **initialization** (before the while) |

### Direct Iterative Derivation

1. The **invariant core** is typically a weakened postcondition.
2. Design guarded instructions that **preserve** the invariant and **decrease** the bounding function.
3. Sufficient cases are covered when: `Invariant ∧ ¬(B₁ ∨ B₂ ∨ ... ∨ Bₙ) ⟹ Post`.

---

## Workflow Summary

When writing any function, follow this process:

1. **Specify:** Write precondition and postcondition. Identify input/output types.
2. **Analyze the postcondition:** Determine if the solution requires assignment, conditionals, recursion, or iteration.
3. **Choose strategy:**
   - Simple postcondition with equalities → direct assignment/composition.
   - Disjunctions → conditional/alternative.
   - Inductive structure → recursion or iteration.
4. **For recursive solutions:**
   - Identify cases (direct + recursive).
   - Define bounding function.
   - Verify each case.
   - Consider immersion if needed for efficiency or tail recursion.
5. **For iterative solutions:**
   - Derive invariant (weakened postcondition).
   - Define bounding function.
   - Design loop body that preserves invariant and decreases bound.
   - Set initializations and post-loop code.
6. **Verify:** Ensure all proof obligations are met — or derive code so they hold by construction.

---

## Applying This Skill

When generating or reviewing code in **any language**:

- Always think in terms of **preconditions and postconditions**, even if the language doesn't enforce them. Document them as comments, assertions, type constraints, or contracts.
- Use **assertions/contracts** available in the language (`assert`, `require`, `ensure`, design-by-contract libraries, property-based tests) to encode pre/post conditions.
- When designing recursive functions, **explicitly identify** the bounding function and verify termination.
- When designing loops, **explicitly state the invariant** and bounding function.
- Prefer **deriving** code from specifications over writing code and testing afterwards.
- When a recursive solution is not tail-recursive, consider **immersion** techniques to transform it.
- Use **algebraic reasoning** over data structures: define operations by their equations, reason by structural induction.
- Encode bounding functions as **decreasing measures** where the language supports it (e.g., Dafny's `decreases`, Coq's structural recursion, or manual assertions).

---

## Connecting Formal Specifications to Tests

Formal specifications (preconditions and postconditions) and tests express the same idea in different languages: one in logic, the other in executable code. Deriving tests from the specification — rather than from the implementation — closes the loop: the tests are correct by the same reasoning that makes the code correct.

### 1. Specification → Tests

Every element of a formal specification maps to a concrete test:

| Formal element | Test equivalent |
|---|---|
| Precondition violated | Unhappy-path test — assert the function signals an error |
| Postcondition equality `r = E` | Equality assertion (`toBe`, `expectEqual`, `assert { condition = ... }`) |
| Postcondition disjunction `A ∨ B` | One test per branch |
| Base constructor (`[]`, `empty`, `0`) | Base-case test — verifies the quantifier's neutral element |
| Inductive constructor (`e:l`, `plant(...)`) | Test with the minimum non-trivial value |

**Worked example — `sum(list)`**

```
function sum(list) returns result
  {Pre:  list is a list of numbers}
  {Post: result = Σ(a: a ∈ list: a)}
```

The neutral element of `Σ` is `0`, so the base case is `sum([]) = 0`. The inductive constructor `e:l` produces the minimal test `sum([5]) = 5`, and the general case `sum([1, 2, 3]) = 6`.

**TypeScript (Jest / Vitest)**

```typescript
// sum.test.ts
import { sum } from "./sum";

it("returns 0 for empty list — neutral element of Σ", () => {
  expect(sum([])).toBe(0);
});

it("returns the element itself for a singleton — minimal inductive case", () => {
  expect(sum([5])).toBe(5);
});

it("returns the total for a general list", () => {
  expect(sum([1, 2, 3])).toBe(6);
});

it("rejects non-numbers — precondition violation", () => {
  expect(() => sum(["a"] as any)).toThrow();
});
```

**Go (testing)**

```go
// sum_test.go
func TestSumEmptyList(t *testing.T) {
    // Base case: neutral element of Σ is 0
    if got := Sum([]int{}); got != 0 {
        t.Errorf("Sum([]) = %d, want 0", got)
    }
}

func TestSumSingleton(t *testing.T) {
    // Minimal inductive case: one recursive step falling into base case
    if got := Sum([]int{5}); got != 5 {
        t.Errorf("Sum([5]) = %d, want 5", got)
    }
}

func TestSumGeneralList(t *testing.T) {
    if got := Sum([]int{1, 2, 3}); got != 6 {
        t.Errorf("Sum([1,2,3]) = %d, want 6", got)
    }
}
```

### 2. Testing Recursive Cases

Every recursive function has the structure:

```
function f(x):
  if d(x)  → h(x)           -- direct case
  ¬d(x)    → c(x, f(s(x)))  -- recursive case
```

This structure directly determines the test suite:

- **Direct case** (`d(x)` is true): test the base case. The expected value is the neutral element of the quantifier that the postcondition expresses.
- **Minimal recursive case** (`¬d(x)` with `s(x)` falling into the direct case): test with the smallest input that triggers exactly one recursive call.
- **General case**: test with an input that triggers multiple recursive calls.
- **Bounding function `t(x)`**: when termination is non-obvious, use a call counter or spy to confirm the number of recursive calls does not exceed `t(x_initial)`.

**Worked example — `length(list)`**

```
function length(list) returns n
  {Pre:  list is a list}
  {Post: n = N(a: a ∈ list: true)}
  if list = []  → n := 0                      -- d(x): list is empty
  list ≠ []     → n := 1 + length(tail(list)) -- recursive case
```

Bounding function: `t(list) = length(list)` (the size of the input structure).

**TypeScript (Jest / Vitest)**

```typescript
// length.test.ts
import { length } from "./length";

it("returns 0 for empty list — direct case, neutral of N", () => {
  expect(length([])).toBe(0);
});

it("returns 1 for singleton — one recursive call into base", () => {
  expect(length([7])).toBe(1);
});

it("returns correct length for general list", () => {
  expect(length([1, 2, 3])).toBe(3);
});
```

**Go (testing)**

```go
// length_test.go
func TestLengthEmpty(t *testing.T) {
    if got := Length([]int{}); got != 0 {
        t.Errorf("Length([]) = %d, want 0", got)
    }
}

func TestLengthSingleton(t *testing.T) {
    if got := Length([]int{7}); got != 1 {
        t.Errorf("Length([7]) = %d, want 1", got)
    }
}

func TestLengthGeneral(t *testing.T) {
    if got := Length([]int{1, 2, 3}); got != 3 {
        t.Errorf("Length([1,2,3]) = %d, want 3", got)
    }
}
```

**Zig (std.testing + comptime table)**

Zig's idiomatic pattern for multiple cases is `inline for` over a `comptime` array — zero-cost parametric tests with no external library:

```zig
const std = @import("std");
const length = @import("length").length;

test "length — direct case, recursive case, general" {
    const cases = comptime [_]struct {
        input: []const i32,
        expected: usize,
    }{
        .{ .input = &[_]i32{},        .expected = 0 }, // direct case: d(x)
        .{ .input = &[_]i32{7},       .expected = 1 }, // minimal recursive case
        .{ .input = &[_]i32{ 1, 2, 3 }, .expected = 3 }, // general case
    };

    inline for (cases) |c| {
        try std.testing.expectEqual(c.expected, length(c.input));
    }
}
```

### 3. Property-Based Testing for Universal Postconditions

When the postcondition contains a universal quantifier (`∀`), manual tests verify only finitely many cases of an infinite truth. Property-based testing (PBT) generates random inputs, checks the property for each, and — on failure — shrinks the counterexample to the minimal failing case.

**Rule:** if the postcondition has the form `∀x ∈ domain: P(x)`, write a property test, not just examples.

**Worked example — algebraic properties of `reverse`**

```
{Post₁: ∀l: length(reverse(l)) = length(l)}   -- reverse preserves length
{Post₂: ∀l: reverse(reverse(l)) = l}           -- reverse is an involution
```

**TypeScript — fast-check**

```typescript
// reverse.property.test.ts
import * as fc from "fast-check";
import { reverse } from "./reverse";

test("Post₁: reverse preserves length — ∀l", () => {
  fc.assert(
    fc.property(fc.array(fc.integer()), (l) => {
      return reverse(l).length === l.length;
    })
  );
});

test("Post₂: reverse is an involution — ∀l", () => {
  fc.assert(
    fc.property(fc.array(fc.integer()), (l) => {
      const rr = reverse(reverse(l));
      return rr.length === l.length && l.every((v, i) => v === rr[i]);
    })
  );
});
```

**Go — rapid**

```go
// reverse_property_test.go
import (
    "testing"
    "pgregory.net/rapid"
)

func TestReversePreservesLength(t *testing.T) {
    // Post₁: ∀l: length(reverse(l)) = length(l)
    rapid.Check(t, func(t *rapid.T) {
        l := rapid.SliceOf(rapid.Int()).Draw(t, "l")
        if len(Reverse(l)) != len(l) {
            t.Fatalf("length(Reverse(%v)) ≠ length(%v)", l, l)
        }
    })
}

func TestReverseIsInvolution(t *testing.T) {
    // Post₂: ∀l: reverse(reverse(l)) = l
    rapid.Check(t, func(t *rapid.T) {
        l := rapid.SliceOf(rapid.Int()).Draw(t, "l")
        got := Reverse(Reverse(l))
        if len(got) != len(l) {
            t.Fatalf("len(Reverse(Reverse(%v))) = %d, want %d", l, len(got), len(l))
        }
        for i := range l {
            if got[i] != l[i] {
                t.Fatalf("Reverse(Reverse(%v)) = %v, want %v", l, got, l)
            }
        }
    })
}
```

**Zig — comptime table + built-in fuzzer**

Zig has no idiomatic PBT library. Use two complementary approaches:

1. **`inline for` over `comptime` array** for representative cases derived from the specification (base, minimal inductive, edge values).
2. **`zig test --fuzz`** for no-crash / no-panic invariants — the built-in fuzzer feeds arbitrary bytes and catches panics, out-of-bounds accesses, and undefined behavior.

```zig
const std = @import("std");
const reverse = @import("reverse").reverse;

// Post₁ + Post₂: representative cases covering base and inductive constructors
test "reverse — spec-derived cases" {
    const allocator = std.testing.allocator;
    const cases = comptime [_]struct {
        input: []const i32,
        expected: []const i32,
    }{
        .{ .input = &[_]i32{},        .expected = &[_]i32{} },
        .{ .input = &[_]i32{1},       .expected = &[_]i32{1} },
        .{ .input = &[_]i32{ 1, 2, 3 }, .expected = &[_]i32{ 3, 2, 1 } },
    };

    inline for (cases) |c| {
        const result = try reverse(allocator, c.input);
        defer allocator.free(result);
        try std.testing.expectEqual(c.expected.len, result.len);
        try std.testing.expectEqualSlices(i32, c.expected, result);
    }
}

// Post₁ no-panic invariant: reverse must not crash on any input
test "reverse — fuzz no-panic" {
    const input = std.testing.fuzzInput(.{});
    const allocator = std.testing.allocator;
    const n = input.len / @sizeOf(i32);

    // Decode fuzz bytes into i32 values without assuming alignment.
    var buf = std.ArrayList(i32).init(allocator);
    defer buf.deinit();
    try buf.ensureTotalCapacity(n);

    var i: usize = 0;
    while (i < n) : (i += 1) {
        const base = i * @sizeOf(i32);
        const chunk = input[base .. base + @sizeOf(i32)];
        const value = std.mem.bytesToValue(i32, chunk);
        buf.appendAssumeCapacity(value);
    }

    const data: []const i32 = buf.items;
    const result = reverse(allocator, data) catch return;
    defer allocator.free(result);
    try std.testing.expectEqual(data.len, result.len);
}
// Run with: zig test reverse_test.zig --fuzz
```

**Terraform — native `terraform test` (`.tftest.hcl`)**

Terraform has no PBT. Universal postconditions (`∀`) are expressed with `alltrue([for ... : ...])` inside an `assert` block. Boundary cases and precondition violations each get a separate `run` block — there are no loops in `.tftest.hcl`.

```hcl
# tests/subnets_unit_test.tftest.hcl

# Post: ∀subnet ∈ aws_subnet.private: subnet.cidr_block ∈ "10.0.0.0/8"
run "all_private_subnets_in_range" {
  command = plan

  assert {
    condition = alltrue([
      for s in aws_subnet.private : can(regex("^10\\.0\\.", s.cidr_block))
    ])
    error_message = "All private subnets must be in the 10.0.x.x range."
  }
}

# Pre violated: cidr_block outside allowed range must be rejected
run "invalid_cidr_rejected" {
  command = plan

  variables {
    cidr_block = "192.168.1.0/24"
  }

  expect_failures = [var.cidr_block]
}

# Boundary: minimum allowed subnet count
run "minimum_subnet_count" {
  command = plan
  variables { subnet_count = 1 }

  assert {
    condition     = length(aws_subnet.private) == 1
    error_message = "Expected exactly 1 subnet."
  }
}

# Boundary: zero subnets must be rejected by variable validation
run "zero_subnets_rejected" {
  command = plan
  variables { subnet_count = 0 }

  expect_failures = [var.subnet_count]
}
```

### 4. The Combined Workflow

Formal derivation and TDD are not alternatives — they are two layers of the same process. The specification is the source of truth; both the code and the tests are derived from it independently.

```
1. Specify   Write Pre and Post formally.
             Identify the quantifier structure (Σ, ∀, N, ...) and its neutral element.
             Identify constructors of the input type (base + inductive).

2. Red       Translate Pre/Post into tests — all must fail (code does not exist yet):
             · Pre violated              → unhappy-path test / expect_failures
             · Base constructor          → test of the quantifier's neutral element
             · Minimal inductive case    → test with one recursive step into base
             · Postcondition with ∀      → property test (fast-check / rapid)
                                           or alltrue([for...]) in Terraform
             · Each postcondition branch → one test per disjunct

3. Derive    Construct the code from the postcondition analysis (not from the tests).
             Choose the strategy that matches the postcondition structure:
             equalities → assignment, disjunctions → conditional, inductive → recursion/loop.

4. Green     The tests pass on the first run — by construction, not by adjustment.
             If a test fails, the derivation has a gap: re-examine the proof obligation.

5. Refactor  Improve structure, names, efficiency. The tests stay green.
             The formal invariant guarantees that refactoring preserves correctness.
             Immersion (tail-recursion transformation) is a safe refactoring step
             because the postcondition is unchanged.
```

**Key insight:** when code and tests are both derived from the same specification, the red-green-refactor cycle collapses into **specify → derive → verify**. Tests written against the specification — not against the implementation — cannot be accidentally made to pass by wrong code that happens to match the test. They fail for the right reason and pass for the right reason.
