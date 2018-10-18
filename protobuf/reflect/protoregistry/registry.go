// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package protoregistry provides data structures to register and lookup
// protobuf descriptor types.
package protoregistry

import (
	"sort"
	"strings"

	"github.com/golang/protobuf/v2/internal/errors"
	"github.com/golang/protobuf/v2/reflect/protoreflect"
)

// TODO: Perhaps Register should record the frame of where the function was
// called and surface that in the error? That would help users debug duplicate
// registration issues. This presumes that we provide a way to disable automatic
// registration in generated code.

// TODO: Add a type registry:
/*
var GlobalTypes = new(Types)

type Type interface {
	protoreflect.Descriptor
	GoType() reflect.Type
}
type Types struct {
	Parent   *Types
	Resolver func(url string) (Type, error)
}
func NewTypes(typs ...Type) *Types
func (*Types) Register(typs ...Type) error
func (*Types) FindEnumByName(enum protoreflect.FullName) (protoreflect.EnumType, error)
func (*Types) FindMessageByName(message protoreflect.FullName) (protoreflect.MessageType, error)
func (*Types) FindMessageByURL(url string) (protoreflect.MessageType, error)
func (*Types) FindExtensionByName(field protoreflect.FullName) (protoreflect.ExtensionType, error)
func (*Types) FindExtensionByNumber(message protoreflect.FullName, field protoreflect.FieldNumber) (protoreflect.ExtensionType, error)
func (*Types) RangeEnums(f func(protoreflect.EnumType) bool)
func (*Types) RangeMessages(f func(protoreflect.MessageType) bool)
func (*Types) RangeExtensions(f func(protoreflect.ExtensionType) bool)
func (*Types) RangeExtensionsByMessage(message protoreflect.FullName, f func(protoreflect.ExtensionType) bool)
*/

// GlobalFiles is a global registry of file descriptors.
var GlobalFiles = new(Files)

// NotFound is a sentinel error value to indicate that the type was not found.
var NotFound = errors.New("not found")

// Files is a registry for looking up or iterating over files and the
// descriptors contained within them.
// The Find and Range methods are safe for concurrent use.
type Files struct {
	filesByPackage filesByPackage
	filesByPath    filesByPath
}

type (
	filesByPackage struct {
		// files is a list of files all in the same package.
		files []protoreflect.FileDescriptor
		// subs is a tree of files all in a sub-package scope.
		// It also maps all top-level identifiers declared in files
		// as the notProtoPackage sentinel value.
		subs map[protoreflect.Name]*filesByPackage // invariant: len(Name) > 0
	}
	filesByPath map[string][]protoreflect.FileDescriptor
)

// notProtoPackage is a sentinel value to indicate that some identifier maps
// to an actual protobuf declaration and is not a sub-package.
var notProtoPackage = new(filesByPackage)

// NewFiles returns a registry initialized with the provided set of files.
// If there are duplicates, the first one takes precedence.
func NewFiles(files ...protoreflect.FileDescriptor) *Files {
	// TODO: Should last take precedence? This allows a user to intentionally
	// overwrite an existing registration.
	//
	// The use case is for implementing the existing v1 proto.RegisterFile
	// function where the behavior is last wins. However, it could be argued
	// that the v1 behavior is broken, and we can switch to first wins
	// without violating compatibility.
	r := new(Files)
	r.Register(files...) // ignore errors; first takes precedence
	return r
}

// Register registers the provided list of file descriptors.
// Placeholder files are ignored.
//
// If any descriptor within a file conflicts with the descriptor of any
// previously registered file (e.g., two enums with the same full name),
// then that file is not registered and an error is returned.
//
// It is permitted for multiple files to have the same file path.
func (r *Files) Register(files ...protoreflect.FileDescriptor) error {
	var firstErr error
fileLoop:
	for _, file := range files {
		if file.IsPlaceholder() {
			continue // TODO: Should this be an error instead?
		}

		// Register the file into the filesByPackage tree.
		//
		// The prototype package validates that a FileDescriptor is internally
		// consistent such it does not have conflicts within itself.
		// However, we need to ensure that the inserted file does not conflict
		// with other previously inserted files.
		pkg := file.Package()
		root := &r.filesByPackage
		for len(pkg) > 0 {
			var prefix protoreflect.Name
			prefix, pkg = splitPrefix(pkg)

			// Add a new sub-package segment.
			switch nextRoot := root.subs[prefix]; nextRoot {
			case nil:
				nextRoot = new(filesByPackage)
				if root.subs == nil {
					root.subs = make(map[protoreflect.Name]*filesByPackage)
				}
				root.subs[prefix] = nextRoot
				root = nextRoot
			case notProtoPackage:
				if firstErr == nil {
					name := strings.TrimSuffix(strings.TrimSuffix(string(file.Package()), string(pkg)), ".")
					firstErr = errors.New("file %q has a name conflict over %v", file.Path(), name)
				}
				continue fileLoop
			default:
				root = nextRoot
			}
		}
		// Check for top-level conflicts within the same package.
		// The current file cannot add any top-level declaration that conflict
		// with another top-level declaration or sub-package name.
		var conflicts []protoreflect.Name
		rangeTopLevelDeclarations(file, func(s protoreflect.Name) {
			if root.subs[s] == nil {
				if root.subs == nil {
					root.subs = make(map[protoreflect.Name]*filesByPackage)
				}
				root.subs[s] = notProtoPackage
			} else {
				conflicts = append(conflicts, s)
			}
		})
		if len(conflicts) > 0 {
			// Remove inserted identifiers to make registration failure atomic.
			sort.Slice(conflicts, func(i, j int) bool { return conflicts[i] < conflicts[j] })
			rangeTopLevelDeclarations(file, func(s protoreflect.Name) {
				i := sort.Search(len(conflicts), func(i int) bool { return conflicts[i] >= s })
				if has := i < len(conflicts) && conflicts[i] == s; !has {
					delete(root.subs, s) // remove everything not in conflicts
				}
			})

			if firstErr == nil {
				name := file.Package().Append(conflicts[0])
				firstErr = errors.New("file %q has a name conflict over %v", file.Path(), name)
			}
			continue fileLoop
		}
		root.files = append(root.files, file)

		// Register the file into the filesByPath map.
		//
		// There is no check for conflicts in file path since the path is
		// heavily dependent on how protoc is invoked. When protoc is being
		// invoked by different parties in a distributed manner, it is
		// unreasonable to assume nor ensure that the path is unique.
		if r.filesByPath == nil {
			r.filesByPath = make(filesByPath)
		}
		r.filesByPath[file.Path()] = append(r.filesByPath[file.Path()], file)
	}
	return firstErr
}

// FindDescriptorByName looks up any descriptor (except files) by its full name.
// Files are not handled since multiple file descriptors may belong in
// the same package and have the same full name (see RangeFilesByPackage).
//
// This return (nil, NotFound) if not found.
func (r *Files) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	if r == nil {
		return nil, NotFound
	}
	pkg := name
	root := &r.filesByPackage
	for len(pkg) > 0 {
		var prefix protoreflect.Name
		prefix, pkg = splitPrefix(pkg)
		switch nextRoot := root.subs[prefix]; nextRoot {
		case nil:
			return nil, NotFound
		case notProtoPackage:
			// Search current root's package for the descriptor.
			for _, fd := range root.files {
				if d := fd.DescriptorByName(name); d != nil {
					return d, nil
				}
			}
			return nil, NotFound
		default:
			root = nextRoot
		}
	}
	return nil, NotFound
}

// RangeFiles iterates over all registered files.
// The iteration order is undefined.
func (r *Files) RangeFiles(f func(protoreflect.FileDescriptor) bool) {
	r.RangeFilesByPackage("", f) // empty package is a prefix for all packages
}

// RangeFilesByPackage iterates over all registered files filtered by
// the given proto package prefix. It iterates over files with an exact package
// match before iterating over files with general prefix match.
// The iteration order is undefined within exact matches or prefix matches.
func (r *Files) RangeFilesByPackage(pkg protoreflect.FullName, f func(protoreflect.FileDescriptor) bool) {
	if r == nil {
		return
	}
	if strings.HasSuffix(string(pkg), ".") {
		return // avoid edge case where splitPrefix allows trailing dot
	}
	root := &r.filesByPackage
	for len(pkg) > 0 && root != nil {
		var prefix protoreflect.Name
		prefix, pkg = splitPrefix(pkg)
		root = root.subs[prefix]
	}
	rangeFiles(root, f)
}
func rangeFiles(fs *filesByPackage, f func(protoreflect.FileDescriptor) bool) bool {
	if fs == nil {
		return true
	}
	// Iterate over exact matches.
	for _, fd := range fs.files { // TODO: iterate non-deterministically
		if !f(fd) {
			return false
		}
	}
	// Iterate over prefix matches.
	for _, fs := range fs.subs {
		if !rangeFiles(fs, f) {
			return false
		}
	}
	return true
}

// RangeFilesByPath iterates over all registered files filtered by
// the given proto path. The iteration order is undefined.
func (r *Files) RangeFilesByPath(path string, f func(protoreflect.FileDescriptor) bool) {
	if r == nil {
		return
	}
	for _, fd := range r.filesByPath[path] { // TODO: iterate non-deterministically
		if !f(fd) {
			return
		}
	}
}

func splitPrefix(name protoreflect.FullName) (protoreflect.Name, protoreflect.FullName) {
	if i := strings.IndexByte(string(name), '.'); i >= 0 {
		return protoreflect.Name(name[:i]), name[i+len("."):]
	}
	return protoreflect.Name(name), ""
}

// rangeTopLevelDeclarations iterates over the name of all top-level
// declarations in the proto file.
func rangeTopLevelDeclarations(fd protoreflect.FileDescriptor, f func(protoreflect.Name)) {
	for i := 0; i < fd.Enums().Len(); i++ {
		e := fd.Enums().Get(i)
		f(e.Name())
		for i := 0; i < e.Values().Len(); i++ {
			f(e.Values().Get(i).Name())
		}
	}
	for i := 0; i < fd.Messages().Len(); i++ {
		f(fd.Messages().Get(i).Name())
	}
	for i := 0; i < fd.Extensions().Len(); i++ {
		f(fd.Extensions().Get(i).Name())
	}
	for i := 0; i < fd.Services().Len(); i++ {
		f(fd.Services().Get(i).Name())
	}
}
