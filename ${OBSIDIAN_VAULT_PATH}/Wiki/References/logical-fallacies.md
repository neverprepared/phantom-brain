---
title: Logical Fallacies & Formal Logic — Reference
kind: reference
tags:
  - logic
  - fallacies
  - reasoning
  - critical-thinking
  - validation
  - mcp-brain
created: '2026-05-23T00:04:53.098Z'
updated: '2026-05-23T00:04:53.098Z'
sources: []
---

> **Purpose:** Reference for `mcp-brain` evaluation layer. Used when validating incoming claims before committing to memory. The evaluation prompt inlines the Quick Reference; this page is consulted for edge cases and full taxonomy.

---

## Core Logic Principles

### Three Types of Reasoning

| Type | Strength | Description | Example |
|---|---|---|---|
| **Deductive** | Necessary | If premises are true, conclusion must be true | All X are Y; Z is X; therefore Z is Y |
| **Inductive** | Probable | Generalizes from observations; conclusion likely but not certain | Every observed swan is white; therefore all swans are white |
| **Abductive** | Plausible | Inference to best explanation; picks most likely cause | The grass is wet; therefore it probably rained |

### Validity Criteria

An argument is **sound** only when both conditions are met:
1. **Valid structure** — the conclusion follows logically from the premises
2. **True premises** — the premises are actually true

Formal logic focuses on structure (condition 1). Evaluating premises requires domain knowledge and source verification (condition 2). Both matter. A structurally valid argument built on false premises produces false conclusions.

### Formal vs Informal Logic

- **Formal logic** — evaluates argument *structure* using symbolic rules, independent of content
- **Informal logic** — evaluates natural language arguments, including context, ambiguity, and intent

Most web content fails at the informal level: the structure may appear valid while premises are weak, cherry-picked, or emotionally loaded.

---

## Quick Reference — Top 12 (Evaluation Prompt Inline Set)

These are the most frequently encountered fallacies in web articles, opinion pieces, and research claims. Included inline in every evaluation prompt.

| # | Fallacy | One-Line Test |
|---|---|---|
| 1 | **Ad Hominem** | Does it attack the arguer rather than the argument? |
| 2 | **Appeal to Authority** | Is the claim true *only because* an authority said so, without supporting evidence? |
| 3 | **False Dichotomy** | Are only two options presented when more exist? |
| 4 | **Hasty Generalization** | Is a broad conclusion drawn from a small or unrepresentative sample? |
| 5 | **Post Hoc** | Is correlation presented as causation (X happened, then Y; therefore X caused Y)? |
| 6 | **Straw Man** | Does it misrepresent an opposing position to make it easier to attack? |
| 7 | **Appeal to Emotion** | Does it substitute emotional manipulation for logical reasoning? |
| 8 | **Cherry Picking** | Does it use only confirming data while ignoring contradicting evidence? |
| 9 | **Circular Reasoning** | Does the conclusion restate a premise (begging the question)? |
| 10 | **Slippery Slope** | Does it assert a small step necessarily leads to extreme outcomes without justification? |
| 11 | **Appeal to Popularity** | Is the claim true *only because* many people believe it? |
| 12 | **False Equivalence** | Does it treat two dissimilar things as if they are equivalent? |

---

## Full Taxonomy

### FORMAL FALLACIES
Errors in argument *structure* — the logic is broken regardless of whether the content is true.

#### Propositional Fallacies
- **Affirming the Consequent** — "If P then Q; Q is true; therefore P is true" (invalid)
- **Denying the Antecedent** — "If P then Q; P is false; therefore Q is false" (invalid)
- **Affirming a Disjunct** — concluding one disjunct is false because the other is true

#### Quantification Fallacies
- **Existential Fallacy** — universal premise used to draw a particular conclusion

#### Syllogistic Fallacies
- **Undistributed Middle** — middle term not distributed in either premise
- **Illicit Major / Illicit Minor** — term distributed in conclusion but not in premise
- **Fallacy of Four Terms** — syllogism uses four terms instead of three
- **Exclusive Premises** — both premises are negative
- **Negative Conclusion from Affirmative Premises**

#### Other Formal
- **Appeal to Probability** — assuming something must be true because it probably is
- **Argument from Fallacy** — concluding a position is false because one argument for it was fallacious
- **Base Rate Fallacy** — ignoring prior probabilities when evaluating conditional probabilities
- **Conjunction Fallacy** — assuming multiple specific conditions are more probable than one alone
- **Modal Fallacy** — confusing necessity with sufficiency

---

### INFORMAL FALLACIES
Errors in *content, premises, or relevance* — the structure may appear valid but the argument is unsound.

#### Faulty Generalizations
- **Hasty Generalization** — broad conclusion from small/unrepresentative sample
- **Cherry Picking / Confirmation Bias** — using only confirming data; ignoring contradictions
- **Survivorship Bias** — promoting successes while ignoring failures
- **False Analogy** — argument by analogy where the analogy is poorly suited
- **Accident** — applying a general rule to a case that is an exception
- **No True Scotsman** — redefining a generalization to exclude counterexamples
- **Misleading Vividness** — exceptional cases described vividly to suggest they are typical
- **Overwhelming Exception** — generalization so qualified it becomes meaningless

#### Questionable Cause
- **Post Hoc Ergo Propter Hoc** — X preceded Y, therefore X caused Y
- **Cum Hoc Ergo Propter Hoc** — correlation presented as causation
- **Wrong Direction** — cause and effect are reversed
- **Ignoring a Common Cause** — two correlated effects attributed to each other
- **Fallacy of Single Cause** — one cause assumed when multiple are operating
- **Regression Fallacy** — natural fluctuation mistaken for causal effect
- **Gambler's Fallacy** — independent events believed to affect each other
- **Sunk Cost Fallacy** — past investment used to justify continuing a failing course

#### Relevance Fallacies — Red Herrings
- **Ad Hominem** — attacking the person, not the argument
  - *Poisoning the Well* — discrediting a person before they speak
  - *Circumstantial Ad Hominem* — dismissing argument based on arguer's situation
  - *Tu Quoque* — "you do it too" used to dismiss a valid criticism
  - *Tone Policing* — focusing on delivery rather than content
- **Straw Man** — misrepresenting a position to make it easier to attack
- **Appeal to Authority** — claim is true because an authority said so (without supporting evidence)
  - *False Authority* — expert with dubious credentials or outside their domain
  - *Courtier's Reply* — dismissing criticism by demanding credentials
- **Appeal to Emotion** — manipulating feelings instead of presenting reasoning
  - *Appeal to Fear, Pity, Flattery, Ridicule, Spite*
  - *Wishful Thinking* — arguing based on what is pleasing to imagine
- **Appeal to Nature** — natural = good, unnatural = bad
- **Appeal to Tradition** — true because it has long been believed
- **Appeal to Novelty** — superior because it is new
- **Appeal to Popularity (Ad Populum)** — true because many believe it
- **Argument from Ignorance** — true because not proven false (or vice versa)
- **Genetic Fallacy** — conclusion based solely on origin of the argument or arguer
- **Motte-and-Bailey** — defending a modest claim (motte) while advancing a controversial one (bailey)
- **Red Herring** — introducing an irrelevant topic to distract from the original
- **Ipse Dixit** — asserted as true without supporting evidence; self-declared authority

#### Structural / Compositional
- **False Dichotomy / False Dilemma** — only two options when more exist
- **False Equivalence** — treating dissimilar things as equivalent
- **Fallacy of Composition** — what is true of the part must be true of the whole
- **Fallacy of Division** — what is true of the whole must be true of the parts
- **Equivocation** — using a word with multiple meanings without specifying which
  - *Definitional Retreat* — changing a word's meaning when challenged
- **Circular Reasoning / Begging the Question** — conclusion restates a premise
- **Slippery Slope** — small step necessarily leads to extreme outcome without justification
- **Special Pleading** — claiming an exception to a rule without justification
- **Moving the Goalposts** — demanding greater evidence after prior evidence is provided
- **Nirvana Fallacy** — rejecting solutions because they are not perfect
- **Proof by Assertion** — repeating a claim until objections cease
- **Argument to Moderation** — assuming the compromise between two positions is always correct
- **Kettle Logic** — using multiple inconsistent arguments to defend one position

#### Statistical / Empirical
- **P-hacking** — claiming significance without accounting for multiple comparisons
- **Ecological Fallacy** — inferring individual properties from aggregate statistics
- **Texas Sharpshooter** — asserting cause to explain a data cluster found after the fact
- **McNamara Fallacy** — using only quantitative data while discounting qualitative evidence
- **Prosecutor's Fallacy** — low probability of false match ≠ low probability of some false match
- **Ludic Fallacy** — ignoring unknown unknowns when calculating probability

---

## Source Quality Tiers
*(Used alongside fallacy detection for confidence scoring)*

| Tier | Examples | Default confidence |
|---|---|---|
| **Authoritative** | arxiv.org, official docs, peer-reviewed journals, .gov, .edu | High |
| **Credible** | Established news outlets, Wikipedia (as starting point), known industry publications | Medium |
| **Unknown** | Personal blogs, Medium, Substack, forums | Low |
| **Low-quality** | Content farms, SEO-bait, sites with known misinformation patterns | Reject at Layer 1 |

A claim from a low-quality source that *corroborates* a high-confidence atom should still be stored at `low` confidence — the source tier caps the ceiling.

---

## Sources
- https://en.wikipedia.org/wiki/List_of_fallacies
- https://en.wikipedia.org/wiki/Logic
- https://www.britannica.com/topic/formal-logic
- https://yourlogicalfallacyis.com/ (SSL error at fetch time — revisit)
