// Package benchmarks measures Strata against the targets in AGENTS.md section 39.
//
// Section 39 asks for targets that are "configurable and benchmarked", and requires every
// result to state dataset size, hardware, index configuration, embedding model, and query
// mix. That reporting requirement is the reason this is a package rather than a scattering
// of Benchmark functions: a number without its conditions is not a measurement, and the
// harness that produces the number is the only thing that reliably knows them.
//
// Three of section 39's items are not speeds at all — bounded graph traversal, no unbounded
// context generation, no full graph scan for ordinary semantic search. Those are invariants,
// so they are ordinary tests that run in CI (guards_test.go) rather than benchmarks nobody
// runs. A performance property that is only checked when someone remembers to check it is a
// property the system does not have.
package benchmarks

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// Corpus is a deterministic synthetic dataset.
//
// Deterministic because a benchmark whose input changes between runs measures the input.
// Synthetic because the shape that matters — how many entities recur across how many
// documents — has to be controlled, and no real corpus lets you state it exactly.
type Corpus struct {
	// Documents is how many source events the corpus produces.
	Documents int
	// EntitiesPerDocument is how many named entities each document mentions.
	EntitiesPerDocument int
	// DistinctEntities bounds the vocabulary, which is what makes entities recur across
	// documents and gives the graph something to traverse. A corpus where every mention
	// is unique has no graph in it.
	DistinctEntities int
	// WordsPerDocument sets the payload size, since ingest throughput depends on it.
	WordsPerDocument int
}

// DefaultCorpus is the standard dataset these benchmarks report against.
//
// Sized to finish in minutes on a laptop while still being large enough that an unindexed
// query is visibly slower than an indexed one — the failure mode the guards exist to catch.
// A deployment benchmarking its own hardware should scale it up and say so.
func DefaultCorpus() Corpus {
	return Corpus{
		Documents:           500,
		EntitiesPerDocument: 4,
		DistinctEntities:    120,
		WordsPerDocument:    140,
	}
}

// Describe renders the dataset parameters for a benchmark report, because section 39
// requires the size to be stated alongside the result.
func (c Corpus) Describe() string {
	return fmt.Sprintf("%d documents, ~%d words each, %d entity mentions each from a "+
		"vocabulary of %d", c.Documents, c.WordsPerDocument, c.EntitiesPerDocument,
		c.DistinctEntities)
}

// entityNames is the vocabulary documents draw from. Two-part names, so entity resolution
// has something to do beyond exact string equality.
var (
	givenNames = []string{
		"Priya", "Alice", "Marcus", "Yuki", "Tomas", "Fatima", "Elena", "Kwame",
		"Sofia", "Rahul", "Ingrid", "Chen", "Amara", "Lucas", "Noor",
	}
	familyNames = []string{
		"Raman", "Chen", "Okafor", "Tanaka", "Novak", "Haddad", "Rossi", "Mensah",
		"Larsen", "Iyer", "Bergstrom", "Wu", "Diallo", "Moreau", "Karim",
	}
	organizations = []string{
		"Kelvin Analytics", "Northbank Logistics", "Aurora Fabrication", "Tessellate",
		"Harbour Data", "Meridian Freight", "Cobalt Systems", "Lanternfish",
	}
	cities = []string{
		"Glasgow", "Edinburgh", "Bristol", "Porto", "Tallinn", "Kyoto", "Valencia",
	}
	predicatesInUse = []string{"WORKS_AT", "LOCATED_IN", "REPORTS_TO", "SUPPLIES"}
)

// Entity is one member of the corpus vocabulary.
type Entity struct {
	Name string
	Type string
}

// Vocabulary builds the recurring cast, deterministically.
func (c Corpus) Vocabulary() []Entity {
	out := make([]Entity, 0, c.DistinctEntities)
	for i := range c.DistinctEntities {
		switch i % 3 {
		case 0:
			out = append(out, Entity{
				Name: givenNames[i%len(givenNames)] + " " + familyNames[(i/3)%len(familyNames)],
				Type: "person",
			})
		case 1:
			out = append(out, Entity{
				Name: organizations[(i/3)%len(organizations)] + " " + suffix(i),
				Type: "organization",
			})
		default:
			out = append(out, Entity{Name: cities[(i/3)%len(cities)] + " " + suffix(i), Type: "place"})
		}
	}
	return out
}

// suffix keeps generated names distinct without making them unrecognizable.
func suffix(i int) string {
	return fmt.Sprintf("%c%d", 'A'+rune(i%26), i)
}

// Document is one generated source payload with the entities it mentions.
type Document struct {
	ExternalID string
	Content    string
	Mentions   []Entity
}

// Documents generates the corpus.
//
// Content is prose rather than lorem ipsum so the lexical projection has real stems to
// index and the query mix has real words to search for. It is repetitive, which is honest:
// this measures the system, not the writing.
func (c Corpus) Generate() []Document {
	vocabulary := c.Vocabulary()
	out := make([]Document, 0, c.Documents)

	for i := range c.Documents {
		mentions := make([]Entity, 0, c.EntitiesPerDocument)
		for j := range c.EntitiesPerDocument {
			// A stride that is coprime with most vocabulary sizes, so co-occurrence
			// spreads across the vocabulary instead of forming disjoint clusters.
			mentions = append(mentions, vocabulary[(i*7+j*13)%len(vocabulary)])
		}

		var body strings.Builder
		fmt.Fprintf(&body, "Report %d. ", i)
		for j, mention := range mentions {
			fmt.Fprintf(&body, "%s %s %s. ", mention.Name,
				strings.ToLower(strings.ReplaceAll(predicatesInUse[j%len(predicatesInUse)], "_", " ")),
				mentions[(j+1)%len(mentions)].Name)
		}
		body.WriteString(filler(i, c.WordsPerDocument-body.Len()/6))

		out = append(out, Document{
			ExternalID: fmt.Sprintf("doc-%05d", i),
			Content:    body.String(),
			Mentions:   mentions,
		})
	}
	return out
}

// filler pads a document to roughly the requested length with deterministic prose.
func filler(seed, words int) string {
	if words <= 0 {
		return ""
	}
	vocabulary := []string{
		"the", "quarterly", "review", "noted", "that", "delivery", "schedules",
		"remained", "stable", "across", "every", "region", "although", "capacity",
		"planning", "for", "the", "next", "period", "requires", "further", "work",
		"and", "the", "team", "agreed", "to", "revisit", "it", "in", "the", "spring",
	}
	hash := fnv.New32a()
	fmt.Fprintf(hash, "%d", seed)
	offset := int(hash.Sum32())

	var out strings.Builder
	for i := range words {
		out.WriteString(vocabulary[(offset+i)%len(vocabulary)])
		out.WriteByte(' ')
	}
	return out.String()
}

// QueryMix is the set of queries a retrieval benchmark runs.
//
// Section 39 requires the mix to be stated with any result, because retrieval latency
// depends entirely on what is asked: a rare term is a different measurement from a common
// one, and an identifier lookup is a different measurement from a semantic question.
type QueryMix struct {
	Name    string
	Queries []string
}

// StandardMix is a spread of query shapes rather than a repetition of one.
func StandardMix(vocabulary []Entity) []QueryMix {
	person, organization := "", ""
	for _, entity := range vocabulary {
		if person == "" && entity.Type == "person" {
			person = entity.Name
		}
		if organization == "" && entity.Type == "organization" {
			organization = entity.Name
		}
	}

	return []QueryMix{
		{Name: "entity-lookup", Queries: []string{person, organization}},
		{Name: "common-term", Queries: []string{
			"quarterly review delivery schedules",
			"capacity planning for the next period",
		}},
		{Name: "rare-term", Queries: []string{"Report 419", "Report 87"}},
		{Name: "relational", Queries: []string{
			person + " works at",
			"who reports to " + person,
		}},
	}
}
