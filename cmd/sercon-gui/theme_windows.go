//go:build windows

// Theme: the colour palette, the fonts, and the small drawing helpers the
// custom-painted parts of the window are built from.
//
// Everything visual lives here rather than at the call sites so that a colour
// is changed in one place and the window stays consistent. The two palettes
// are not a light/dark toggle — the sidebar is a dark rail beside a light
// content area, and both are on screen at once. Each has its own set of names.
package main

// Palette. COLORREF is 0x00BBGGRR, which is why these do not read like hex
// colour codes.
//
// Written slightly out of order so that the RGB value in the comment is what
// the literal spells.
const (
	// The window body: white, with text in near-black.
	colBackground = 0x00FFFFFF // #FFFFFF
	colText       = 0x00332D2B // #2B2D33
	colTextDim    = 0x00A3A19A // #9AA1A3
	colHint       = 0x00B4B2A9 // #A9B2B4
	colRule       = 0x00EBEAE6 // #E6EAEB

	// The sidebar: a neutral dark grey rail. Not near-black, so a screen left
	// open all day does not read as a black bar.
	colRailBG      = 0x00332D2B // #2B2D33
	colRailText    = 0x00E3E1DA // #DAE1E3
	colRailDim     = 0x00767470 // #707476
	colRailSel     = 0x003A3C36 // #363C3A
	colRailRule    = 0x003B3D33 // #333D3B
	colRailHover   = 0x00424440 // #404442
	colRailSelBar  = 0x00DD777F // #7F77DD
	colRailSelText = 0x00EAEFF0 // #F0EFEA

	// Status colours are the only saturated colour in the content area, because
	// the question they answer is the one an operator actually asks.
	colOnline    = 0x00119E63 // #639E11
	colOffline   = 0x002D2DA3 // #A32D2D
	colBusy      = 0x00BA7517 // #1775BA
	colOnlineBG  = 0x00E1F5EE // #E1F5EE
	colOfflineBG = 0x00EBEBFC // #FCEBEB
	colBusyBG    = 0x00DAEEDF // #FAEEDA

	colAccent = 0x00DD777F // #7F77DD

	// Row selection in the table: a very light tint, because the state capsules
	// already carry colour and a saturated selection would fight them.
	colRowSel = 0x00F5F0E9 // #E9F0F5
)

// Font roles. Segoe UI for prose, Consolas for anything whose digits must line
// up in a column: the port table and the counters. Proportional digits change
// width as the value changes, so a column of them visibly jitters every tick.
const (
	faceUI   = "Segoe UI"
	faceMono = "Consolas"
)

// fontRoles holds the fonts by role so a control asks for "the caption font"
// rather than for a point size.
type fontRoles struct {
	rail         uintptr // sidebar items
	railCaption  uintptr // sidebar section caption
	railFooter   uintptr // sidebar build info
	title        uintptr // window heading
	subtitle     uintptr // the summary line under the heading
	body         uintptr // table rows, general text
	bodyMedium   uintptr // emphasised table cell (the port name)
	colHead      uintptr // table column headers
	section      uintptr // "PORTS" label above the table
	counter      uintptr // the big statistics numbers
	counterLabel uintptr // the caption under each counter
	footer       uintptr // bottom bar
}

var fonts fontRoles

// bgBrush is the window background, created once and returned from
// WM_CTLCOLORSTATIC so the key panel's labels sit on the right surface.
var bgBrush uintptr

func initFonts() {
	bgBrush = uintptr(brush(colBackground))

	fonts.rail = createFont(faceUI, 9, fwNormal)
	fonts.railCaption = createFont(faceUI, 8, fwNormal)
	fonts.railFooter = createFont(faceMono, 8, fwNormal)

	fonts.title = createFont(faceUI, 12, fwSemibold)
	fonts.subtitle = createFont(faceUI, 9, fwNormal)
	fonts.body = createFont(faceUI, 9, fwNormal)
	fonts.bodyMedium = createFont(faceMono, 9, fwNormal)
	fonts.colHead = createFont(faceUI, 8, fwNormal)
	fonts.section = createFont(faceUI, 8, fwNormal)

	fonts.counter = createFont(faceMono, 15, fwSemibold)
	fonts.counterLabel = createFont(faceUI, 8, fwNormal)
	fonts.footer = createFont(faceUI, 8, fwNormal)
}
