---
title: Philosophical Razors — Decision Heuristics Reference
kind: reference
tags:
  - logic
  - razors
  - heuristics
  - reasoning
  - critical-thinking
  - validation
  - mcp-phantom-brain
created: '2026-05-23T00:08:48.911Z'
updated: '2026-05-23T00:08:48.911Z'
sources: []
---

> **Purpose:** Razors are decision heuristics used by `mcp-phantom-brain` when choosing between competing explanations or evaluating whether a claim warrants serious consideration. Where fallacies identify *what's wrong* with an argument, razors guide *which explanation to prefer* when evidence is ambiguous.

---

## What Is a Razor?

A razor is a principle that "shaves off" unlikely explanations — a rule of thumb for cutting through complexity when you have multiple plausible interpretations. Razors don't prove conclusions; they bias toward simpler, more conservative, or more empirically grounded ones.

They function as **priors** in the evaluation layer: before deeper analysis, they establish a default preference.

---

## The Razors

### Occam's Razor
**Rule:** Among competing explanations with equal predictive power, prefer the one requiring fewer assumptions.

**Application:** When two claims explain the same evidence equally well, the simpler one is more likely correct. Does this claim introduce unnecessary entities, mechanisms, or conspiracies?

**Evaluation signal:** If a claim requires many unstated assumptions to hold, confidence should be lower than a simpler competing claim.

---

### Hitchens's Razor
**Rule:** What can be asserted without evidence can be dismissed without evidence.

**Application:** Burden of proof lies with the person making the claim. Absence of counter-evidence is not the same as presence of supporting evidence.

**Evaluation signal:** If a claim provides no evidence, it can be stored at `low` confidence or rejected outright — not because it's necessarily false, but because it's unverified.

---

### Sagan Standard
**Rule:** Extraordinary claims require extraordinary evidence.

**Application:** Calibrate the evidentiary threshold to the implausibility of the claim. A claim that contradicts well-established knowledge needs proportionally stronger evidence.

**Evaluation signal:** If a claim contradicts a `high`-confidence atom, the incoming evidence must itself be from authoritative sources. A low-quality source cannot overturn a high-confidence atom regardless of what it asserts.

---

### Hanlon's Razor
**Rule:** Never attribute to malice what can be adequately explained by incompetence (or ignorance).

**Application:** When evaluating claims about intent, bias toward the mundane explanation. Most errors are mistakes, not conspiracies.

**Evaluation signal:** Claims asserting coordinated deception or intentional harm require stronger evidence than claims asserting simple error or oversight.

---

### Popper's Falsifiability Criterion
**Rule:** A claim that cannot in principle be proven false is not a scientific claim — it may still be meaningful, but it cannot be verified empirically.

**Application:** Distinguish empirical claims (testable) from philosophical or normative ones (non-testable). Non-falsifiable claims should not be stored as facts; store them as opinions, frameworks, or beliefs.

**Evaluation signal:** If a claim is structured so no possible evidence could contradict it, assign `low` confidence and tag appropriately. Do not treat it as an established fact.

---

### Hume's Guillotine (Is-Ought Problem)
**Rule:** Prescriptive conclusions ("ought") cannot be derived solely from descriptive facts ("is").

**Application:** A claim that moves from factual observations to moral or normative conclusions without stating the normative premise is making a hidden assumption.

**Evaluation signal:** Flag claims that jump from "X is the case" to "therefore we should do Y" without stating the value premise. The factual part may be true; the normative conclusion is a separate claim.

---

### Alder's Razor (Newton's Flaming Laser Sword)
**Rule:** If something cannot be settled by experiment or observation, it is not worth debating as a matter of fact.

**Application:** Demarcates questions that can be answered empirically from those that cannot. Unfalsifiable debates should be stored as philosophical positions, not factual claims.

**Evaluation signal:** If the claim is inherently untestable and the source presents it as established fact, reject or downgrade confidence.

---

### Grice's Razor
**Rule:** When interpreting language, prefer the simpler pragmatic explanation over a complex semantic one.

**Application:** When a claim's meaning is ambiguous, interpret it as the speaker most likely intended rather than as the most literal or most extreme reading.

**Evaluation signal:** Useful when the same source text could support multiple interpretations — choose the most plausible intent before evaluating truth.

---

## Application Order in Evaluation

When evaluating an incoming claim, apply razors in this sequence:

```
1. Alder / Popper  → Is this claim even testable? If not, flag as non-empirical.
2. Hitchens        → Does it provide any evidence? If not, confidence = low.
3. Sagan           → Is this extraordinary? If so, raise the evidence bar.
4. Occam           → Is there a simpler explanation? Prefer it.
5. Hanlon          → Does this claim require malicious intent? Require stronger evidence.
6. Hume            → Does this jump from facts to values? Flag the hidden premise.
7. Grice           → Am I interpreting this fairly? Use charitable reading.
```

---

## Sources
- https://en.wikipedia.org/wiki/Philosophical_razor
