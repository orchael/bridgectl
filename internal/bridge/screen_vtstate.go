package bridge

import (
	"reflect"
	"unsafe"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/vt"
)

// vtState is emulator state that a faithful checkpoint must reproduce but
// that charmbracelet/x/vt does not export: the active pen and hyperlink, the
// saved cursor (DECSC), the scrolling margins, the pending-wrap flag and the
// character-set shift state.
type vtState struct {
	cursor  vt.Cursor
	saved   vt.Cursor
	scroll  uv.Rectangle
	phantom bool
	// charset is true when a non-ASCII character set is designated or
	// shifted in for GL. Checkpoints decline that rare state.
	charset bool
}

var (
	vtScreenPtrType = reflect.TypeFor[*vt.Screen]()
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
	sv := scr.Elem()
	cur, ok1 := vtField(sv, "cur", vtCursorType)
	saved, ok2 := vtField(sv, "saved", vtCursorType)
	scroll, ok3 := vtField(sv, "scroll", vtRectType)
	phantom, ok4 := vtField(ev, "atPhantom", vtBoolType)
	gl, ok5 := vtField(ev, "gl", vtIntType)
	gsingle, ok6 := vtField(ev, "gsingle", vtIntType)
	charsets, ok7 := vtField(ev, "charsets", vtCharsetsType)
	if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 || !ok6 || !ok7 {
		return st, false
	}
	st.cursor = cur.Interface().(vt.Cursor)
	st.saved = saved.Interface().(vt.Cursor)
	st.scroll = scroll.Interface().(uv.Rectangle)
	st.phantom = phantom.Bool()
	sets := charsets.Interface().([4]vt.CharSet)
	glIndex := int(gl.Int())
	st.charset = gsingle.Int() != 0 || glIndex < 0 || glIndex > 3 || sets[glIndex] != nil
	return st, true
}
