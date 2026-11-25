package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

// The filler of a context_fill phase: plain sentences from a fixed word list,
// drawn by a seeded generator, so the same seed gives the same text on every
// build. A number of facts ("Record 03: the harbour code is 4821.") are
// planted at even distances through the text the phase expects to send, and
// the last turn asks them back.

// fillerWords is the word list. It is part of the format: changing it
// changes every filler (and its hash), so it only ever grows at the end.
var fillerWords = strings.Fields(`
the a an of and to in on at by for with from over under near after before
during between around through about against without within along across behind
river valley town road bridge field garden window door table chair lamp book
letter paper market harbour station tower wall gate path hill stone tree forest
lake island coast village street house room kitchen roof floor shelf box basket
morning evening night winter summer autumn spring week month year hour minute
day season weather rain snow wind cloud sun moon star light shadow sound voice
farmer baker teacher sailor driver painter doctor clerk miller weaver smith
guard keeper writer reader traveller neighbour child family friend stranger
crowd council school office shop workshop mill farm barn stable dock ship boat
cart wagon train engine wheel rope chain hammer nail board plank brick tile
glass cup plate bowl spoon knife bread cheese apple pear plum grain flour salt
water milk tea soup honey butter oil candle coal wood iron copper silver tin
cloth wool linen thread needle button coat hat boot glove scarf blanket pillow
map clock bell drum flute song story note list record ledger page line word
number count measure weight length width height distance price cost wage tax
walked carried opened closed built mended painted counted measured weighed
wrote read signed sent kept found lost moved lifted lowered turned stopped
started waited watched listened heard saw noticed remembered forgot planned
cleaned washed swept cooked baked boiled poured filled emptied gathered sorted
stacked folded packed loaded unloaded pulled pushed rolled dropped raised
crossed climbed followed passed reached left returned arrived stayed rested
quiet busy small large old new early late long short wide narrow high low
warm cold dry wet bright dark heavy light soft hard plain simple careful slow
quick steady patient tired calm clean rough smooth round square flat deep
every each some many few several most other another same next last first second
third fourth slowly quietly carefully often always never sometimes usually
again later soon then still also only just almost nearly together apart
north south east west upper lower inner outer front back middle left right
`)

// fillerFactRe finds a planted fact.
var fillerFactRe = regexp.MustCompile(`Record (\d+): the harbour code is (\d{4})\.`)

// filler makes the text of one context_fill phase.
type filler struct {
	seed   int64
	recall int
	// plantAt[k] is the character offset of the whole filler at which fact
	// k+1 goes (the first paragraph starting at or after it carries it).
	plantAt []int
	codes   []string
	rng     *rand.Rand
	paras   []string
	chars   int // characters of the paragraphs made so far, with their separators
	planted int // facts planted so far
}

// newFiller makes the filler of a seed, with recall facts planted through
// about targetTokens tokens of text (four characters a token is assumed for
// where they go; the count itself comes from the model's tokenizer).
func newFiller(seed int64, recall, targetTokens int) *filler {
	f := &filler{seed: seed, recall: recall, rng: rand.New(rand.NewPCG(uint64(seed), 0x9e3779b97f4a7c15))}
	codes := rand.New(rand.NewPCG(uint64(seed), 0x2545f4914f6cdd1d))
	total := targetTokens * 4
	for k := 0; k < recall; k++ {
		f.plantAt = append(f.plantAt, int((float64(k)+0.5)/float64(recall)*float64(total)))
		f.codes = append(f.codes, strconv.Itoa(1000+codes.IntN(9000)))
	}
	return f
}

// code is the harbour code of record k (1-based).
func (f *filler) code(k int) string { return f.codes[k-1] }

// paragraph is paragraph i (0-based) of the filler; paragraphs are made in order.
func (f *filler) paragraph(i int) string {
	for len(f.paras) <= i {
		f.paras = append(f.paras, f.next())
	}
	return f.paras[i]
}

func (f *filler) next() string {
	var b strings.Builder
	if f.planted < f.recall && f.chars >= f.plantAt[f.planted] {
		f.planted++
		fmt.Fprintf(&b, "Record %02d: the harbour code is %s. ", f.planted, f.codes[f.planted-1])
	}
	sentences := 2 + f.rng.IntN(2)
	for s := 0; s < sentences; s++ {
		n := 8 + f.rng.IntN(10)
		for w := 0; w < n; w++ {
			word := fillerWords[f.rng.IntN(len(fillerWords))]
			if w == 0 {
				word = strings.ToUpper(word[:1]) + word[1:]
			} else {
				b.WriteByte(' ')
			}
			b.WriteString(word)
		}
		b.WriteString(". ")
	}
	p := strings.TrimSpace(b.String())
	f.chars += len(p) + 2
	return p
}

// score is how many of the codes the answer gives right ("03: 4821", one
// per record, in any order and with any words around them).
func (f *filler) score(answer string) int {
	got := map[int]string{}
	for _, m := range regexp.MustCompile(`(\d{1,2})\D{1,40}?(\d{4})`).FindAllStringSubmatch(answer, -1) {
		k, _ := strconv.Atoi(m[1])
		if _, seen := got[k]; !seen {
			got[k] = m[2]
		}
	}
	n := 0
	for k := 1; k <= f.recall; k++ {
		if got[k] == f.codes[k-1] {
			n++
		}
	}
	return n
}

// digest is the sha256 (hex) of the given text joined with blank lines, as sent.
func fillerDigest(parts []string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte("\n\n"))
		}
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}
