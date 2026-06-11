package main

import "crypto/sha256"

// Biome flavors one region of the hyperstructure: the inks a plate is drawn
// in, the glyphs its elevations quantize to, and the noise profile that
// shapes its terrain. Palettes are muted survey-map inks on the warm farfield
// surface — instrument colors, not neon.
type Biome struct {
	Name    string
	Palette [3]string // elevation inks, low → high band
	Ramp    []rune    // elevation glyphs, low → high
	Terrain terrainParams
}

// biomes are the eight regions of the structure. Each ramp mixes a different
// vocabulary — density blocks, accreting dots, bars, braille, water marks,
// squares, circles — so neighboring regions read as different map hatchings.
var biomes = [8]Biome{
	{
		Name:    "basin",
		Palette: [3]string{"#5b6661", "#39443f", "#1f2a26"}, // gray-green inks
		Ramp:    []rune("·:░▒▓█"),
		Terrain: terrainParams{Octaves: 3, BaseFreq: 2, Persistence: 0.35},
	},
	{
		Name:    "dune",
		Palette: [3]string{"#a08660", "#7a5f3b", "#54401f"}, // ochres
		Ramp:    []rune("˙‥∴∷▒▓"),
		Terrain: terrainParams{Octaves: 3, BaseFreq: 3, Persistence: 0.5},
	},
	{
		Name:    "ridge",
		Palette: [3]string{"#6b6460", "#4a423d", "#2b2522"}, // basalt browns
		Ramp:    []rune("·▁▂▄▆█"),
		Terrain: terrainParams{Octaves: 4, BaseFreq: 3, Persistence: 0.65},
	},
	{
		Name:    "mire",
		Palette: [3]string{"#5e6b50", "#42503a", "#2a3724"}, // moss greens
		Ramp:    []rune("·⠂⠆⠖⠶⠿"),
		Terrain: terrainParams{Octaves: 4, BaseFreq: 4, Persistence: 0.45},
	},
	{
		Name:    "shoal",
		Palette: [3]string{"#52707d", "#39565f", "#243c44"}, // slate blues
		Ramp:    []rune("·~≈≋▒▓"),
		Terrain: terrainParams{Octaves: 3, BaseFreq: 2, Persistence: 0.55},
	},
	{
		Name:    "steppe",
		Palette: [3]string{"#857a55", "#635a38", "#423b1f"}, // dry-grass golds
		Ramp:    []rune("·∙▪▮▓█"),
		Terrain: terrainParams{Octaves: 3, BaseFreq: 2, Persistence: 0.4},
	},
	{
		Name:    "karst",
		Palette: [3]string{"#707a84", "#4e5862", "#323a44"}, // limestone blue-grays
		Ramp:    []rune("·○◍◉●█"),
		Terrain: terrainParams{Octaves: 4, BaseFreq: 4, Persistence: 0.6},
	},
	{
		Name:    "caldera",
		Palette: [3]string{"#8a5a4a", "#66392c", "#421f16"}, // oxide reds
		Ramp:    []rune("·∘≡▒▓█"),
		Terrain: terrainParams{Octaves: 4, BaseFreq: 3, Persistence: 0.7},
	},
}

// biomeIndexAt assigns a biome to a 4-D lattice cell. Coordinates are
// downsampled by 4 (>>2), so biomes form contiguous 4×4×4×4 regions — about
// two months of days share one — then hashed under an "art-biome" domain
// prefix so the assignment is independent of every other derived stream.
func biomeIndexAt(x, y, z, w int) int {
	h := sha256.Sum256([]byte{
		'a', 'r', 't', '-', 'b', 'i', 'o', 'm', 'e', ':',
		byte(x >> 2), byte(y >> 2), byte(z >> 2), byte(w >> 2),
	})
	return int(h[0] % uint8(len(biomes)))
}
