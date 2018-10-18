// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build purego

package impl

import (
	"fmt"
	"reflect"
)

// offset represents the offset to a struct field, accessible from a pointer.
// The offset is the field index into a struct.
type offset []int

// offsetOf returns a field offset for the struct field.
func offsetOf(f reflect.StructField) offset {
	if len(f.Index) != 1 {
		panic("embedded structs are not supported")
	}
	return f.Index
}

// pointer is an abstract representation of a pointer to a struct or field.
type pointer struct{ v reflect.Value }

// pointerOfValue returns v as a pointer.
func pointerOfValue(v reflect.Value) pointer {
	return pointer{v: v}
}

// pointerOfIface returns the pointer portion of an interface.
func pointerOfIface(v *interface{}) pointer {
	return pointer{v: reflect.ValueOf(*v)}
}

// apply adds an offset to the pointer to derive a new pointer
// to a specified field. The current pointer must be pointing at a struct.
func (p pointer) apply(f offset) pointer {
	// TODO: Handle unexported fields in an API that hides XXX fields?
	return pointer{v: p.v.Elem().FieldByIndex(f).Addr()}
}

// asType treats p as a pointer to an object of type t and returns the value.
func (p pointer) asType(t reflect.Type) reflect.Value {
	if p.v.Type().Elem() != t {
		panic(fmt.Sprintf("invalid type: got %v, want %v", p.v.Type(), t))
	}
	return p.v
}
