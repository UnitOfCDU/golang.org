// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package impl

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"

	pref "github.com/golang/protobuf/v2/reflect/protoreflect"
	ptype "github.com/golang/protobuf/v2/reflect/prototype"
)

// MessageType provides protobuf related functionality for a given Go type
// that represents a message. A given instance of MessageType is tied to
// exactly one Go type, which must be a pointer to a struct type.
type MessageType struct {
	// Desc is an optionally provided message descriptor. If nil, the descriptor
	// is lazily derived from the Go type information of generated messages
	// for the v1 API.
	//
	// Once set, this field must never be mutated.
	Desc pref.MessageDescriptor

	once sync.Once // protects all unexported fields

	goType reflect.Type     // pointer to struct
	pbType pref.MessageType // only valid if goType does not implement proto.Message

	// TODO: Split fields into dense and sparse maps similar to the current
	// table-driven implementation in v1?
	fields map[pref.FieldNumber]*fieldInfo
}

// init lazily initializes the MessageType upon first use and
// also checks that the provided pointer p is of the correct Go type.
//
// It must be called at the start of every exported method.
func (mi *MessageType) init(p interface{}) {
	mi.once.Do(func() {
		v := reflect.ValueOf(p)
		t := v.Type()
		if t.Kind() != reflect.Ptr && t.Elem().Kind() != reflect.Struct {
			panic(fmt.Sprintf("got %v, want *struct kind", t))
		}
		mi.goType = t

		// Derive the message descriptor if unspecified.
		md := mi.Desc
		if md == nil {
			// TODO: derive the message type from the Go struct type
		}

		// Initialize the Go message type wrapper if the Go type does not
		// implement the proto.Message interface.
		//
		// Otherwise, we assume that the Go type manually implements the
		// interface and is internally consistent such that:
		//	goType == reflect.New(goType.Elem()).Interface().(proto.Message).ProtoReflect().Type().GoType()
		//
		// Generated code ensures that this property holds.
		if _, ok := p.(pref.ProtoMessage); !ok {
			mi.pbType = ptype.NewGoMessage(&ptype.GoMessage{
				MessageDescriptor: md,
				New: func(pref.MessageType) pref.ProtoMessage {
					p := reflect.New(t.Elem()).Interface()
					return (*message)(mi.dataTypeOf(p))
				},
			})
		}

		mi.generateFieldFuncs(t.Elem(), md)
	})

	// TODO: Remove this check? This API is primarily used by generated code,
	// and should not violate this assumption. Leave this check in for now to
	// provide some sanity checks during development. This can be removed if
	// it proves to be detrimental to performance.
	if reflect.TypeOf(p) != mi.goType {
		panic(fmt.Sprintf("type mismatch: got %T, want %v", p, mi.goType))
	}
}

// generateFieldFuncs generates per-field functions for all common operations
// to be performed on each field. It takes in a reflect.Type representing the
// Go struct, and a protoreflect.MessageDescriptor to match with the fields
// in the struct.
//
// This code assumes that the struct is well-formed and panics if there are
// any discrepancies.
func (mi *MessageType) generateFieldFuncs(t reflect.Type, md pref.MessageDescriptor) {
	// Generate a mapping of field numbers and names to Go struct field or type.
	fields := map[pref.FieldNumber]reflect.StructField{}
	oneofs := map[pref.Name]reflect.StructField{}
	oneofFields := map[pref.FieldNumber]reflect.Type{}
	special := map[string]reflect.StructField{}
fieldLoop:
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		for _, s := range strings.Split(f.Tag.Get("protobuf"), ",") {
			if len(s) > 0 && strings.Trim(s, "0123456789") == "" {
				n, _ := strconv.ParseUint(s, 10, 64)
				fields[pref.FieldNumber(n)] = f
				continue fieldLoop
			}
		}
		if s := f.Tag.Get("protobuf_oneof"); len(s) > 0 {
			oneofs[pref.Name(s)] = f
			continue fieldLoop
		}
		switch f.Name {
		case "XXX_weak", "XXX_unrecognized", "XXX_sizecache", "XXX_extensions", "XXX_InternalExtensions":
			special[f.Name] = f
			continue fieldLoop
		}
	}
	if fn, ok := reflect.PtrTo(t).MethodByName("XXX_OneofFuncs"); ok {
		vs := fn.Func.Call([]reflect.Value{reflect.New(fn.Type.In(0)).Elem()})[3]
	oneofLoop:
		for _, v := range vs.Interface().([]interface{}) {
			tf := reflect.TypeOf(v).Elem()
			f := tf.Field(0)
			for _, s := range strings.Split(f.Tag.Get("protobuf"), ",") {
				if len(s) > 0 && strings.Trim(s, "0123456789") == "" {
					n, _ := strconv.ParseUint(s, 10, 64)
					oneofFields[pref.FieldNumber(n)] = tf
					continue oneofLoop
				}
			}
		}
	}

	mi.fields = map[pref.FieldNumber]*fieldInfo{}
	for i := 0; i < md.Fields().Len(); i++ {
		fd := md.Fields().Get(i)
		fs := fields[fd.Number()]
		var fi fieldInfo
		switch {
		case fd.IsWeak():
			fi = fieldInfoForWeak(fd, special["XXX_weak"])
		case fd.OneofType() != nil:
			fi = fieldInfoForOneof(fd, oneofs[fd.OneofType().Name()], oneofFields[fd.Number()])
		case fd.IsMap():
			fi = fieldInfoForMap(fd, fs)
		case fd.Cardinality() == pref.Repeated:
			fi = fieldInfoForVector(fd, fs)
		case fd.Kind() == pref.MessageKind || fd.Kind() == pref.GroupKind:
			fi = fieldInfoForMessage(fd, fs)
		default:
			fi = fieldInfoForScalar(fd, fs)
		}
		mi.fields[fd.Number()] = &fi
	}
}

func (mi *MessageType) MessageOf(p interface{}) pref.Message {
	mi.init(p)
	if m, ok := p.(pref.ProtoMessage); ok {
		// We assume p properly implements protoreflect.Message.
		// See the comment in MessageType.init regarding pbType.
		return m.ProtoReflect()
	}
	return (*message)(mi.dataTypeOf(p))
}

func (mi *MessageType) KnownFieldsOf(p interface{}) pref.KnownFields {
	mi.init(p)
	return (*knownFields)(mi.dataTypeOf(p))
}

func (mi *MessageType) UnknownFieldsOf(p interface{}) pref.UnknownFields {
	mi.init(p)
	return (*unknownFields)(mi.dataTypeOf(p))
}

func (mi *MessageType) dataTypeOf(p interface{}) *messageDataType {
	return &messageDataType{pointerOfIface(&p), mi}
}

// messageDataType is a tuple of a pointer to the message data and
// a pointer to the message type.
//
// TODO: Unfortunately, we need to close over a pointer and MessageType,
// which incurs an an allocation. This pair is similar to a Go interface,
// which is essentially a tuple of the same thing. We can make this efficient
// with reflect.NamedOf (see https://golang.org/issues/16522).
//
// With that hypothetical API, we could dynamically create a new named type
// that has the same underlying type as MessageType.goType, and
// dynamically create methods that close over MessageType.
// Since the new type would have the same underlying type, we could directly
// convert between pointers of those types, giving us an efficient way to swap
// out the method set.
//
// Barring the ability to dynamically create named types, the workaround is
//	1. either to accept the cost of an allocation for this wrapper struct or
//	2. generate more types and methods, at the expense of binary size increase.
type messageDataType struct {
	p  pointer
	mi *MessageType
}

type message messageDataType

func (m *message) Type() pref.MessageType {
	return m.mi.pbType
}
func (m *message) KnownFields() pref.KnownFields {
	return (*knownFields)(m)
}
func (m *message) UnknownFields() pref.UnknownFields {
	return (*unknownFields)(m)
}
func (m *message) Unwrap() interface{} { // TODO: unexport?
	return m.p.asType(m.mi.goType.Elem()).Interface()
}
func (m *message) Interface() pref.ProtoMessage {
	return m
}
func (m *message) ProtoReflect() pref.Message {
	return m
}
func (m *message) ProtoMutable() {}

type knownFields messageDataType

func (fs *knownFields) Len() (cnt int) {
	for _, fi := range fs.mi.fields {
		if fi.has(fs.p) {
			cnt++
		}
	}
	// TODO: Handle extension fields.
	return cnt
}
func (fs *knownFields) Has(n pref.FieldNumber) bool {
	if fi := fs.mi.fields[n]; fi != nil {
		return fi.has(fs.p)
	}
	// TODO: Handle extension fields.
	return false
}
func (fs *knownFields) Get(n pref.FieldNumber) pref.Value {
	if fi := fs.mi.fields[n]; fi != nil {
		return fi.get(fs.p)
	}
	// TODO: Handle extension fields.
	return pref.Value{}
}
func (fs *knownFields) Set(n pref.FieldNumber, v pref.Value) {
	if fi := fs.mi.fields[n]; fi != nil {
		fi.set(fs.p, v)
		return
	}
	// TODO: Handle extension fields.
	panic(fmt.Sprintf("invalid field: %d", n))
}
func (fs *knownFields) Clear(n pref.FieldNumber) {
	if fi := fs.mi.fields[n]; fi != nil {
		fi.clear(fs.p)
		return
	}
	// TODO: Handle extension fields.
	panic(fmt.Sprintf("invalid field: %d", n))
}
func (fs *knownFields) Mutable(n pref.FieldNumber) pref.Mutable {
	if fi := fs.mi.fields[n]; fi != nil {
		return fi.mutable(fs.p)
	}
	// TODO: Handle extension fields.
	panic(fmt.Sprintf("invalid field: %d", n))
}
func (fs *knownFields) Range(f func(pref.FieldNumber, pref.Value) bool) {
	for n, fi := range fs.mi.fields {
		if fi.has(fs.p) {
			if !f(n, fi.get(fs.p)) {
				return
			}
		}
	}
	// TODO: Handle extension fields.
}
func (fs *knownFields) ExtensionTypes() pref.ExtensionFieldTypes {
	return (*extensionFieldTypes)(fs)
}

type extensionFieldTypes messageDataType // TODO

func (fs *extensionFieldTypes) Len() int                                     { return 0 }
func (fs *extensionFieldTypes) Register(pref.ExtensionType)                  { return }
func (fs *extensionFieldTypes) Remove(pref.ExtensionType)                    { return }
func (fs *extensionFieldTypes) ByNumber(pref.FieldNumber) pref.ExtensionType { return nil }
func (fs *extensionFieldTypes) ByName(pref.FullName) pref.ExtensionType      { return nil }
func (fs *extensionFieldTypes) Range(f func(pref.ExtensionType) bool)        { return }

type unknownFields messageDataType // TODO

func (fs *unknownFields) Len() int                                            { return 0 }
func (fs *unknownFields) Get(n pref.FieldNumber) pref.RawFields               { return nil }
func (fs *unknownFields) Set(n pref.FieldNumber, b pref.RawFields)            { return }
func (fs *unknownFields) Range(f func(pref.FieldNumber, pref.RawFields) bool) { return }
func (fs *unknownFields) IsSupported() bool                                   { return false }
