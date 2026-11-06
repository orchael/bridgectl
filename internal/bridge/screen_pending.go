package bridge

// maxPendingInput bounds the incomplete trailing input a checkpoint carries.
// Longer unterminated strings (for example a large OSC 52 clipboard write)
// make checkpoints decline until the sequence ends.
const maxPendingInput = 4096

type pendingState uint8

const (
	pendGround    pendingState = iota
	pendEsc                    // ESC seen
	pendEscInter               // ESC + intermediates (charset designation etc.)
	pendCSI                    // ESC [ ... before the final byte
	pendOSC                    // ESC ] ... before BEL or ST
	pendString                 // DCS/SOS/PM/APC before ST
	pendStringEsc              // ESC inside an OSC/DCS/SOS/PM/APC string
	pendUTF8                   // inside a multi-byte UTF-8 character
)

// pendingInput tracks the bytes of an escape sequence or UTF-8 character that
// a chunk boundary left incomplete. The emulator keeps that prefix in its
// parser; a viewer that receives a checkpoint instead of the earlier chunk
// needs the same prefix so the next chunk completes it.
type pendingInput struct {
	state    pendingState
	utf8Left int
	buf      []byte
	overflow bool
}

func (p *pendingInput) start(c byte, state pendingState) {
	p.state = state
	p.buf = append(p.buf[:0], c)
	p.overflow = false
}

func (p *pendingInput) ground() {
	p.state = pendGround
	p.buf = p.buf[:0]
	p.overflow = false
}

// feed advances the tracker over one output chunk.
func (p *pendingInput) feed(data []byte) {
	for i := 0; i < len(data); i++ {
		c := data[i]
		if p.state != pendGround {
			if len(p.buf) < maxPendingInput {
				p.buf = append(p.buf, c)
			} else {
				p.overflow = true
			}
		}
		switch p.state {
		case pendGround:
			switch {
			case c == 0x1b:
				p.start(c, pendEsc)
			case c >= 0xc2 && c <= 0xf4:
				p.start(c, pendUTF8)
				p.utf8Left = 1
				if c >= 0xe0 {
					p.utf8Left = 2
				}
				if c >= 0xf0 {
					p.utf8Left = 3
				}
			}
		case pendUTF8:
			if c&0xc0 != 0x80 {
				// Invalid continuation: the character ends; reprocess c.
				p.ground()
				i--
				continue
			}
			p.utf8Left--
			if p.utf8Left == 0 {
				p.ground()
			}
		case pendEsc:
			switch {
			case c == '[':
				p.state = pendCSI
			case c == ']':
				p.state = pendOSC
			case c == 'P' || c == 'X' || c == '^' || c == '_':
				p.state = pendString
			case c == 0x1b:
				p.start(c, pendEsc)
			case c >= 0x20 && c <= 0x2f:
				p.state = pendEscInter
			case c == 0x18 || c == 0x1a || c >= 0x30:
				p.ground()
			}
		case pendEscInter:
			switch {
			case c == 0x1b:
				p.start(c, pendEsc)
			case c >= 0x30 || c == 0x18 || c == 0x1a:
				p.ground()
			}
		case pendCSI:
			switch {
			case c == 0x1b:
				p.start(c, pendEsc)
			case c >= 0x40 && c <= 0x7e, c == 0x18, c == 0x1a:
				p.ground()
			}
		case pendOSC:
			switch c {
			case 0x07, 0x18, 0x1a:
				p.ground()
			case 0x1b:
				p.state = pendStringEsc
			}
		case pendString:
			switch c {
			case 0x18, 0x1a:
				p.ground()
			case 0x1b:
				p.state = pendStringEsc
			}
		case pendStringEsc:
			if c == '\\' {
				p.ground()
				continue
			}
			// ESC followed by anything else ends the string and starts a new
			// escape sequence at that ESC.
			p.start(0x1b, pendEsc)
			i--
		}
	}
}

// prefix returns the incomplete trailing input, and false when it was too
// long to keep.
func (p *pendingInput) prefix() ([]byte, bool) {
	if p.state == pendGround {
		return nil, true
	}
	if p.overflow {
		return nil, false
	}
	return p.buf, true
}
