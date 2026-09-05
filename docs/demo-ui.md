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

### It renders the model's markdown, in a deliberately small subset

The model writes markdown — bold spans and hyphen lists in almost every reply — and the
page showed the customer the asterisks and hyphens, verbatim, in the pane a reader looks
at first. Nothing was broken except the rendering, the text arrived correctly, and no test
noticed. It was found by driving the page in a real browser, which is the only reason to
do that at all.

Bold, unordered lists and inline code are rendered. **Links are not**, on purpose: every
other construct is formatting, and a link is a capability — a model-authored `href` is the
one piece of markdown that does something rather than looks like something.

The rendering builds DOM nodes. There is no `innerHTML` on the page, and
`TestTheDemoPageNeverTurnsAStringIntoMarkup` asserts that every sink that turns a string
into markup stays absent. Model text becomes a text node or it does not appear —
`**<script>…</script>**` renders as a bold span whose *text* is `<script>…</script>`. That
matters more here than in an ordinary chat client, because the model's input includes
retrieved passages: the injection path the system prompt can only ask about.

**The system prompt was not touched.** One sentence telling the model to answer in plain
text would have closed this, and it would also have made this implementation's prompt
differ from the Java one — and prompt parity is part of what makes the two comparable at
all. The gap was in a demo page, so the fix is in the demo page. (The Java implementation's
page has the same gap: its `bubble()` assigns `textContent` too.)

### Two model calls are two messages

A tool-calling turn makes two model calls, and if the model says something before asking
for the tool — *"I'll look that up for you."* — the second call's text is a new message
rather than a continuation of the first. Appended raw, the two run together:

```
...and any tracking details.Here's what I found for order ORD-10042:
```

That reads as a typo in the answer rather than as the seam it is. The stream now carries a
paragraph break at the boundary, in the streamed events and in what is persisted, so the
next turn does not re-send a run-together message as history.

It only appears when the model narrates before calling the tool, which is why the obvious
test question — one that goes straight to the tool — never surfaces it. Found by the Java
implementation's session in a screenshot it had been shipping, and reproduced here on a
live turn in both languages before being fixed.

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

### Verified in a browser, and what that did not cover

Driven in a headless Chromium at 1440×950 by the Java implementation's session, sampling
the DOM every 120 ms:

```
 123 ms   retrieval panel — 8 passages, scores, relative bars
1933 ms   tool pill — lookup_order_status → found
3629 ms   the first word of the answer
5065 ms   usage card — claude-opus-5, model calls 2, 3,816 in / 238 out
```

Retrieval is on screen three and a half seconds before the first word of the answer, and
the tool pill a second and a half before it. The same turn timed independently on the wire
lines up, so the page adds no reordering of its own.

Headless, with a throwaway profile. Font fallback and anything gated on a real display are
not covered by that.

### What it does not show

Retrieval carries no duration on the card. The honest per-stage timing is on the trace —
`embed query` and `pgvector similarity search` are separate spans — and a number invented
in the browser would be worse than a link to the real one.

---

[← Back to the README](../README.md)
