// Package rag turns the FAQ corpus into vectors, stores them, and finds the passages
// that answer a question.
package rag

import (
	"encoding/json"
	"fmt"
	"os"
)

// Source prefixes every document id, so a row in faq_document can be traced back to
// the corpus entry and language it came from by eye.
const Source = "faq"

type Corpus struct {
	Version string  `json:"version"`
	Notice  string  `json:"notice"`
	Entries []Entry `json:"entries"`
}

type Entry struct {
	ID        string      `json:"id"`
	Category  string      `json:"category"`
	Localized []Localized `json:"localized"`
}

type Localized struct {
	Language string `json:"language"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

// Document is one indexable unit. There is deliberately no text splitter behind this:
// splitters exist to cut long prose into retrievable pieces, and an FAQ entry is
// already the unit a customer's question should match — splitting one would separate a
// question from its answer. A corpus of long-form policy documents would need one.
//
// Each language becomes its own document. A Chinese question then matches a Chinese
// passage, which is what makes bilingual retrieval work at all; the cost is that
// same-language matches dominate, so cross-lingual retrieval is invisible on the full
// corpus and has to be tested by filtering to the other language.
type Document struct {
	ID            string
	EntryID       string
	Language      string
	Category      string
	Question      string
	Answer        string
	Content       string
	CorpusVersion string
}

func LoadCorpus(path string) (Corpus, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Corpus{}, fmt.Errorf("read FAQ corpus %s: %w", path, err)
	}
	var c Corpus
	if err := json.Unmarshal(raw, &c); err != nil {
		return Corpus{}, fmt.Errorf("parse FAQ corpus %s: %w", path, err)
	}
	if len(c.Entries) == 0 {
		return Corpus{}, fmt.Errorf("FAQ corpus %s contains no entries", path)
	}
	return c, nil
}

// Documents flattens the corpus. Both the question and the answer are embedded:
// embedding the question alone matches incoming phrasing most closely but loses recall
// whenever a customer describes their situation in the answer's vocabulary.
func (c Corpus) Documents() []Document {
	docs := make([]Document, 0, len(c.Entries)*2)
	for _, entry := range c.Entries {
		for _, l := range entry.Localized {
			docs = append(docs, Document{
				ID:            fmt.Sprintf("%s:%s:%s", Source, entry.ID, l.Language),
				EntryID:       entry.ID,
				Language:      l.Language,
				Category:      entry.Category,
				Question:      l.Question,
				Answer:        l.Answer,
				Content:       fmt.Sprintf("Q: %s\nA: %s", l.Question, l.Answer),
				CorpusVersion: c.Version,
			})
		}
	}
	return docs
}
