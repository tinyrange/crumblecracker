package rfb

// LinuxKeycode maps the X11 keysyms carried by RFB to Linux input-event
// keycodes. Shift state is sent independently by VNC clients, so upper- and
// lower-case symbols map to the same physical key.
func LinuxKeycode(keysym uint32) (uint16, bool) {
	if keysym >= 'A' && keysym <= 'Z' {
		keysym += 'a' - 'A'
	}
	if code, ok := asciiLinuxKeycodes[keysym]; ok {
		return code, true
	}
	if keysym >= 0xffbe && keysym <= 0xffc7 {
		return uint16(59 + keysym - 0xffbe), true
	}
	switch keysym {
	case 0xffc8:
		return 87, true
	case 0xffc9:
		return 88, true
	case 0xff08:
		return 14, true
	case 0xff09:
		return 15, true
	case 0xff0d:
		return 28, true
	case 0xff1b:
		return 1, true
	case 0xffe1:
		return 42, true
	case 0xffe2:
		return 54, true
	case 0xffe3, 0xffe4:
		return 29, true
	case 0xffe9, 0xffea:
		return 56, true
	case 0xffe5:
		return 58, true
	case 0xffeb:
		return 125, true
	case 0xffec:
		return 126, true
	case 0xff50:
		return 102, true
	case 0xff52:
		return 103, true
	case 0xff55:
		return 104, true
	case 0xff51:
		return 105, true
	case 0xff53:
		return 106, true
	case 0xff57:
		return 107, true
	case 0xff54:
		return 108, true
	case 0xff56:
		return 109, true
	case 0xff63:
		return 110, true
	case 0xffff:
		return 111, true
	default:
		return 0, false
	}
}

var asciiLinuxKeycodes = map[uint32]uint16{
	'1': 2, '2': 3, '3': 4, '4': 5, '5': 6,
	'6': 7, '7': 8, '8': 9, '9': 10, '0': 11,
	'!': 2, '@': 3, '#': 4, '$': 5, '%': 6,
	'^': 7, '&': 8, '*': 9, '(': 10, ')': 11,
	'-': 12, '_': 12, '=': 13, '+': 13,
	'q': 16, 'w': 17, 'e': 18, 'r': 19, 't': 20,
	'y': 21, 'u': 22, 'i': 23, 'o': 24, 'p': 25,
	'[': 26, '{': 26, ']': 27, '}': 27,
	'a': 30, 's': 31, 'd': 32, 'f': 33, 'g': 34,
	'h': 35, 'j': 36, 'k': 37, 'l': 38,
	';': 39, ':': 39, '\'': 40, '"': 40, '`': 41, '~': 41,
	'\\': 43, '|': 43,
	'z': 44, 'x': 45, 'c': 46, 'v': 47, 'b': 48,
	'n': 49, 'm': 50, ',': 51, '<': 51, '.': 52, '>': 52,
	'/': 53, '?': 53, ' ': 57,
}
