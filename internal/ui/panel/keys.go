package panel

import (
	"slices"
	"unicode"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// keyboard is the X server's table: for each keycode, its keysyms, per per keycode, the first one
// being first. A keycode only names a physical key; the table says what it writes in the current
// layout.
type keyboard struct {
	first xproto.Keycode
	per   int
	syms  []xproto.Keysym
}

// mapping reads the server's table. Without it, no key writes anything.
func mapping(conn *xgb.Conn) keyboard {
	setup := xproto.Setup(conn)
	reply, err := xproto.GetKeyboardMapping(conn, setup.MinKeycode, byte(setup.MaxKeycode-setup.MinKeycode+1)).Reply()
	if err != nil {
		return keyboard{}
	}
	return keyboard{first: setup.MinKeycode, per: int(reply.KeysymsPerKeycode), syms: reply.Keysyms}
}

// symbol says what a key writes under these modifiers, following the core protocol's rules: the
// level-3 column under AltGr when the key has one, the keypad under Num Lock, the upper case under
// Shift or, for a letter, under Caps Lock. An empty second column is the case of the first.
func (k keyboard) symbol(code xproto.Keycode, state uint16) xproto.Keysym {
	at := (int(code) - int(k.first)) * k.per
	if code < k.first || k.per == 0 || at+k.per > len(k.syms) {
		return 0
	}
	row := slices.Clone(k.syms[at : at+k.per])
	for len(row) < 6 {
		row = append(row, 0)
	}
	shifted := state&xproto.ModMaskShift != 0
	if state&xproto.ModMask2 != 0 && keypad(row[1]) {
		if shifted {
			return row[0]
		}
		return row[1]
	}
	lower, upper := row[0], row[1]
	if state&xproto.ModMask5 != 0 && row[4] != 0 {
		lower, upper = row[4], row[5]
	}
	if upper == 0 {
		upper = cased(lower, unicode.ToUpper)
		lower = cased(lower, unicode.ToLower)
	}
	if letter := unicode.IsLetter(runeOf(lower)); shifted != (letter && state&xproto.ModMaskLock != 0) {
		return upper
	}
	return lower
}

// The keysyms that are not characters and that text entry uses.
const (
	xkBackSpace = 0xff08
	xkReturn    = 0xff0d
	xkEscape    = 0xff1b
	xkHome      = 0xff50
	xkLeft      = 0xff51
	xkRight     = 0xff53
	xkEnd       = 0xff57
	xkKPEnter   = 0xff8d
	xkKPHome    = 0xff95
	xkKPLeft    = 0xff96
	xkKPRight   = 0xff98
	xkKPEnd     = 0xff9c
	xkKPDelete  = 0xff9f
	xkKPDecimal = 0xffae
	xkKP0       = 0xffb0
	xkKP9       = 0xffb9
	xkDelete    = 0xffff
)

// edited returns the draft after a key: a character is written at the cursor, the arrows, Home and End
// move it, Backspace and Delete erase on either side. The rest changes nothing — Enter and Escape
// concern whoever holds the draft, not the draft.
func (d draft) edited(sym xproto.Keysym) draft {
	text := slices.Clone(d.text)
	switch sym {
	case xkLeft, xkKPLeft:
		d.at = max(0, d.at-1)
	case xkRight, xkKPRight:
		d.at = min(len(text), d.at+1)
	case xkHome, xkKPHome:
		d.at = 0
	case xkEnd, xkKPEnd:
		d.at = len(text)
	case xkBackSpace:
		if d.at > 0 {
			text, d.at = slices.Delete(text, d.at-1, d.at), d.at-1
		}
	case xkDelete, xkKPDelete:
		if d.at < len(text) {
			text = slices.Delete(text, d.at, d.at+1)
		}
	default:
		if r := runeOf(sym); unicode.IsPrint(r) {
			text, d.at = slices.Insert(text, d.at, r), d.at+1
		}
	}
	d.text = text
	return d
}

// runeOf returns the character a keysym writes, zero if it writes none: Latin-1 is itself, a Unicode
// keysym carries its code point, and the keypad writes its digits.
func runeOf(sym xproto.Keysym) rune {
	switch {
	case sym >= 0x20 && sym <= 0x7e, sym >= 0xa0 && sym <= 0xff:
		return rune(sym)
	case sym >= 0x01000100 && sym <= 0x0110ffff:
		return rune(sym - 0x01000000)
	case sym >= xkKP0 && sym <= xkKP9:
		return '0' + rune(sym-xkKP0)
	case sym == xkKPDecimal:
		return '.'
	case sym == 0xffab:
		return '+'
	case sym == 0xffad:
		return '-'
	}
	return 0
}

// keypad says whether a keysym comes from the numeric keypad.
func keypad(sym xproto.Keysym) bool { return sym >= 0xff80 && sym <= 0xffbd }

// cased returns the keysym of the other case of a Latin-1 letter, the same one otherwise.
func cased(sym xproto.Keysym, to func(rune) rune) xproto.Keysym {
	if r := to(runeOf(sym)); sym <= 0xff && r <= 0xff {
		return xproto.Keysym(r)
	}
	return sym
}
