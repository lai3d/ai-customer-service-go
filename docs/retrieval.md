# Retrieval


The FAQ corpus lives in [`corpus/faq.json`](../corpus/faq.json) — 18 entries across
returns, shipping, payment, account and support, each written in English and Chinese.
Every language becomes its own document, so 36 in total. **It is sample data.** Replace
it before this answers anything real.

It is copied byte for byte from the Java implementation of this system, and that is
deliberate: the two repositories exist to be compared, and a reworded corpus would make
every retrieval number below incomparable.

Ingestion runs at startup and *replaces* what it wrote last time rather than appending.
Duplicates do not merely waste space: they crowd out distinct passages in the top-k
window, so the model sees one answer four times instead of four different ones.

No text splitter, deliberately. An FAQ entry is already the unit a customer's question
should match, and splitting one would separate a question from its answer. Long-form
policy documents would need one.

### In-process embedding in Go: yes, and it costs cgo

Anthropic has no embedding API, so a RAG path either runs a model locally or takes a
dependency on a second vendor. Whether that is viable in Go was the open question this
repository started with. It is:

| | |
| --- | --- |
| Session start (470 MB fp32 model) | 141 ms, once |
| One query embedded | **2 ms** |
| The whole 36-document corpus indexed | 166–194 ms |

`multilingual-e5-small` through [`onnxruntime_go`](https://github.com/yalue/onnxruntime_go),
tokenised by the Rust HuggingFace tokenizers through
[`daulet/tokenizers`](https://github.com/daulet/tokenizers). Both have prebuilt native
libraries for darwin and linux on arm64 and amd64, which `make deps` fetches.

What it costs is cgo, and the bill has three lines. The build needs two native artefacts
present before it will link. The binary is no longer statically linked or trivially
cross-compiled. And — measured, not anticipated — **a goroutine inside a cgo call blocks
its OS thread**, which under load makes the Go runtime create more of them; that is the
subject of [the benchmark](benchmark.md) and the reason `rag.Bounded` exists.

Against that: no second vendor, no second API key, nothing per query, and 2 ms.

**The scores agree with the Java implementation to four decimal places**, and that
agreement is the tokenizer test. `multilingual-e5-small` is XLM-RoBERTa, whose tokenizer
is a SentencePiece unigram model; getting it subtly wrong produces plausible vectors and
bad rankings rather than an error. Two independent tokenizer implementations — Java's DJL
and Rust's `tokenizers` — landing on 0.8896 for the same query against the same passage
is a stronger check than either side could run alone. A reader will not reconstruct that
reasoning from the number, so it is written here.

e5 requires asymmetric input markers — `query: ` before a search query, `passage: ` before
an indexed document. They are part of the model contract, not decoration, and applying
them to one side only is worse than applying neither. Here they are enforced by the type:
[`Embedder`](../internal/rag/embedder.go) has `EmbedQuery` and `EmbedPassages` and no
`Embed`, so there is no way to embed a query as a passage. The Java implementation wrapped
the model in a decorator that inferred which case it was in from which overload the vector
store happened to call.

### Retrieval quality

Measured against a real pgvector and the real model on every build, with no API key
([`internal/rag/retrieval_test.go`](../internal/rag/retrieval_test.go)). The queries are
the Java implementation's, verbatim, so the numbers can be put side by side. They avoid
the corpus wording in both languages: matching a question to its own text proves nothing
about a customer describing a problem in their own words.

- **20 of 20** paraphrased questions, ten English and ten Chinese, retrieve the correct
  entry first.
- A Chinese question matches a Chinese passage, every time.
- **4 of 4** cross-lingual: a Chinese question finds the right English passage when only
  English exists.

That last one cannot be observed on the full corpus — same-language matches score high
enough that all eighteen Chinese passages outrank every English one, so the English half
has to be isolated with a filter. It is what matters for an entry nobody has translated
yet.

### No similarity threshold is worth setting with this model

The Java implementation measured 20 relevant questions against 10 off-topic ones, found
the weakest real match 0.006 above the strongest off-topic one, called that margin too
thin to tune against, and kept `0.5` as "a floor for degenerate input". This
implementation reproduces its weakest relevant score exactly — same model, same corpus,
same query — and then re-samples the other two populations:

| | n | boundary | worst case |
| --- | --- | --- | --- |
| Relevant questions (en + zh) | 20 | weakest **0.8378** | *"my parcel showed up broken"* |
| Off-topic questions (en + zh) | 10 | strongest **0.8490** | *"你们招聘工程师吗"* |
| Degenerate input | 15 | strongest **0.8417** | *"。。。"* |

All three overlap. Two separate things were wrong with the original conclusion, and they
are worth separating:

**The off-topic margin was not thin, it was sign-unstable.** Ten *different* off-topic
questions — the same sample size, not a larger one — put the strongest one *above* the
weakest real match. A margin of 0.006 that flips to −0.0113 when you re-draw the sample
was never a margin.

**The floor was never a floor.** Degenerate input — punctuation, a stray keystroke —
scores 0.75 to 0.84. A threshold of 0.5 rejected nothing that was measured, while
implying a mechanism that did not exist.

So the default is `0`, with the numbers in the comment, and relevance judgement lives in
the system prompt: it tells the model that reference material is selected by similarity,
that some of it will be unrelated, and to say so rather than stretch an unrelated passage
to fit. Ranking is what the retriever is good at, and it is good at it — 20 of 20.

The knob stays, because a different embedding model may well have a usable distribution.
**Re-measure before setting it. Do not copy this 0 either.**

#### The sample size is a lesson, in both directions

The first four degenerate inputs tried here topped out at 0.8119, and a floor at 0.82
looked defensible on that evidence — it cleared every real match. The eleventh input,
three Chinese full stops, scores 0.8417.

Four samples is not a measurement. That cuts against the conclusion as easily as for it,
which is the whole point: `TestNoSimilarityThresholdIsUseful` asserts the *overlap*, so if
a future embedding model separates the populations again the test fails and says to
re-measure rather than quietly passing on a claim that has stopped being true.

### What is not measured here

There is no evaluation harness scoring answer quality against a golden set. Everything
above says which passage was found, not whether the answer built from it was good.

The Java implementation reports one further limit that has not been re-measured here: one
of fourteen long, multi-intent questions still misses the passage that answers it, and
fixing it would mean putting a third of the corpus into every prompt. The `top-k` of 8
comes from that measurement and is inherited rather than independently confirmed —
**labelled here as unverified on this side.**

Worth saying plainly: at eighteen entries, retrieval is barely earning its keep. A corpus
this size could sit in the system prompt. The design matters for a corpus that cannot,
and the measurements are what would carry over.

## Reloading the corpus used to degrade the index

Ingestion replaces the corpus rather than appending to it, and it runs on every process
start. That is the right shape — a restart must not double the corpus — and the way it
cleared the table was wrong in a way nothing here could see.

`DELETE FROM faq_document` leaves every previous generation of the corpus in the HNSW
index as dead entries. An HNSW scan is **approximate**: it collects `hnsw.ef_search`
candidates by walking the graph and only afterwards discards the ones whose heap tuples
are dead. After enough restarts the candidates are mostly dead, and
`ORDER BY embedding <=> $1 LIMIT 8` returns fewer than eight live rows — or none — with no
error, no log line, and a service that looks healthy.

Measured on pgvector 0.8.6 (`pgvector/pgvector:pg17`), autovacuum disabled on the table so
the daemon is not racing the measurement:

| after 60 reload cycles | rows returned for `LIMIT 8` |
| --- | --- |
| HNSW index scan | **2** |
| sequential scan (`enable_indexscan = off`) | 8 |
| HNSW index scan after `VACUUM` | 8 |

The table held 36 live rows throughout.

`TRUNCATE` inside the same transaction rebuilds the index empty. Readers block on its
`ACCESS EXCLUSIVE` lock until the new rows are committed, which for a 36-document load is
the right trade: a reader that waits a moment is better than a reader that silently
retrieves nothing.

With autovacuum on — the default — this is a race with a background daemon rather than a
certainty, which is what makes it dangerous. It appears in a deployment that restarts often
and never on a laptop.

**This repository did not find it.** The .NET implementation of this system did, in its own
code, and reported the shape across; it was reproduced here in `psql` before anything was
changed. The reason the existing tests could not have found it is worth keeping: they
ingest once per package, so the second reload never happened. `TestRetrievalSurvivesManyCorpusReloads`
does sixty, and sixty is measured rather than chosen — at thirty the test passed with
`DELETE`, which would have made it a test that could not tell the fix from the defect.

Thirty is enough on the .NET side, and the difference is the write, not the database: that
implementation reloads with one `INSERT` per row inside one transaction, where this one
uses a single `CopyFrom`. Thirty-six statements churn the index more per cycle than one
copy does, so the same defect needs about twice the cycles to show here. A cycle count
copied between implementations would have been the wrong number in one of them.

One trap in measuring it, which cost a wrong first answer here: `enable_seqscan = off` and
`enable_indexscan = off` are cost penalties, not prohibitions. With both off the planner
still chooses the HNSW index, so a row labelled "sequential scan" is the index scan again
and the comparison says nothing. Leave `enable_seqscan` on.

---

[← Back to the README](../README.md)
