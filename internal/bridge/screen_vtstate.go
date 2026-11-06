package bridge

import (
	"reflect"
	"strings"
	"unsafe"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// vtScreenState is per-buffer state of one vt screen (main or alternate).
type vtScreenState struct {
	screen *vt.Screen
	cursor vt.Cursor
	saved  vt.Cursor
	scroll uv.Rectangle
}

// vtState is emulator state that a faithful checkpoint must reproduce but
// that charmbracelet/x/vt does not export: per-buffer cursor, pen,
// hyperlink, saved cursor (DECSC) and scrolling margins for both the active
// and the main buffer, the pending-wrap flag, and the character sets.
type vtState struct {
	vtScreenState
	main    vtScreenState
	phantom bool
	// charsets are the G0-G3 designations (nil = ASCII); gl is the set
	// shifted into GL; gsingle is a pending single shift.
	charsets [4]vt.CharSet
	gl       int
	gsingle  int
}

var (
	vtScreenPtrType = reflect.TypeFor[*vt.Screen]()
	vtScreensType   = reflect.TypeFor[[2]vt.Screen]()
	vtCursorType    = reflect.TypeFor[vt.Cursor]()
	vtRectType      = reflect.TypeFor[uv.Rectangle]()
	vtBoolType      = reflect.TypeFor[bool]()
	vtIntType       = reflect.TypeFor[int]()
	vtCharsetsType  = reflect.TypeFor[[4]vt.CharSet]()
)

// vtField returns an unexported field of an addressable struct value, only if
// it exists with exactly the expected type. Reads happen under
// managedSession.mu, the lock that also serializes every emulator write.
func vtField(v reflect.Value, name string, typ reflect.Type) (reflect.Value, bool) {
	f := v.FieldByName(name)
	if !f.IsValid() || f.Type() != typ || !f.CanAddr() {
		return reflect.Value{}, false
	}
	return reflect.NewAt(typ, unsafe.Pointer(f.UnsafeAddr())).Elem(), true
}

func readVTScreen(scr *vt.Screen) (vtScreenState, bool) {
	st := vtScreenState{screen: scr}
	sv := reflect.ValueOf(scr).Elem()
	cur, ok1 := vtField(sv, "cur", vtCursorType)
	saved, ok2 := vtField(sv, "saved", vtCursorType)
	scroll, ok3 := vtField(sv, "scroll", vtRectType)
	if !ok1 || !ok2 || !ok3 {
		return st, false
	}
	st.cursor = cur.Interface().(vt.Cursor)
	st.saved = saved.Interface().(vt.Cursor)
	st.scroll = scroll.Interface().(uv.Rectangle)
	return st, true
}

// readVTState reads the unexported state through reflection. It is pinned to
// the vt version in go.mod; if a dependency update renames or retypes any of
// these fields, ok is false and checkpoints are declined (gap without data)
// rather than emitting a screen that continues incorrectly. A unit test
// fails in that case so the accessor is updated with the dependency.
func readVTState(e *vt.Emulator) (st vtState, ok bool) {
	if e == nil {
		return st, false
	}
	ev := reflect.ValueOf(e).Elem()
	scr, ok := vtField(ev, "scr", vtScreenPtrType)
	if !ok || scr.IsNil() {
		return st, false
	}
	scrs, ok1 := vtField(ev, "scrs", vtScreensType)
	phantom, ok2 := vtField(ev, "atPhantom", vtBoolType)
	gl, ok3 := vtField(ev, "gl", vtIntType)
	gsingle, ok4 := vtField(ev, "gsingle", vtIntType)
	charsets, ok5 := vtField(ev, "charsets", vtCharsetsType)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
		return st, false
	}
	active, okA := readVTScreen(scr.Interface().(*vt.Screen))
	main, okM := readVTScreen(scrs.Index(0).Addr().Interface().(*vt.Screen))
	if !okA || !okM {
		return st, false
	}
	st.vtScreenState, st.main = active, main
	st.phantom = phantom.Bool()
	st.gl, st.gsingle = int(gl.Int()), int(gsingle.Int())
	st.charsets = charsets.Interface().([4]vt.CharSet)
	return st, true
}

// charsetSequence returns the designations and locking shift that reproduce
// the character-set state on a viewer reset to ASCII in G0-G3 with G0 in GL.
// ok is false for state it cannot express: an unknown set, or a pending
// single shift.
func (st *vtState) charsetSequence() (string, bool) {
	if st.gsingle != 0 || st.gl < 0 || st.gl > 3 {
		return "", false
	}
	var b strings.Builder
	for i, set := range st.charsets {
		if set == nil {
			continue
		}
		var final byte
		switch reflect.ValueOf(set).Pointer() {
		case reflect.ValueOf(vt.SpecialDrawing).Pointer():
			final = '0'
		case reflect.ValueOf(vt.UK).Pointer():
			final = 'A'
		default:
			return "", false
		}
		b.WriteByte(0x1b)
		b.WriteByte("()*+"[i])
		b.WriteByte(final)
	}
	b.WriteString([]string{"", "\x0e", "\x1bn", "\x1bo"}[st.gl])
	return b.String(), true
}
