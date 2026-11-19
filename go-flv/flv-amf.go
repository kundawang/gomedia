package flv

import (
	"encoding/binary"
	"math"
)

// Minimal, allocation-free AMF0 support used only to locate and rewrite the
// "duration" field of an onMetaData script tag. The tag payload is patched in
// place: an AMF0 number is always an IEEE-754 double, so rewriting its value
// never changes the tag size.
//
// AMF0 markers used here, per the legacy RTMP spec:
const (
	amf0Number      = 0x00
	amf0Boolean     = 0x01
	amf0String      = 0x02
	amf0Object      = 0x03
	amf0MovieClip   = 0x04 // reserved, unsupported
	amf0Null        = 0x05
	amf0Undefined   = 0x06
	amf0Reference   = 0x07
	amf0EcmaArray   = 0x08
	amf0ObjectEnd   = 0x09
	amf0StrictArray = 0x0A
	amf0Date        = 0x0B
	amf0LongString  = 0x0C
	amf0XmlDoc      = 0x0F
	amf0TypedObject = 0x10
	amf3Marker      = 0x11 // switches to AMF3; unsupported for patching
)

// isOnMetaData reports whether a script-tag payload starts with the AMF0
// string "onMetaData" (optionally preceded by an AMF3 switch marker).
func isOnMetaData(payload []byte) bool {
	p := payload
	if len(p) > 0 && p[0] == amf3Marker {
		p = p[1:]
	}
	name, ok := amf0ReadString(p)
	if !ok {
		return false
	}
	return name == "onMetaData" || name == "@setDataFrame"
}

// patchMetadataDuration rewrites the duration property of the first metadata
// object found in payload to durationSec. The returned slice is always payload
// itself; ok is false when no numeric duration field exists.
func patchMetadataDuration(payload []byte, durationSec float64) (out []byte, ok bool) {
	out = payload
	off := 0
	// Skip an optional AMF3 marker and the leading name string.
	if len(payload) > 0 && payload[0] == amf3Marker {
		off = 1
	}
	n, ok1 := amf0SkipValue(payload, off)
	if !ok1 {
		return out, false
	}
	// n is the offset of the next value's marker byte, which is exactly where
	// the value-oriented walker expects to start.
	ok = amf0PatchWalk(payload, n, "duration", durationSec)
	return out, ok
}

// amf0PatchWalk walks a value starting at the marker byte at markerPos; for
// containers it recurses into every property/element looking for the named
// numeric property.
func amf0PatchWalk(b []byte, markerPos int, key string, val float64) bool {
	off := markerPos
	for off < len(b) {
		marker := b[off]
		off++
		switch marker {
		case amf0Number:
			if off+8 > len(b) {
				return false
			}
			off += 8
		case amf0Boolean:
			if off+1 > len(b) {
				return false
			}
			off++
		case amf0String:
			_, n, ok := amf0ReadStringAt(b, off-1)
			if !ok {
				return false
			}
			off = n
		case amf0LongString:
			_, n, ok := amf0ReadLongStringAt(b, off)
			if !ok {
				return false
			}
			off = n
		case amf0Null, amf0Undefined:
		case amf0Reference:
			if off+2 > len(b) {
				return false
			}
			off += 2
		case amf0Date:
			if off+10 > len(b) {
				return false
			}
			off += 10
		case amf0Object:
			n, found := amf0PatchObject(b, off, key, val)
			if found {
				return true
			}
			if n < 0 {
				return false
			}
			off = n
		case amf0TypedObject:
			// Typed object: skip the class name string first.
			_, n, ok := amf0ReadStringAt(b, off)
			if !ok {
				return false
			}
			off = n
			nn, found := amf0PatchObject(b, off, key, val)
			if found {
				return true
			}
			if nn < 0 {
				return false
			}
			off = nn
		case amf0EcmaArray:
			// The marker byte was already consumed; the array count is the
			// next four bytes (and is advisory only).
			if off+4 > len(b) {
				return false
			}
			off += 4
			n, found := amf0PatchObject(b, off, key, val)
			if found {
				return true
			}
			if n < 0 {
				return false
			}
			off = n
		case amf0StrictArray:
			if off+4 > len(b) {
				return false
			}
			count := int(binary.BigEndian.Uint32(b[off:]))
			off += 4
			for i := 0; i < count; i++ {
				if amf0PatchWalk(b, off, key, val) {
					return true
				}
				n, ok2 := amf0SkipValue(b, off)
				if !ok2 {
					return false
				}
				off = n
			}
		case amf0XmlDoc:
			_, n, ok := amf0ReadLongStringAt(b, off)
			if !ok {
				return false
			}
			off = n
		case amf0ObjectEnd:
			return false
		default:
			// Unknown/unsupported marker (incl. AMF3 switch): stop walking.
			return false
		}
	}
	return false
}

// amf0PatchObject walks an object body (key/value pairs terminated by the
// 0x00 0x00 0x09 end marker) and patches the named numeric property in place.
// It returns the offset just past the end marker, or -1 on malformed data.
func amf0PatchObject(b []byte, off int, key string, val float64) (int, bool) {
	found := false
	for {
		if off+2 > len(b) {
			return -1, found
		}
		// Empty key (UI16 length 0) followed by object-end marker.
		if b[off] == 0x00 && b[off+1] == 0x00 {
			if off+3 > len(b) || b[off+2] != amf0ObjectEnd {
				return -1, found
			}
			return off + 3, found
		}
		keyLen := int(binary.BigEndian.Uint16(b[off:]))
		off += 2
		if off+keyLen > len(b) {
			return -1, false
		}
		propName := string(b[off : off+keyLen])
		off += keyLen
		if off >= len(b) {
			return -1, false
		}
		if propName == key && b[off] == amf0Number {
			if off+9 > len(b) {
				return -1, false
			}
			binary.BigEndian.PutUint64(b[off+1:off+9], math.Float64bits(val))
			// Continue walking the rest of the object so nested containers are
			// still validated, but remember the hit for the caller.
			found = true
		}
		// Recurse into nested containers so a duration buried in a sub-object
		// is still reachable.
		if b[off] == amf0Object || b[off] == amf0EcmaArray {
			if amf0PatchWalk(b, off, key, val) {
				found = true
			}
		}
		n, ok := amf0SkipValue(b, off)
		if !ok {
			return -1, false
		}
		off = n
	}
}

// amf0SkipValue returns the offset immediately after the AMF0 value at off.
func amf0SkipValue(b []byte, off int) (int, bool) {
	if off >= len(b) {
		return off, false
	}
	marker := b[off]
	off++
	switch marker {
	case amf0Number:
		return off + 8, off+8 <= len(b)
	case amf0Boolean:
		return off + 1, off+1 <= len(b)
	case amf0String:
		_, n, ok := amf0ReadStringAt(b, off-1)
		return n, ok
	case amf0LongString, amf0XmlDoc:
		_, n, ok := amf0ReadLongStringAt(b, off)
		return n, ok
	case amf0Null, amf0Undefined:
		return off, true
	case amf0Reference:
		return off + 2, off+2 <= len(b)
	case amf0Date:
		return off + 10, off+10 <= len(b)
	case amf0Object:
		return amf0SkipObject(b, off)
	case amf0TypedObject:
		_, n, ok := amf0ReadStringAt(b, off-1)
		if !ok {
			return off, false
		}
		return amf0SkipObject(b, n)
	case amf0EcmaArray:
		if off+4 > len(b) {
			return off, false
		}
		return amf0SkipObject(b, off+4)
	case amf0StrictArray:
		if off+4 > len(b) {
			return off, false
		}
		count := int(binary.BigEndian.Uint32(b[off:]))
		off += 4
		for i := 0; i < count; i++ {
			n, ok := amf0SkipValue(b, off)
			if !ok {
				return off, false
			}
			off = n
		}
		return off, true
	default:
		return off, false
	}
}

func amf0SkipObject(b []byte, off int) (int, bool) {
	for {
		if off+2 > len(b) {
			return off, false
		}
		if b[off] == 0x00 && b[off+1] == 0x00 {
			if off+3 > len(b) || b[off+2] != amf0ObjectEnd {
				return off, false
			}
			return off + 3, true
		}
		keyLen := int(binary.BigEndian.Uint16(b[off:]))
		off += 2
		if off+keyLen > len(b) {
			return off, false
		}
		off += keyLen
		n, ok := amf0SkipValue(b, off)
		if !ok {
			return off, false
		}
		off = n
	}
}

// amf0ReadStringAt expects markerPos to point at an AMF0 string marker.
func amf0ReadStringAt(b []byte, markerPos int) (string, int, bool) {
	if markerPos >= len(b) || b[markerPos] != amf0String {
		return "", markerPos, false
	}
	off := markerPos + 1
	if off+2 > len(b) {
		return "", off, false
	}
	n := int(binary.BigEndian.Uint16(b[off:]))
	off += 2
	if off+n > len(b) {
		return "", off, false
	}
	return string(b[off : off+n]), off + n, true
}

func amf0ReadLongStringAt(b []byte, off int) (string, int, bool) {
	if off+4 > len(b) {
		return "", off, false
	}
	n := int(binary.BigEndian.Uint32(b[off:]))
	off += 4
	if off+n > len(b) {
		return "", off, false
	}
	return string(b[off : off+n]), off + n, true
}

func amf0ReadString(b []byte) (string, bool) {
	s, _, ok := amf0ReadStringAt(b, 0)
	return s, ok
}
