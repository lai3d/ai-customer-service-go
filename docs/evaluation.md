# Measuring the answers

Retrieval was measured — 20/20 paraphrases, 4/4 cross-lingual, a threshold shown to be
useless — and the answers were not. A prompt edit, a model upgrade or a corpus change could
have made the product worse and every test would have stayed green, because every test
asserts against a stub. For something customers talk to, this is the measurement that
decides whether it is usable.

```sh
make eval           # 35 cases against the real model
make eval-control   # the same cases with no corpus: the negative control
```

## The numbers

| | Cases passed | Cost | Duration |
| --- | --- | --- | --- |
| **`claude-opus-5`, corpus ingested** | **34–35/35 across six runs** | ~$0.52 | ~2m10s |
| the same run with **no corpus** | **15/35 (42.9%)** | $0.46 | 3m02s |

About 74,600 input and 6,100 output tokens per graded run, measured 2026-09-06.

**A range, not a number, and the first version of this document had it wrong.** It said
35/35 (100%) on the strength of one run. Six runs later the score has been 35 four times and
34 twice, with two *different* cases failing once each — and both passed when re-run alone.
That is the variance this document already listed as something it could not see, observed
rather than hypothesised, and the honest thing is a range with the reason attached rather
than the best sample presented as a property.

Retrieval reads the active corpus version, as production does — an earlier run of this
suite went through the unversioned fallback path and would have measured a query no
deployment uses.

**The second row is why the first one means anything.** A suite that scores 100% has told
you nothing until the same harness has been shown to produce a bad number — otherwise it
may be measuring how plausible a large model sounds rather than whether this system is
grounded in this corpus. `make eval-control` leaves the corpus out and the score collapses
by 57 points, which is what "the answers come from the corpus" looks like as a measurement
rather than as a claim.

The control is expected to fail its cases and exits 0 deliberately: a successful
demonstration should not look like a broken suite.

## What is asserted

Every check is mechanical. There is no model grading another model's output here, which
costs nothing to run and buys reproducibility — the same answer scores the same way twice.

| Check | What it catches |
| --- | --- |
| `mustContain` | The number from the corpus. 30 days, 48 hours, 34 countries, 3 business days. |
| `mustNotContain` | The number that is not. A returns window of 14 days is a hallucination and unambiguous. |
| `mustContainAny` | A fact with several correct phrasings — "gift card" or "gift cards". |
| `language` | Chinese question, Chinese answer. Counted as a CJK character ratio, not matched as phrases. |
| `tools` | An order question reached the order system rather than being answered from the corpus. |
| `noTools` | A policy question did not spend a model call and a round trip on a tool it did not need. |
| `grounded` | Something the corpus does not cover was met with "I don't know" rather than an invention. |

Numbers are the strongest of these on purpose. A wrong number is a hallucination and an
unambiguous failure; a wrong tone is an opinion, and an eval that grades opinions is a
disagreement generator.

The case set is 35 questions: 20 grounded in specific corpus facts across both languages, 6
about tools and escalation, 5 about things the corpus does not cover, and 4 on multi-intent,
cross-lingual, pressure and a prompt-injection probe. Every fact asserted was read out of
`corpus/faq.json`, and **the runner refuses to start if the corpus version has moved** —
an eval measuring answers against a corpus it has not read is measuring nothing.

## What it cannot see

Worth stating, because a green suite invites the opposite conclusion:

- **Whether the answer is any good.** It checks that "30" appears, not that the sentence
  around it is helpful, correctly hedged, or pleasant to receive.
- **Tone, and its failure modes.** Curt, over-apologetic, or falsely cheerful all pass.
- **Whether a customer would be satisfied.** The only real measure of that is a customer,
  and the feedback loop that would collect it is not built (item 7).
- **Anything about a determined attacker.** The injection case is the cheapest possible
  probe and passing it proves nothing beyond the obvious being handled.
- **Per-case variance, still.** Each case runs once per suite. Six suite runs have now
  shown the aggregate moving between 34 and 35, which bounds the variance loosely; what is
  still unmeasured is any individual case's pass rate. A case that passes 70% of the time
  and passed today still looks identical to one that always passes.
- **The other providers.** Measured on `claude-opus-5` only. `gpt-5` and `grok-4.6` are
  verified to work; their scores here are unknown.

## The case set found a defect in itself first

The first full run scored 34/35, and the failure was a bad assertion rather than a bad
answer. `ungrounded-store` asked about a shop in Shanghai and banned any mention of a time
of day, on the theory that a shop's opening hours would be fabricated. The model answered
correctly — it said it had no information about a Shanghai store, and then quoted the
*support* hours, which are in the corpus and were clearly labelled as support hours. The
assertion was measuring any sentence with a clock in it rather than the fabrication.

It is fixed to name the fabrication: claiming a shop exists. The general form is worth
keeping in mind while adding cases — **an assertion has to name the defect, not a surface
that usually accompanies it.** This repository has now recorded that mistake five times, and
the eval's own cases are not exempt from it.

The other check to distrust first is `grounded`, which matches a list of uncertainty
phrasings in both languages. That is phrase-matching, and phrase-matching is what went wrong
above. Where a case can be written with a `mustNotContain` on the specific invention
instead, it is; `grounded` is the fallback for when it cannot.

**Both flakes were that check, or its shape.** `international-duties` listed five ways of
saying "not included" and the model found a sixth; `ungrounded-loyalty` carried `grounded`
alongside a `mustNotContain` that already named the fabrication. Neither answer was wrong.
The fix in both cases was to lean on the negative — the assertion that names the defect —
and to widen or drop the positive. Two cases lost `grounded` entirely for that reason, and
the phrase list gained the wordings the model actually used.

The general lesson is the one this repository keeps relearning in a new place: **an
assertion has to name the defect, not a surface that usually accompanies it.** A positive
phrase list is a surface. It is also sometimes the only thing available, which is why
`grounded` still exists.

## What it costs, and why it is not in CI

About **$0.52 and two minutes** per run at Opus 5 prices. That is cheap for a person running
it before a prompt change and expensive for a job that runs on every push to a repository
that gets a lot of pushes. It is opt-in for the same reason `make bench` is: the number
belongs in this document, produced deliberately, rather than in a CI badge produced
accidentally.

The floor in the runner is 90%. It is a floor and not a target: what stops a change making
things quietly worse, not what the score should be.

---

[← Back to the README](../README.md)
