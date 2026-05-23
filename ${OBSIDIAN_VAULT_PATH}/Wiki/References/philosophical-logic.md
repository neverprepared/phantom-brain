---
title: Philosophical Logic — Fallback Frameworks for Ambiguous Claims
kind: reference
tags:
  - logic
  - philosophical-logic
  - modal-logic
  - epistemic-logic
  - fuzzy-logic
  - paraconsistent
  - reasoning
  - mcp-brain
created: '2026-05-23T00:09:25.720Z'
updated: '2026-05-23T00:09:25.720Z'
sources: []
---

> **Purpose:** Classical binary logic (true/false) is insufficient for many real-world claims. This reference provides `mcp-brain`'s fallback frameworks when a claim cannot be cleanly evaluated as simply true or false. Consult this when the evaluation layer encounters ambiguity, contradiction, time-sensitivity, or vagueness.

---

## When Classical Logic Fails

Classical logic assumes every proposition is either true or false — the law of excluded middle. In practice, claims from the web are often:

- **Modal** — about possibility or necessity, not current fact ("X could cause Y")
- **Epistemic** — about what is *believed*, not what *is* ("scientists think X")
- **Temporal** — true at one point but not another ("X was the standard in 2020")
- **Vague** — gradient rather than binary ("X is fast", "Y is expensive")
- **Contradictory** — two credible sources say opposite things
- **Normative** — about what *should* be, not what *is*

These cases require extended or deviant logical frameworks rather than binary evaluation.

---

## Decision Tree — Which Framework to Use

```
Is the claim about what IS true right now?
├─ Yes → classical logic, fallacies, razors apply directly
└─ No → which dimension?
    ├─ Possibility / necessity ("could", "must", "might") → Modal Logic
    ├─ Knowledge / belief ("scientists believe", "it is thought") → Epistemic Logic
    ├─ Time-bound ("was", "will be", "used to") → Temporal Logic
    ├─ Ethical / normative ("should", "ought", "right to") → Deontic Logic
    ├─ Vague / gradient ("fast", "large", "often") → Fuzzy Logic
    ├─ Contradicts existing high-confidence atom → Paraconsistent Logic
    └─ Fictional / non-existent entities → Free Logic
```

---

## The Frameworks

### Modal Logic — Possibility and Necessity
**When:** Claim uses "could", "might", "must", "necessarily", "possibly", "would".

**Core distinction:**
- **Possible (◊)** — true in *some* conceivable world
- **Necessary (□)** — true in *all* possible worlds

**Evaluation rule:** A claim that X is *possible* is much weaker than a claim that X *is* true. Store modal claims with explicit tags (`possible`, `hypothetical`) and lower confidence. Never treat "X could cause Y" as equivalent to "X causes Y."

**Example:** "This drug might reduce inflammation" → store as `possible mechanism`, not `established fact`.

---

### Epistemic Logic — Knowledge vs. Belief
**When:** Claim is about what someone *knows* or *believes*, not about objective reality.

**Core distinction:**
- **Knowledge (K)** — requires truth; if known, it is true
- **Belief (B)** — may be false; sincere but not necessarily accurate

**Evaluation rule:** "Scientists believe X" and "X is true" are different claims. The first is factual (they do hold that belief) and can be stored at high confidence. The second is a separate claim requiring its own evidence. Tag epistemic claims as `belief` or `consensus` rather than `fact`.

**Example:** "Most economists believe inflation will fall" → store as `expert consensus 2026`, not `inflation will fall`.

---

### Temporal Logic — Time-Sensitive Claims
**When:** Claim was true at a specific time but may not be now, or makes predictions about the future.

**Operators:**
- **Past (P)** — was true at some past time
- **Future (F)** — will be true at some future time
- **Always-past (H)** — has always been true
- **Always-future (G)** — will always be true

**Evaluation rule:** Time-stamp all claims where the truth value changes over time. A claim about a tool's API, a company's policy, or a scientific consensus has a freshness dimension. This is why TTL exists in the vault — temporal logic is built into the atom lifecycle.

**Example:** "Python 3.11 is the latest version" → store with short TTL, tag `version-specific`.

---

### Deontic Logic — Normative and Ethical Claims
**When:** Claim is about obligation, permission, or prohibition ("should", "must", "ought", "allowed to").

**Core distinction:**
- **Obligatory (O)** — must be done
- **Permitted (P)** — may be done
- **Forbidden (F)** — must not be done

**Evaluation rule:** Normative claims cannot be evaluated as simply true or false — they depend on unstated value premises (see Hume's Guillotine in razors reference). Store them as normative positions, not facts. Flag when a source presents a normative claim as if it were empirical.

**Example:** "Companies should prioritize sustainability" → store as `normative position`, not `factual claim`.

---

### Fuzzy Logic — Vague and Gradient Claims
**When:** Claim uses vague predicates where truth is a matter of degree ("fast", "large", "often", "significant").

**Core concept:** Truth values are continuous between 0 (completely false) and 1 (completely true) rather than binary.

**Evaluation rule:** Don't force a binary true/false judgment on inherently vague claims. Store them with qualifiers and context. "X is fast" is only meaningful relative to a baseline. Require the source to specify the comparison class.

**Example:** "This model is accurate" → request/note the benchmark. Store as `accurate on benchmark Y`, not simply `accurate`.

---

### Paraconsistent Logic — Handling Contradictions
**When:** Two credible sources make directly contradictory claims, or a new claim contradicts an existing high-confidence atom.

**Core concept:** Classical logic explodes when given a contradiction (anything follows from P and ¬P). Paraconsistent logic allows contradictions to coexist without the system becoming useless — it isolates the contradiction rather than propagating it.

**Evaluation rule:** Do not silently overwrite or silently ignore. Store both claims with a `conflict` flag. Let `reflect()` surface them for resolution. The contradiction itself is information — it means the topic is contested, the sources disagree, or one is outdated.

**Example:** Source A says "X framework is faster". Source B says "Y framework is faster". Both can be stored: they may be measuring different things, at different times, or under different conditions. Tag both as `disputed`, link them, and flag for resolution.

---

### Free Logic — Non-Existent or Fictional Entities
**When:** Claim involves entities that may not exist (hypothetical companies, deprecated products, future technologies, fictional scenarios).

**Evaluation rule:** Classical logic assumes every named entity exists. Free logic doesn't. If a claim is about something that may not exist, store it with an `unverified-entity` tag and low confidence until the entity's existence is confirmed.

---

## Storing Philosophically Complex Claims

| Claim type | How to store |
|---|---|
| Modal (possible/necessary) | Tag `hypothetical` or `possible`, lower confidence |
| Epistemic (belief/consensus) | Tag `belief` or `consensus-YEAR`, not `fact` |
| Temporal (time-bound) | Short TTL, tag `version-specific` or `as-of-DATE` |
| Normative (should/ought) | Tag `normative`, store as position not fact |
| Vague (gradient) | Add qualifier/benchmark context to content |
| Contradictory | Tag `disputed`, link both atoms, flag for `reflect()` |

---

## Sources
- https://en.wikipedia.org/wiki/Philosophical_logic
