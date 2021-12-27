package hl7

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"
)

type decoder struct {
	sep      byte    // usually a |
	repeat   byte    // usually a ~
	dividers [3]byte // usually |, ^, &
	chars    [4]byte // usually ^!\&
	escape   byte    // usually a \
	readSep  bool

	unescaper *strings.Replacer
}

func (d *decoder) setupUnescaper() {
	d.unescaper = strings.NewReplacer(
		string([]byte{d.escape, 'F', d.escape}), string(d.sep),
		string([]byte{d.escape, 'S', d.escape}), string(d.chars[0]),
		string([]byte{d.escape, 'R', d.escape}), string(d.chars[1]),
		string([]byte{d.escape, 'E', d.escape}), string(d.chars[2]),
		string([]byte{d.escape, 'T', d.escape}), string(d.chars[3]),
	)
}

var timeType reflect.Type = reflect.TypeOf(time.Time{})

func Unmarshal(data []byte, registry Registry) ([]any, error) {
	// Explicitly accept both CR and LF as new lines. Some systems do use \n, despite the spec.
	lines := bytes.FieldsFunc(data, func(r rune) bool {
		switch r {
		default:
			return false
		case '\r', '\n':
			return true
		}
	})

	type field struct {
		name  string
		index int
		tag   tag
		field reflect.Value
	}

	ret := []any{}

	d := &decoder{}

	for index, line := range lines {
		lineNumber := index + 1
		if len(line) == 0 {
			continue
		}

		segTypeName, n := d.getID(line)
		remain := line[n:]
		if len(segTypeName) == 0 {
			return nil, fmt.Errorf("line %d: missing segment type", lineNumber)
		}

		seg, ok := registry[segTypeName]
		if !ok {
			return nil, fmt.Errorf("line %d: unknown segment type %q", lineNumber, segTypeName)
		}

		rt := reflect.TypeOf(seg)
		ct := rt.NumField()

		fieldList := make([]field, 0, ct)

		hasInit := false

		rv := reflect.New(rt)
		rvv := rv.Elem()

		var SegmentName string
		var SegmentSize int32
		var maxOrd int32

		for i := 0; i < ct; i++ {
			ft := rt.Field(i)
			tag, err := parseTag(ft.Name, ft.Tag.Get(tagName))
			if err != nil {
				return nil, err
			}
			if !tag.Present {
				continue
			}
			if tag.Meta {
				SegmentName = tag.Name
				SegmentSize = tag.Order
				if ft.Type.Kind() == reflect.String {
					rvv.Field(i).SetString(tag.Name)
				}
				continue
			}
			if tag.Order > maxOrd {
				maxOrd = tag.Order
			}
			if tag.FieldSep || tag.FieldChars {
				hasInit = true
			}
			f := field{
				name:  ft.Name,
				index: i,
				tag:   tag,
			}
			f.field = rvv.Field(i)

			if !f.field.IsValid() {
				return nil, fmt.Errorf("%s.%s invalid reflect value", SegmentName, f.name)
			}

			fieldList = append(fieldList, f)
		}
		if SegmentSize == 0 {
			SegmentSize = maxOrd
		}

		offset := 0
		if hasInit {
			if len(remain) < 5 {
				return nil, fmt.Errorf("missing format delims")
			}
			d.sep = remain[0]
			copy(d.chars[:], remain[1:5])

			d.dividers = [3]byte{d.sep, d.chars[0], d.chars[3]}
			d.repeat = d.chars[1]
			d.escape = d.chars[2]
			d.setupUnescaper()
			d.readSep = true

			remain = remain[5:]
			offset = 2
		}

		if d.sep == 0 {
			return nil, fmt.Errorf("missing sep prior to field")
		}

		parts := bytes.Split(remain, []byte{d.sep})

		ff := make([]field, SegmentSize)
		for _, f := range fieldList {
			if f.tag.FieldSep {
				f.field.SetString(string(d.sep))
				continue
			}
			if f.tag.FieldChars {
				f.field.SetString(string(d.chars[:]))
				continue
			}
			index := int(f.tag.Order) - offset
			if index < 0 || index >= int(SegmentSize) {
				continue
			}
			ff[index] = f
		}

		for i, f := range ff {
			if i >= len(parts) {
				break
			}
			p := parts[i]
			if !f.tag.Present {
				continue
			}
			if f.tag.Omit {
				continue
			}
			if f.tag.Child {
				continue
			}
			err := d.decodeSegmentList(p, f.tag, f.field)
			if err != nil {
				return ret, fmt.Errorf("line %d, %s.%s: %w", lineNumber, SegmentName, f.name, err)
			}
		}

		ret = append(ret, rv.Interface())
	}

	return ret, nil
}

func (d *decoder) decodeSegmentList(data []byte, t tag, rv reflect.Value) error {
	if len(data) == 0 {
		return nil
	}
	parts := bytes.Split(data, []byte{d.repeat})
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		err := d.decodeSegment(p, t, rv, 1, len(parts) > 1)
		if err != nil {
			return fmt.Errorf("%s.%d: %w", rv.Type().String(), t.Order, err)
		}
	}
	return nil
}
func (d *decoder) decodeSegment(data []byte, t tag, rv reflect.Value, level int, mustBeSlice bool) error {
	type field struct {
		tag   tag
		field reflect.Value
	}

	isSlice := rv.Kind() == reflect.Slice
	if mustBeSlice && !isSlice {
		return fmt.Errorf("data repeats but element %v does not", rv.Type())
	}

	switch rv.Kind() {
	default:
		return fmt.Errorf("unknown field kind %v value=%v(%v) tag=%v data=%q", rv.Kind(), rv, rv.Type(), t, data)
	case reflect.Interface:
		// TODO: Support a true VARIES.
		return fmt.Errorf("unsupported interface field kind, data=%q", data)
	case reflect.Pointer:
		next := reflect.New(rv.Type().Elem())
		rv.Set(next)
		return d.decodeSegment(data, t, next.Elem(), level, false)
	case reflect.Slice:
		if len(data) == 0 {
			return nil
		}
		itemType := rv.Type().Elem()
		itemValue := reflect.New(itemType)
		ivv := itemValue.Elem()
		err := d.decodeSegment(data, t, ivv, level, false)
		if err != nil {
			return fmt.Errorf("slice: %w", err)
		}

		rv.Set(reflect.Append(rv, ivv))
		return nil
	case reflect.Struct:
		switch rv.Type() {
		default:
			sep := d.dividers[level]

			rt := rv.Type()
			ct := rv.NumField()

			fieldList := []field{}

			var SegmentName string
			var SegmentSize int32
			var maxOrd int32

			for i := 0; i < ct; i++ {
				ft := rt.Field(i)
				fTag, err := parseTag(ft.Name, ft.Tag.Get(tagName))
				if err != nil {
					return err
				}

				if fTag.Meta {
					SegmentName = fTag.Name
					SegmentSize = fTag.Order
					if ft.Type.Kind() == reflect.String {
						rv.Field(i).SetString(SegmentName)
					}
					continue
				}
				if !fTag.Present {
					continue
				}
				if fTag.Omit {
					continue
				}
				if fTag.Child {
					continue
				}
				if fTag.Order > maxOrd {
					maxOrd = fTag.Order
				}

				fv := rv.Field(i)

				f := field{
					tag:   fTag,
					field: fv,
				}
				fieldList = append(fieldList, f)
			}
			if SegmentSize == 0 {
				SegmentSize = maxOrd
			}
			ff := make([]field, int(SegmentSize))

			for _, f := range fieldList {
				index := int(f.tag.Order - 1)
				if index < 0 || index >= len(ff) {
					continue
				}

				ff[index] = f
			}

			// TODO: Make more robust. Watch for repeats, etc, other stuff.
			parts := bytes.Split(data, []byte{sep})
			for i, p := range parts {
				if i >= len(ff) {
					continue
				}
				f := ff[i]
				err := d.decodeSegment(p, f.tag, f.field, level+1, false)
				if err != nil {
					return fmt.Errorf("%s-%s.%d: %w", SegmentName, f.field.Type().String(), f.tag.Order, err)
				}
			}
			return nil
		case timeType:
			v := d.decodeByte(data, t)
			t, err := parseDateTime(v)
			if err != nil {
				return err
			}
			rv.Set(reflect.ValueOf(t))
			return nil
		}
	case reflect.String:
		c1, c2, c3 := d.dividers[0], d.dividers[1], d.dividers[2]
		for _, b := range data {
			switch b {
			case c1, c2, c3:
				return fmt.Errorf("%s contains an escape character %s; data may be malformed, invalid type, or contain a bug", t.Name, data)
			}
		}
		rv.SetString(d.decodeByte(data, t))
		return nil
	}
}

func (d *decoder) decodeByte(v []byte, t tag) string {
	if t.NoEscape {
		return string(v)
	}
	return d.unescaper.Replace(string(v))
}
func (d *decoder) decodeString(v string, t tag) string {
	if t.NoEscape {
		return v
	}
	return d.unescaper.Replace(v)
}

func (d *decoder) getID(data []byte) (string, int) {
	if d.readSep {
		v, _, _ := bytes.Cut(data, []byte{d.sep})
		return string(v), len(v)
	}
	for i, r := range data {
		if unicode.IsLetter(rune(r)) || unicode.IsNumber(rune(r)) {
			continue
		}
		return string(data[:i]), i
	}
	return string(data), len(data)
}

func parseDateTime(dt string) (time.Time, error) {
	for _, r := range dt {
		if r < '0' || r > '9' {
			return time.Time{}, nil // Probably something like "Invalid date".
		}
	}
	switch len(dt) {
	default:
		return time.Parse("20060102150405", dt)
	case 0:
		return time.Time{}, nil // No date supplied, use zero value
	case 8:
		return time.Parse("20060102", dt)
	case 12:
		return time.Parse("200601021504", dt)
	case 14, 16:
		return time.Parse("20060102150405", dt[:14])
	}
}