package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"strings"

	"github.com/iammatthias/farfield/lib/cid"
)

// The wordle artifact — one five-letter word a day, derived from the date.
// Unlike sudoku, the secret cannot be re-derived client-side from public
// data alone without shipping the answer order, so guess feedback is a
// server call; everything else mirrors the other daily artifacts: pure
// derivation, nothing stored, the answer never serialized until a game is
// over.

// wordleAnswersRaw is the embedded answers list: 2,300 common five-letter
// words, one per line, alphabetized.
//
// Provenance (generated 2026-06, committed for a deterministic build):
// take the five-letter all-lowercase a-z words of /usr/share/dict/web2 —
// macOS ships Webster's Second New International Dictionary (1934) word
// list, public domain — and rank them by the Google Books unigram counts in
// Peter Norvig's count_1w.txt (norvig.com/ngrams — word frequencies are
// uncopyrightable facts). Keep the 2,300 most frequent, excluding s-plurals
// of four-letter web2 lemmas and words whose frequency is inflated by being
// a common first name (/usr/share/dict/propernames). Answer quality is
// P1-refinable; regenerating with the same inputs reproduces this file.
// This list is NOT the NYT list and was not derived from it.
//
//go:embed wordle_answers.txt
var wordleAnswersRaw string

// wordleValidRaw is the embedded guess-validity dictionary: 14,194
// five-letter words, one per line, alphabetized — a superset of the answers.
//
// Provenance (generated 2026-06, committed for a deterministic build): the
// five-letter all-lowercase a-z words of /usr/share/dict/web2 (public
// domain, see above), unioned with regular inflections of its shorter
// lemmas (4-letter+s, 4-letter-in-e+d, 3-letter+ed, sibilant 3-letter+es,
// 3-letter-in-Cy→ies, 3-letter-in-e→ing — web2 is a lemma list and omits
// "birds", "moved", "asked", …), plus a small hand-screened supplement of
// common words web2 predates or omits (email, women, heard, began, boxes,
// pixel, …). Overgenerated near-words from the inflation rules are
// harmless here: this list only validates guesses, never picks answers.
//
//go:embed wordle_valid.txt
var wordleValidRaw string

// wordleAnswers is the answers list in play order: the embedded list
// shuffled once with the fixed seed("wordle-shuffle", "v1") stream, so
// consecutive days are not alphabetical neighbors. The shuffle is part of
// the artifact's identity — changing it changes every day's word.
var wordleAnswers = func() []string {
	words := strings.Fields(wordleAnswersRaw)
	rng := rand.NewChaCha8(seed("wordle-shuffle", "v1"))
	for i := len(words) - 1; i > 0; i-- {
		j := int(rng.Uint64() % uint64(i+1))
		words[i], words[j] = words[j], words[i]
	}
	return words
}()

// wordleValid is the guess-validity set.
var wordleValid = func() map[string]bool {
	words := strings.Fields(wordleValidRaw)
	set := make(map[string]bool, len(words))
	for _, w := range words {
		set[w] = true
	}
	return set
}()

// wordleGuesses is how many guesses a game allows.
const wordleGuesses = 6

// wordleDayValid reports whether a date has a word: well-formed, on or after
// the artifact epoch, not in the future.
func wordleDayValid(date string) bool {
	return validDate(date) && date >= artifactEpoch && date <= todayUTC()
}

// wordleAnswer returns the secret word for a date — the day index into the
// shuffled answers list, wrapping when the list runs out (~6.3 years).
// Callers must have validated the date; it must never be serialized into a
// response unless the game is over.
func wordleAnswer(date string) string {
	n, err := dayIndex(date)
	if err != nil || n < 0 {
		return ""
	}
	return wordleAnswers[int(n%int64(len(wordleAnswers)))]
}

// wordleCID is the public identity of one day's puzzle: a CID over
// "wordle:"+date+":"+sha256(answer) hex. Hashing the answer keeps the CID
// stable for a given day's word while revealing nothing recoverable about
// it — the public JSON carries only this.
func wordleCID(date string) string {
	sum := sha256.Sum256([]byte(wordleAnswer(date)))
	return cid.Of([]byte("wordle:" + date + ":" + hex.EncodeToString(sum[:])))
}

// wordleWordWellFormed reports whether w is five lowercase a-z letters.
func wordleWordWellFormed(w string) bool {
	if len(w) != 5 {
		return false
	}
	for i := range 5 {
		if w[i] < 'a' || w[i] > 'z' {
			return false
		}
	}
	return true
}

// wordleGuessAllowed reports whether a well-formed word may be played: in
// the validity dictionary, or the day's answer itself (the answers list is a
// subset of the dictionary by construction, but the answer must never be
// rejectable even if the lists drift).
func wordleGuessAllowed(guess, answer string) bool {
	return wordleValid[guess] || guess == answer
}

// wordleFeedback scores one guess against the answer: 'g' correct position,
// 'y' present elsewhere, '-' absent. Duplicate letters are handled the
// standard way — greens consume their letter first, then yellows consume
// what remains left-to-right, so a guess never shows more copies of a
// letter than the answer holds.
func wordleFeedback(answer, guess string) string {
	fb := [5]byte{'-', '-', '-', '-', '-'}
	var remaining [26]int
	for i := range 5 {
		if guess[i] == answer[i] {
			fb[i] = 'g'
		} else {
			remaining[answer[i]-'a']++
		}
	}
	for i := range 5 {
		if fb[i] == 'g' {
			continue
		}
		if c := guess[i] - 'a'; remaining[c] > 0 {
			fb[i] = 'y'
			remaining[c]--
		}
	}
	return string(fb[:])
}

// wordleHardViolation checks a guess against hard-mode constraints derived
// from the prior guesses: every revealed green must be reused in position,
// and every revealed yellow letter must appear (counting multiplicity of
// green+yellow per prior guess). It returns "" when the guess complies, or
// a short reason naming only letters the player has already seen revealed.
func wordleHardViolation(answer string, prior []string, guess string) string {
	for _, p := range prior {
		fb := wordleFeedback(answer, p)
		var need [26]int
		for i := range 5 {
			switch fb[i] {
			case 'g':
				if guess[i] != p[i] {
					return fmt.Sprintf("letter %d must be %c", i+1, p[i]-'a'+'A')
				}
				need[p[i]-'a']++
			case 'y':
				need[p[i]-'a']++
			}
		}
		var have [26]int
		for i := range 5 {
			have[guess[i]-'a']++
		}
		for c := range 26 {
			if have[c] < need[c] {
				return fmt.Sprintf("guess must contain %c", 'A'+byte(c))
			}
		}
	}
	return ""
}
