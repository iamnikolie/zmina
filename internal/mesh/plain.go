package mesh

import (
	"reflect"
	"strings"
	"unicode"
)

// Plain is s as a line of a terminal may show what a peer wrote: every control character (the C0 and C1 ones, DEL, so an escape
// sequence is only text) and every format character that hides or reorders text (zero-width and bidirectional marks) is dropped, a
// line break or a tab becomes a space, and a byte that is no UTF-8 becomes "?". What a node tells is data, never a line the
// terminal of the owner's node should act on.
func Plain(s string) string {
	return clean(s, false)
}

// PlainText is Plain for a text of several lines (a diff of a crew file): line breaks and tabs stay, every other control
// character goes.
func PlainText(s string) string {
	return clean(s, true)
}

func clean(s string, lines bool) string {
	s = strings.ToValidUTF8(s, "?")

	var b strings.Builder

	b.Grow(len(s))

	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			if lines {
				b.WriteRune(r)
			} else {
				b.WriteByte(' ')
			}
		case r == '\r' && !lines:
			b.WriteByte(' ')
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
		default:
			b.WriteRune(r)
		}
	}

	return b.String()
}

// PlainAll makes every string of v (a pointer to a struct, a slice, a map) Plain: its fields, the elements of its slices, the keys and
// the values of its maps, and what its pointers lead to. Bytes (a raw JSON field, a file) are left as they are: the one that prints
// them makes them Plain where it prints.
func PlainAll(v any) {
	plainValue(reflect.ValueOf(v))
}

func plainValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			plainValue(v.Elem())
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(Plain(v.String()))
		}
	case reflect.Struct:
		for i := range v.NumField() {
			plainValue(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return
		}

		for i := range v.Len() {
			plainValue(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)

			// A map's entries are not addressable: a copy is made Plain and put back under a key made Plain too.
			c := reflect.New(e.Type()).Elem()
			c.Set(e)
			plainValue(c)

			nk := k

			if k.Kind() == reflect.String {
				nk = reflect.ValueOf(Plain(k.String())).Convert(k.Type())
				if nk.String() != k.String() {
					v.SetMapIndex(k, reflect.Value{})
				}
			}

			v.SetMapIndex(nk, c)
		}
	}
}
