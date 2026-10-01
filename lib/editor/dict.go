package editor

import _ "embed"

// Dictionary is the spelling word list, one word per line: SCOWL / ESDB
// size 60, US spelling, pre-expanded (no affix rules). See
// dict/LICENSE-SCOWL.txt. The editor loads it into a hash table in wasm and
// checks words as it draws them.
//
//go:embed dict/en_US.txt
var Dictionary []byte
