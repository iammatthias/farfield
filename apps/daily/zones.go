package main

import "crypto/sha256"

// Zone is the day's color identity — a curated ink palette drawn fresh each
// day from the date alone, the way Terraforms' zones give every token its
// palette while biomes give the character set. The biome keeps the glyphs
// and the terrain's shape; the zone supplies every ink the day is printed
// in, across the SVG plate, the structure slice, and the three.js scenes.
type Zone struct {
	Name string
	Inks [4]string // elevation inks, low → high band, light → dark
	Wash string    // the day's paper tint — a pale ground under the inks
}

// zones are the sixteen ink palettes of the structure. All are survey-
// instrument inks — muted, legible on the warm farfield surface — but with
// genuine range: process blues, iron reds, umbers, coppers, indigos.
var zones = [16]Zone{
	{
		Name: "cyanotype", // process blues
		Inks: [4]string{"#8aa2b8", "#5d7f9d", "#39597a", "#1e3a57"},
		Wash: "#eef2f5",
	},
	{
		Name: "oxide", // iron reds
		Inks: [4]string{"#b58270", "#97604c", "#714033", "#48241a"},
		Wash: "#f5ece8",
	},
	{
		Name: "sepia", // umber browns
		Inks: [4]string{"#ab8e6b", "#8b6d4b", "#654b2e", "#3e2c17"},
		Wash: "#f4eee4",
	},
	{
		Name: "moss", // leaf greens
		Inks: [4]string{"#8ba874", "#678851", "#486538", "#2a4120"},
		Wash: "#edf2e7",
	},
	{
		Name: "slate", // cool gray-blues
		Inks: [4]string{"#94a0ab", "#6d7a86", "#4a5661", "#2c343d"},
		Wash: "#eef0f2",
	},
	{
		Name: "ochre", // earth golds
		Inks: [4]string{"#bd9a55", "#9c7a38", "#745822", "#493710"},
		Wash: "#f6efdd",
	},
	{
		Name: "graphite", // neutral pencil grays
		Inks: [4]string{"#9c9c9a", "#757572", "#4f4f4c", "#292927"},
		Wash: "#efefee",
	},
	{
		Name: "heath", // violet-grays
		Inks: [4]string{"#9f93a9", "#7a6d85", "#564a61", "#322a3a"},
		Wash: "#f1eff3",
	},
	{
		Name: "verdigris", // weathered copper greens
		Inks: [4]string{"#7fa899", "#5b8878", "#3d6555", "#234135"},
		Wash: "#e9f1ee",
	},
	{
		Name: "rust", // orange-browns on cream
		Inks: [4]string{"#c28a58", "#a26937", "#7a4b21", "#4d2d10"},
		Wash: "#f7eee2",
	},
	{
		Name: "midnight", // indigo blue-blacks
		Inks: [4]string{"#7c87a0", "#58647f", "#38435f", "#1d2640"},
		Wash: "#edeff3",
	},
	{
		Name: "kelp", // olive sea-greens
		Inks: [4]string{"#99986e", "#76754d", "#525230", "#313118"},
		Wash: "#f2f1e4",
	},
	{
		Name: "lichen", // pale gray-greens
		Inks: [4]string{"#9dab92", "#79876e", "#55624c", "#343f2e"},
		Wash: "#f0f2ec",
	},
	{
		Name: "clay", // fired pink-tans
		Inks: [4]string{"#b48d7e", "#946b5c", "#6d4a3d", "#452a20"},
		Wash: "#f5edea",
	},
	{
		Name: "tarn", // dark lake teals
		Inks: [4]string{"#7e9aa0", "#5a797f", "#3a585e", "#1f383d"},
		Wash: "#ecf1f2",
	},
	{
		Name: "bone", // warm bone grays
		Inks: [4]string{"#aaa394", "#847d6e", "#5d574a", "#38332a"},
		Wash: "#f3f1ec",
	},
}

// zoneIndexFor assigns a zone to one calendar day. The date hashes under its
// own "art-zone" domain prefix, so the day's palette is independent of its
// biome (a 4-D neighborhood property) and of the art seed's terrain stream —
// same date, same zone, everywhere it is drawn. len(zones) divides 256, so
// the first hash byte maps uniformly.
func zoneIndexFor(dateISO string) int {
	h := sha256.Sum256([]byte("art-zone:" + dateISO))
	return int(h[0]) % len(zones)
}
