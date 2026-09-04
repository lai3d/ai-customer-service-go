# The demo UI


`docker compose up` serves a single page at **http://localhost:8081** — one HTML file,
embedded in the binary, no build step and no `npm install`.

It is deliberately not a chat widget. A widget's job is to make the model feel seamless
and invisible; this repository's substance *is* the invisible part. So the page is a
glass box: the conversation on the left, and on the right, for every turn, the passages
retrieval found with their scores, the tools the model called and what they decided, how
many model calls the turn billed for, and a link to that turn's trace in Jaeger.

That is only possible because the stream carries typed events rather than bare tokens:

```
event: retrieval    passages, with entry ids, languages and scores
event: tool         name and outcome
event: message      a chunk of the answer
event: usage        model, model calls, tokens, cost, wall time, trace id
event: error        a failure after the response was already committed
```

A production widget would read `message` and `error` and ignore the rest.

### Three things the page is careful about

**Score bars are normalised within each result set**, not drawn from the raw score. e5
compresses cosine similarity into a narrow high band, so absolute widths make every bar
look full and convey nothing. Relative widths show ranking, which is the part that is
reliable — the same finding that moved relevance filtering out of the threshold and into
the system prompt.

**Retrieval appears before the first token**, because that is when it is emitted. It also
survives a failed model call, which is exactly when someone debugging a bad answer most
needs to see what was retrieved.

**The usage card says "model calls: 2" and means it.** A tool-calling turn bills for at
least two, and a UI that showed one number for "the model call" would hide half of what
the turn cost. When the count is above one the card says so in a sentence, because the
number alone reads like a bug.

### What it does not show

Retrieval carries no duration on the card. The honest per-stage timing is on the trace —
`embed query` and `pgvector similarity search` are separate spans — and a number invented
in the browser would be worse than a link to the real one.

---

[← Back to the README](../README.md)
